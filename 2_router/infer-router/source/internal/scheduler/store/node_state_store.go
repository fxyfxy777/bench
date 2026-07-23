package store

import (
	"maps"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/yzx/rl-router/internal/domain"
)

// NodeStateStore maintains the global load state for all backend instances.
// Uses Copy-on-Write (COW) pattern: reads are lock-free via atomic.Pointer,
// writes create new maps under a mutex. The primary index is by instance ID;
// the secondary index groups the same NodeState pointers by resource_group so
// hot-path scheduling can resolve candidates without scanning all instances.
type NodeStateStore struct {
	nodesPtr   atomic.Pointer[map[string]*domain.NodeState]
	groupsPtr  atomic.Pointer[map[string]map[string]*domain.NodeState]
	generation atomic.Uint64 // incremented on every write (Register/Unregister/Sync)
	mu         sync.Mutex    // protects write operations only (COW)
}

// ResourceGroupConflict reports an instance ID already owned by another group.
type ResourceGroupConflict struct {
	ID            string
	ExistingGroup string
}

func NewNodeStateStore() *NodeStateStore {
	sm := &NodeStateStore{}
	empty := make(map[string]*domain.NodeState)
	emptyGroups := make(map[string]map[string]*domain.NodeState)
	sm.nodesPtr.Store(&empty)
	sm.groupsPtr.Store(&emptyGroups)
	return sm
}

// loadNodes returns the current nodes map. Lock-free.
func (sm *NodeStateStore) loadNodes() map[string]*domain.NodeState {
	return *sm.nodesPtr.Load()
}

func (sm *NodeStateStore) loadGroups() map[string]map[string]*domain.NodeState {
	return *sm.groupsPtr.Load()
}

// storeAndBump atomically stores a new node map, rebuilds secondary indexes,
// and increments the generation counter. Caller must hold sm.mu.
func (sm *NodeStateStore) storeAndBump(m *map[string]*domain.NodeState) {
	groups := buildResourceGroupIndex(*m)
	sm.storeMapsAndBump(m, &groups)
}

func (sm *NodeStateStore) storeMapsAndBump(nodes *map[string]*domain.NodeState, groups *map[string]map[string]*domain.NodeState) {
	sm.nodesPtr.Store(nodes)
	sm.groupsPtr.Store(groups)
	sm.generation.Add(1)
}

func buildResourceGroupIndex(nodes map[string]*domain.NodeState) map[string]map[string]*domain.NodeState {
	groups := make(map[string]map[string]*domain.NodeState)
	for id, ns := range nodes {
		if ns == nil || ns.Instance == nil {
			continue
		}
		group := domain.NormalizeResourceGroup(ns.Instance.ResourceGroup)
		bucket := groups[group]
		if bucket == nil {
			bucket = make(map[string]*domain.NodeState)
			groups[group] = bucket
		}
		bucket[id] = ns
	}
	return groups
}

// Generation returns the current generation counter. Lock-free.
func (sm *NodeStateStore) Generation() uint64 {
	return sm.generation.Load()
}

// cloneMap creates a shallow copy of the current nodes map for COW mutation.
// Caller must hold sm.mu.
func (sm *NodeStateStore) cloneMap() map[string]*domain.NodeState {
	old := sm.loadNodes()
	newMap := make(map[string]*domain.NodeState, len(old)+1)
	maps.Copy(newMap, old)
	return newMap
}

func normalizeInstanceResourceGroup(inst *domain.Instance, group string) *domain.Instance {
	if inst == nil {
		return nil
	}
	if group == "" {
		group = inst.ResourceGroup
	}
	copyInst := *inst
	copyInst.ResourceGroup = domain.NormalizeResourceGroup(group)
	copyInst.Labels = maps.Clone(inst.Labels)
	return &copyInst
}

func copyGroupBucket(groups map[string]map[string]*domain.NodeState, group string) map[string]*domain.NodeState {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	old := groups[group]
	out := make(map[string]*domain.NodeState, len(old)+1)
	maps.Copy(out, old)
	return out
}

func cloneGroupsForUpdate(old map[string]map[string]*domain.NodeState, touched map[string]struct{}) map[string]map[string]*domain.NodeState {
	next := make(map[string]map[string]*domain.NodeState, len(old)+len(touched))
	maps.Copy(next, old)
	for group := range touched {
		next[group] = copyGroupBucket(old, group)
	}
	return next
}

func nodeResourceGroup(ns *domain.NodeState) string {
	if ns == nil || ns.Instance == nil {
		return domain.DefaultResourceGroup
	}
	return domain.NormalizeResourceGroup(ns.Instance.ResourceGroup)
}

// FindResourceGroupConflict returns the first instance whose ID already belongs
// to a different resource group. It is lock-free and safe for preflight checks.
func (sm *NodeStateStore) FindResourceGroupConflict(group string, instances []*domain.Instance) (ResourceGroupConflict, bool) {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	nodes := sm.loadNodes()
	for _, inst := range instances {
		if inst == nil || inst.ID == "" {
			continue
		}
		if ns := nodes[inst.ID]; ns != nil && ns.Instance != nil {
			existingGroup := domain.NormalizeResourceGroup(ns.Instance.ResourceGroup)
			if existingGroup != group {
				return ResourceGroupConflict{ID: inst.ID, ExistingGroup: existingGroup}, true
			}
		}
	}
	return ResourceGroupConflict{}, false
}

// Register adds or replaces an instance in the state tree (upsert, idempotent).
func (sm *NodeStateStore) Register(inst *domain.Instance) {
	sm.RegisterBatch([]*domain.Instance{inst})
}

// RegisterBatch adds or replaces multiple instances atomically (upsert, idempotent).
// Single lock acquisition for the entire batch.
func (sm *NodeStateStore) RegisterBatch(instances []*domain.Instance) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	old := sm.loadNodes()
	newMap := sm.cloneMap()
	touched := make(map[string]struct{}, len(instances))
	normalized := make([]*domain.Instance, 0, len(instances))
	for _, inst := range instances {
		inst = normalizeInstanceResourceGroup(inst, "")
		if inst == nil || inst.ID == "" {
			continue
		}
		if oldNS := old[inst.ID]; oldNS != nil {
			touched[nodeResourceGroup(oldNS)] = struct{}{}
		}
		touched[domain.NormalizeResourceGroup(inst.ResourceGroup)] = struct{}{}
		normalized = append(normalized, inst)
	}
	if len(normalized) == 0 {
		return
	}
	newGroups := cloneGroupsForUpdate(sm.loadGroups(), touched)
	for _, inst := range normalized {
		if oldNS := old[inst.ID]; oldNS != nil {
			oldGroup := nodeResourceGroup(oldNS)
			delete(newGroups[oldGroup], inst.ID)
			if len(newGroups[oldGroup]) == 0 {
				delete(newGroups, oldGroup)
			}
		}
		ns := &domain.NodeState{Instance: inst, Healthy: 1}
		newMap[inst.ID] = ns
		group := domain.NormalizeResourceGroup(inst.ResourceGroup)
		if newGroups[group] == nil {
			newGroups[group] = make(map[string]*domain.NodeState)
		}
		newGroups[group][inst.ID] = ns
	}
	sm.storeMapsAndBump(&newMap, &newGroups)
}

// RegisterBatchForResourceGroup upserts instances into one resource group.
// The scoped resource group owner wins over empty per-instance ResourceGroup.
func (sm *NodeStateStore) RegisterBatchForResourceGroup(group string, instances []*domain.Instance) {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	normalized := make([]*domain.Instance, 0, len(instances))
	for _, inst := range instances {
		if inst != nil {
			normalized = append(normalized, normalizeInstanceResourceGroup(inst, group))
		}
	}
	sm.RegisterBatch(normalized)
}

// Unregister removes an instance from the state tree (idempotent).
// Returns true if the instance was found and removed, false if it was already absent.
func (sm *NodeStateStore) Unregister(instanceID string) bool {
	return sm.UnregisterBatch([]string{instanceID}) == 1
}

// UnregisterBatch removes multiple instances atomically (idempotent).
// Returns the count of instances that were actually found and removed.
func (sm *NodeStateStore) UnregisterBatch(ids []string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	old := sm.loadNodes()
	toRemove := make(map[string]struct{}, len(ids))
	touched := make(map[string]struct{}, len(ids))
	removed := 0
	for _, id := range ids {
		if ns, ok := old[id]; ok {
			if _, seen := toRemove[id]; seen {
				continue
			}
			toRemove[id] = struct{}{}
			touched[nodeResourceGroup(ns)] = struct{}{}
			removed++
		}
	}
	if removed == 0 {
		return 0
	}
	newMap := make(map[string]*domain.NodeState, len(old)-removed)
	for k, v := range old {
		if _, skip := toRemove[k]; !skip {
			newMap[k] = v
		}
	}
	newGroups := cloneGroupsForUpdate(sm.loadGroups(), touched)
	for id := range toRemove {
		group := nodeResourceGroup(old[id])
		delete(newGroups[group], id)
		if len(newGroups[group]) == 0 {
			delete(newGroups, group)
		}
	}
	sm.storeMapsAndBump(&newMap, &newGroups)
	return removed
}

// UnregisterResourceGroup removes IDs only when they currently belong to group.
func (sm *NodeStateStore) UnregisterResourceGroup(group string, ids []string) int {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	old := sm.loadNodes()
	toRemove := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if ns, ok := old[id]; ok && ns != nil && ns.Instance != nil && domain.NormalizeResourceGroup(ns.Instance.ResourceGroup) == group {
			toRemove[id] = struct{}{}
		}
	}
	if len(toRemove) == 0 {
		return 0
	}
	newMap := make(map[string]*domain.NodeState, len(old)-len(toRemove))
	for id, ns := range old {
		if _, skip := toRemove[id]; !skip {
			newMap[id] = ns
		}
	}
	sm.storeAndBump(&newMap)
	return len(toRemove)
}

// SyncResult holds the diff produced by a Sync operation.
type SyncResult struct {
	Added   int
	Removed int
	Updated int
}

// Sync atomically replaces the entire instance set with the given list.
func (sm *NodeStateStore) Sync(instances []*domain.Instance) SyncResult {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	old := sm.loadNodes()

	incoming := make(map[string]*domain.Instance, len(instances))
	for _, inst := range instances {
		inst = normalizeInstanceResourceGroup(inst, "")
		if inst != nil && inst.ID != "" {
			incoming[inst.ID] = inst
		}
	}

	var result SyncResult
	for id := range old {
		if _, ok := incoming[id]; !ok {
			result.Removed++
		}
	}

	newMap := make(map[string]*domain.NodeState, len(incoming))
	for id, inst := range incoming {
		if _, exists := old[id]; exists {
			result.Updated++
		} else {
			result.Added++
		}
		newMap[id] = &domain.NodeState{Instance: inst, Healthy: 1}
	}

	sm.storeAndBump(&newMap)
	return result
}

// SyncResourceGroup atomically replaces one resource group's instance set.
func (sm *NodeStateStore) SyncResourceGroup(group string, instances []*domain.Instance) SyncResult {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	old := sm.loadNodes()

	incoming := make(map[string]*domain.Instance, len(instances))
	for _, inst := range instances {
		inst = normalizeInstanceResourceGroup(inst, group)
		if inst != nil && inst.ID != "" {
			incoming[inst.ID] = inst
		}
	}

	var result SyncResult
	newMap := make(map[string]*domain.NodeState, len(old)+len(incoming))
	for id, ns := range old {
		if ns == nil || ns.Instance == nil {
			continue
		}
		if domain.NormalizeResourceGroup(ns.Instance.ResourceGroup) == group {
			if _, keep := incoming[id]; !keep {
				result.Removed++
			}
			continue
		}
		newMap[id] = ns
	}
	for id, inst := range incoming {
		if _, exists := old[id]; exists {
			result.Updated++
		} else {
			result.Added++
		}
		newMap[id] = &domain.NodeState{Instance: inst, Healthy: 1}
	}

	sm.storeAndBump(&newMap)
	return result
}

// List returns all registered instances.
func (sm *NodeStateStore) List() []*domain.Instance {
	nodes := sm.loadNodes()
	instances := make([]*domain.Instance, 0, len(nodes))
	for _, ns := range nodes {
		instances = append(instances, ns.Instance)
	}
	return instances
}

// ListByResourceGroup returns all registered instances for group.
func (sm *NodeStateStore) ListByResourceGroup(group string) []*domain.Instance {
	nodes := sm.GetNodesByResourceGroup(group)
	instances := make([]*domain.Instance, 0, len(nodes))
	for _, ns := range nodes {
		instances = append(instances, ns.Instance)
	}
	return instances
}

// ResourceGroups returns all known resource group names in stable order.
func (sm *NodeStateStore) ResourceGroups() []string {
	groups := sm.loadGroups()
	out := make([]string, 0, len(groups))
	for group := range groups {
		out = append(out, group)
	}
	sort.Strings(out)
	return out
}

// GetSnapshot returns a shallow copy of the current node map (each NodeState is cloned).
func (sm *NodeStateStore) GetSnapshot() map[string]*domain.NodeState {
	nodes := sm.loadNodes()
	snap := make(map[string]*domain.NodeState, len(nodes))
	for id, ns := range nodes {
		snap[id] = ns.Clone()
	}
	return snap
}

// Acquire increments the active request counter for the given instance.
func (sm *NodeStateStore) Acquire(instanceID string) {
	nodes := sm.loadNodes()
	ns, ok := nodes[instanceID]
	if !ok {
		return
	}
	ns.AddActiveRequests(1)
}

// Release decrements the active request counter for the given instance.
func (sm *NodeStateStore) Release(instanceID string) {
	nodes := sm.loadNodes()
	ns, ok := nodes[instanceID]
	if !ok {
		return
	}
	if ns.LoadActiveRequests() > 0 {
		ns.AddActiveRequests(-1)
	}
}

// GetNodes returns the current nodes map directly.
func (sm *NodeStateStore) GetNodes() map[string]*domain.NodeState {
	return sm.loadNodes()
}

// GetNodesByResourceGroup returns the COW candidate map for a resource group.
func (sm *NodeStateStore) GetNodesByResourceGroup(group string) map[string]*domain.NodeState {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	groups := sm.loadGroups()
	if nodes, ok := groups[group]; ok {
		return nodes
	}
	return nil
}

// ResetAll resets all node load counters to zero.
func (sm *NodeStateStore) ResetAll() {
	nodes := sm.loadNodes()
	for _, ns := range nodes {
		resetNodeState(ns)
	}
}

// ResetResourceGroup resets node load counters for one resource group only.
func (sm *NodeStateStore) ResetResourceGroup(group string) {
	nodes := sm.GetNodesByResourceGroup(group)
	for _, ns := range nodes {
		resetNodeState(ns)
	}
}

func resetNodeState(ns *domain.NodeState) {
	if ns == nil {
		return
	}
	ns.StoreActiveRequests(0)
	ns.StoreLockedMemory(0)
	ns.StoreActualLoad(0)
	ns.StoreHealthy(true)
	ns.StoreCircuitOpen(false)
}

// GetNodesByRole returns nodes whose Instance.Role matches the given role.
func (sm *NodeStateStore) GetNodesByRole(role string) map[string]*domain.NodeState {
	nodes := sm.loadNodes()
	result := make(map[string]*domain.NodeState)
	for id, ns := range nodes {
		if ns.Instance != nil && ns.Instance.Role == role {
			result[id] = ns
		}
	}
	return result
}

// GetNodesByResourceGroupRole returns nodes for group whose Instance.Role matches role.
func (sm *NodeStateStore) GetNodesByResourceGroupRole(group, role string) map[string]*domain.NodeState {
	nodes := sm.GetNodesByResourceGroup(group)
	result := make(map[string]*domain.NodeState)
	for id, ns := range nodes {
		if ns.Instance != nil && ns.Instance.Role == role {
			result[id] = ns
		}
	}
	return result
}

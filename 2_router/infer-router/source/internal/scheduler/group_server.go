package scheduler

import (
	"fmt"
	"sort"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
)

func (s *Server) defaultGroup() *groupRuntime {
	return s.getOrCreateGroup(domain.DefaultResourceGroup)
}

func (s *Server) getGroup(resourceGroup string) *groupRuntime {
	if resourceGroup == "" {
		resourceGroup = domain.DefaultResourceGroup
	}
	return s.groups[resourceGroup]
}

func (s *Server) getOrCreateGroup(resourceGroup string) *groupRuntime {
	if resourceGroup == "" {
		resourceGroup = domain.DefaultResourceGroup
	}
	if s.groups == nil {
		s.groups = make(map[string]*groupRuntime)
	}
	if g := s.groups[resourceGroup]; g != nil {
		return g
	}
	p := s.policy
	name := policy.DefaultName
	if v := s.policyName.Load(); v != nil {
		if stored := policy.Name(v.(string)); stored != "" {
			name = stored
		}
	} else if p != nil {
		name = p.Name()
	}
	if resourceGroup != domain.DefaultResourceGroup || p == nil {
		built, err := policy.Build(name, policy.PolicyConfig{})
		if err == nil {
			p = built
		}
	}
	if p == nil {
		p, _ = policy.Build(policy.DefaultName, policy.PolicyConfig{})
	}
	g := newGroupRuntime(resourceGroup, p, s.defaultQueueCfg)
	if resourceGroup == domain.DefaultResourceGroup {
		g.phase = domain.StepPhase(s.stepPhase.Load())
		g.stepID = s.stepID.Load()
		g.stepDoneCh = s.stepDoneCh
		g.lastEndedStepID = s.lastEndedStepID
		g.paused = s.paused
		g.queueEnabled = s.queueEnabled
		g.queueMaxSize = s.queueMaxSize
		g.queueTimeout = s.queueTimeout
		g.waitingQueue = s.waitingQueue
		g.allocDedup = s.allocDedup
		g.releaseDedup = s.releaseDedup
		g.activeCount = s.activeCount
		g.allocCounter = s.allocCounter
		g.pdPrefillPolicy = s.pdPrefillPolicy
		g.pdDecodePolicy = s.pdDecodePolicy
		g.pdActiveCount = s.pdActiveCount
		g.pdAllocDedup = s.pdAllocDedup
		g.pdReleaseDedup = s.pdReleaseDedup
		g.pdWaitingQueue = s.pdWaitingQueue
		g.currentPolicyConfig = s.currentPolicyConfig
	}
	s.groups[resourceGroup] = g
	return g
}

func (s *Server) hasResourceGroupRuntimeOrInstances(resourceGroup string) bool {
	if resourceGroup == "" {
		resourceGroup = domain.DefaultResourceGroup
	}
	if resourceGroup == domain.DefaultResourceGroup {
		return true
	}
	if s.getGroup(resourceGroup) != nil {
		return true
	}
	return len(s.state.GetNodesByResourceGroup(resourceGroup)) > 0
}

func (s *Server) resolveGroupPolicy(g *groupRuntime, stepID int64, policyName string, cfg policy.PolicyConfig) error {
	requested := policy.Name(policyName)
	if requested == "" {
		requested = g.policyName
	}
	if requested == "" {
		requested = policy.DefaultName
	}

	if policyName == "" && g.policy != nil {
		if r, ok := g.policy.(policy.Resettable); ok {
			r.Reset()
		}
		g.policyConfig = cfg
		return nil
	}

	newPolicy, err := policy.Build(requested, cfg)
	if err != nil {
		return fmt.Errorf("scheduler: cannot start resource_group %q step %d, invalid policy %q: %w",
			g.resourceGroup, stepID, requested, err)
	}
	g.policy = newPolicy
	g.policyName = newPolicy.Name()
	g.policyConfig = cfg
	g.bindPolicyInterfaces()
	return nil
}

func (s *Server) prepareGroupPolicyForNodes(g *groupRuntime, nodes map[string]*domain.NodeState) {
	if g == nil {
		return
	}
	if g.generationAware != nil {
		g.generationAware.SetGeneration(s.state.Generation())
	}
	if g.cachedIDEnsurer != nil {
		g.cachedIDEnsurer.EnsureCachedIDs(nodes)
	}
}

func (s *Server) syncDefaultGroupMirrors() {
	g := s.getOrCreateGroup(domain.DefaultResourceGroup)
	s.policy = g.policy
	s.policyName.Store(string(g.policyName))
	s.currentPolicyConfig = g.currentPolicyConfig
	s.batchSelector = g.batchSelector
	s.generationAware = g.generationAware
	s.driftTracker = g.driftTracker
	s.cachedIDEnsurer = g.cachedIDEnsurer
	s.pdPrefillPolicy = g.pdPrefillPolicy
	s.pdDecodePolicy = g.pdDecodePolicy
	s.queueEnabled = g.queueEnabled
	s.queueMaxSize = g.queueMaxSize
	s.queueTimeout = g.queueTimeout
	s.waitingQueue = g.waitingQueue
	s.pdWaitingQueue = g.pdWaitingQueue
	s.paused = g.paused
	s.pausedAtomic.Store(g.paused)
	s.allocCounter = g.allocCounter
	s.allocCounterAtomic.Store(g.allocCounter)
	s.stepPhase.Store(int32(g.phase))
	s.stepID.Store(g.stepID)
	s.stepDoneCh = g.stepDoneCh
	s.lastEndedStepID = g.lastEndedStepID
	s.drainCyclesWithoutRelease = g.drainCyclesWithoutRelease
	s.lastDrainHadCapacityFree = g.lastDrainHadCapacityFree
	s.refreshAggregateMirrors()
}

func (s *Server) refreshAggregateMirrors() {
	var normal, pd, queueDepth, pdQueueDepth int64
	for _, g := range s.groups {
		normal += g.activeCount
		pd += g.pdActiveCount
		queueDepth += int64(len(g.waitingQueue))
		pdQueueDepth += int64(len(g.pdWaitingQueue))
	}
	s.activeCount = normal
	s.pdActiveCount = pd
	s.activeCountAtomic.Store(normal)
	s.pdActiveAtomic.Store(pd)
	s.queueDepthAtomic.Store(queueDepth)
	s.pdQueueDepthAtomic.Store(pdQueueDepth)
}

func (s *Server) groupQueueEnabledAny() bool {
	for _, g := range s.groups {
		if g.queueEnabled && (len(g.waitingQueue) > 0 || len(g.pdWaitingQueue) > 0) {
			return true
		}
	}
	return false
}

func (s *Server) drainWaitingQueuesForGroups() {
	if len(s.groups) == 0 {
		s.getOrCreateGroup(domain.DefaultResourceGroup)
	}
	for _, g := range s.groups {
		if !g.queueEnabled || (len(g.waitingQueue) == 0 && len(g.pdWaitingQueue) == 0) {
			continue
		}
		s.withGroup(g, func() {
			if s.lastDrainedNormal {
				if len(s.pdWaitingQueue) > 0 {
					s.drainPDWaitingQueue()
				}
				if len(s.waitingQueue) > 0 {
					s.drainWaitingQueue()
				}
			} else {
				if len(s.waitingQueue) > 0 {
					s.drainWaitingQueue()
				}
				if len(s.pdWaitingQueue) > 0 {
					s.drainPDWaitingQueue()
				}
			}
		})
	}
	s.lastDrainedNormal = !s.lastDrainedNormal
}

func (s *Server) currentResourceGroup() string {
	if s.activeGroup != nil && s.activeGroup.resourceGroup != "" {
		return s.activeGroup.resourceGroup
	}
	return domain.DefaultResourceGroup
}

func (s *Server) currentGroupActiveCount() int64 {
	if s.activeGroup != nil {
		return s.activeCount + s.pdActiveCount
	}
	return s.totalActiveCount()
}

func (s *Server) currentNodes() map[string]*domain.NodeState {
	if s.activeGroup == nil {
		return s.state.GetNodes()
	}
	return s.state.GetNodesByResourceGroup(s.activeGroup.resourceGroup)
}

func (s *Server) currentNodesByRole(role string) map[string]*domain.NodeState {
	if s.activeGroup == nil {
		return s.state.GetNodesByRole(role)
	}
	return s.state.GetNodesByResourceGroupRole(s.activeGroup.resourceGroup, role)
}

func (s *Server) currentInstances() []*domain.Instance {
	if s.activeGroup == nil {
		return s.state.List()
	}
	return s.state.ListByResourceGroup(s.activeGroup.resourceGroup)
}

func (s *Server) resourceGroupForInstance(instanceID string) string {
	if ns, ok := s.state.GetNodes()[instanceID]; ok && ns != nil && ns.Instance != nil {
		return domain.NormalizeResourceGroup(ns.Instance.ResourceGroup)
	}
	return domain.DefaultResourceGroup
}

func (s *Server) resourceGroupForRelease(ev *event) string {
	if ev != nil && ev.gatewayAddr != "" && ev.allocationID != "" {
		if group := s.resourceGroupForGatewayAllocation(ev.gatewayAddr, ev.allocationID); group != "" {
			return group
		}
	}
	if ev != nil {
		return s.resourceGroupForInstance(ev.instanceID)
	}
	return domain.DefaultResourceGroup
}

func (s *Server) resourceGroupForGatewayAllocation(gatewayAddr, allocationID string) string {
	if gatewayAddr == "" || allocationID == "" {
		return domain.DefaultResourceGroup
	}
	if allocs := s.gatewayAllocs[gatewayAddr]; len(allocs) > 0 {
		if alloc, ok := allocs[allocationID]; ok {
			if alloc.resourceGroup != "" {
				return alloc.resourceGroup
			}
			return s.resourceGroupForInstance(alloc.instanceID)
		}
	}
	return domain.DefaultResourceGroup
}

func (s *Server) loadGroupState(g *groupRuntime) {
	s.activeGroup = g
	s.policy = g.policy
	s.policyName.Store(string(g.policyName))
	s.currentPolicyConfig = g.currentPolicyConfig
	s.batchSelector = g.batchSelector
	s.generationAware = g.generationAware
	s.driftTracker = g.driftTracker
	s.cachedIDEnsurer = g.cachedIDEnsurer
	s.stepPhase.Store(int32(g.phase))
	s.stepID.Store(g.stepID)
	s.paused = g.paused
	s.stepDoneCh = g.stepDoneCh
	s.lastEndedStepID = g.lastEndedStepID
	s.queueEnabled = g.queueEnabled
	s.queueMaxSize = g.queueMaxSize
	s.queueTimeout = g.queueTimeout
	s.waitingQueue = g.waitingQueue
	s.allocDedup = g.allocDedup
	s.releaseDedup = g.releaseDedup
	s.activeCount = g.activeCount
	s.allocCounter = g.allocCounter
	s.allocCounterAtomic.Store(g.allocCounter)
	s.pdPrefillPolicy = g.pdPrefillPolicy
	s.pdDecodePolicy = g.pdDecodePolicy
	s.pdActiveCount = g.pdActiveCount
	s.pdAllocDedup = g.pdAllocDedup
	s.pdReleaseDedup = g.pdReleaseDedup
	s.pdWaitingQueue = g.pdWaitingQueue
	s.drainCyclesWithoutRelease = g.drainCyclesWithoutRelease
	s.lastDrainHadCapacityFree = g.lastDrainHadCapacityFree
}

func (s *Server) storeGroupState(g *groupRuntime) {
	if s.activeGroup != g {
		if s.logger != nil {
			activeGroup := "<nil>"
			if s.activeGroup != nil {
				activeGroup = s.activeGroup.resourceGroup
			}
			targetGroup := "<nil>"
			if g != nil {
				targetGroup = g.resourceGroup
			}
			s.logger.Error("group state store skipped: active group mismatch",
				zap.String("active_resource_group", activeGroup),
				zap.String("target_resource_group", targetGroup))
		}
		return
	}
	g.policy = s.policy
	g.policyName = policy.Name(s.policyName.Load().(string))
	g.currentPolicyConfig = s.currentPolicyConfig
	g.batchSelector = s.batchSelector
	g.generationAware = s.generationAware
	g.driftTracker = s.driftTracker
	g.cachedIDEnsurer = s.cachedIDEnsurer
	g.phase = domain.StepPhase(s.stepPhase.Load())
	g.stepID = s.stepID.Load()
	g.stepDoneCh = s.stepDoneCh
	g.lastEndedStepID = s.lastEndedStepID
	g.paused = s.paused
	g.queueEnabled = s.queueEnabled
	g.queueMaxSize = s.queueMaxSize
	g.queueTimeout = s.queueTimeout
	g.waitingQueue = s.waitingQueue
	g.allocDedup = s.allocDedup
	g.releaseDedup = s.releaseDedup
	g.activeCount = s.activeCount
	g.allocCounter = s.allocCounter
	g.pdPrefillPolicy = s.pdPrefillPolicy
	g.pdDecodePolicy = s.pdDecodePolicy
	g.pdActiveCount = s.pdActiveCount
	g.pdAllocDedup = s.pdAllocDedup
	g.pdReleaseDedup = s.pdReleaseDedup
	g.pdWaitingQueue = s.pdWaitingQueue
	g.drainCyclesWithoutRelease = s.drainCyclesWithoutRelease
	g.lastDrainHadCapacityFree = s.lastDrainHadCapacityFree
}

func (s *Server) syncDefaultGroupPolicyOverrides(g *groupRuntime) {
	if g == nil || g.resourceGroup != domain.DefaultResourceGroup {
		return
	}
	// Package-internal tests may swap policy fields directly on Server between
	// event-loop calls. Keep that compatibility without copying aggregate
	// counters or per-step state back into the default group runtime.
	if s.policy != nil {
		g.policy = s.policy
		g.policyName = s.policy.Name()
		g.bindPolicyInterfaces()
	}
	g.currentPolicyConfig = s.currentPolicyConfig
	g.pdPrefillPolicy = s.pdPrefillPolicy
	g.pdDecodePolicy = s.pdDecodePolicy
}

func (s *Server) withGroup(g *groupRuntime, fn func()) {
	if g == nil {
		g = s.defaultGroup()
	}
	if s.activeGroup == nil {
		s.syncDefaultGroupPolicyOverrides(g)
	}
	s.loadGroupState(g)
	fn()
	s.storeGroupState(g)
	s.activeGroup = nil
	s.refreshAggregateMirrors()
	s.syncDefaultGroupMirrors()
}

func (s *Server) handleGetResourceGroupState(ev *event) {
	group := ev.resourceGroup
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	g := s.getGroup(group)
	if ev.stepStateResult != nil {
		ev.stepStateResult <- stepStateForResourceGroup(group, g)
	}
}

func (s *Server) handleGetResourceGroupDrainWait(ev *event) {
	group := ev.resourceGroup
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	g := s.getGroup(group)
	state := stepStateForResourceGroup(group, g)
	var doneCh <-chan struct{}
	if g != nil && g.phase == domain.StepDraining {
		doneCh = g.stepDoneCh
	}
	if ev.stepWaitResult != nil {
		ev.stepWaitResult <- stepWaitResult{state: state, doneCh: doneCh}
	}
}

func (s *Server) handleListResourceGroups(ev *event) {
	if ev.resourceGroupsResult == nil {
		return
	}
	ev.resourceGroupsResult <- s.knownResourceGroups()
}

func (s *Server) handleListResourceGroupStates(ev *event) {
	if ev.resourceGroupStatesResult == nil {
		return
	}
	groups := s.knownResourceGroups()
	states := make([]domain.StepState, 0, len(groups))
	for _, group := range groups {
		states = append(states, stepStateForResourceGroup(group, s.getGroup(group)))
	}
	ev.resourceGroupStatesResult <- states
}

func (s *Server) knownResourceGroups() []string {
	seen := map[string]struct{}{domain.DefaultResourceGroup: {}}
	for _, group := range s.state.ResourceGroups() {
		seen[group] = struct{}{}
	}
	for group := range s.groups {
		if group == "" {
			group = domain.DefaultResourceGroup
		}
		seen[group] = struct{}{}
	}
	groups := make([]string, 0, len(seen))
	for group := range seen {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	return groups
}

func stepStateForResourceGroup(group string, g *groupRuntime) domain.StepState {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	if g == nil {
		return domain.StepState{
			ResourceGroup: group,
			Phase:         domain.StepIdle,
		}
	}
	return domain.StepState{
		ResourceGroup: group,
		Phase:         g.phase,
		StepID:        g.stepID,
		Paused:        g.paused,
	}
}

func (s *Server) pruneEmptyResourceGroups() {
	for group, g := range s.groups {
		if group == "" || group == domain.DefaultResourceGroup {
			continue
		}
		if !canPruneResourceGroup(g) {
			continue
		}
		if len(s.state.GetNodesByResourceGroup(group)) > 0 {
			continue
		}
		delete(s.groups, group)
		s.logger.Info("resource group pruned",
			zap.String("resource_group", group))
	}
}

func canPruneResourceGroup(g *groupRuntime) bool {
	if g == nil {
		return true
	}
	return g.phase == domain.StepIdle &&
		!g.paused &&
		g.activeCount == 0 &&
		g.pdActiveCount == 0 &&
		len(g.waitingQueue) == 0 &&
		len(g.pdWaitingQueue) == 0
}

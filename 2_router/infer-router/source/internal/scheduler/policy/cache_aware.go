package policy

import (
	"context"
	"math"
	"sync"

	"github.com/yzx/rl-router/internal/domain"
)

// CacheAwarePolicy implements radix-tree-based prefix cache routing,
// inspired by sglang's cache-aware algorithm.
//
// Two-mode routing:
//   - Imbalanced mode (maxLoad-minLoad > BalanceAbsThreshold AND
//     maxLoad > minLoad*BalanceRelThreshold): shortest-queue routing
//   - Balanced mode: prefix match via radix tree; if matchRate > CacheThreshold,
//     route to the best-matching instance; otherwise route to the instance with
//     the fewest tracked tree bytes (most available cache capacity)
//
// Implements: Policy, BatchSelector, Resettable, SessionRemover.
type CacheAwarePolicy struct {
	mu          sync.Mutex
	tree        *RadixTree
	requestLoad map[string]int64 // instance_id → in-flight request count

	// Configuration (with defaults).
	CacheThreshold      float64 // prefix match rate threshold (default 0.5)
	BalanceAbsThreshold int64   // absolute load diff for imbalance (default 32)
	BalanceRelThreshold float64 // relative load ratio for imbalance (default 1.1)
	MaxTreeSize         int64   // max tree bytes per tenant before eviction (default 10000)
	EvictionIntervalSec int64   // seconds between inline eviction sweeps (default 60)
	MaxRequestLoad      int64   // per-instance request load cap (default 100)

	lastEvictionEpoch uint64 // epoch at last eviction

	// Pre-resolved branch counters for observability.
	branches *CacheAwareBranches

	// Reusable buffers — only accessed from event-loop (single-threaded).
	resultsBuf []BatchSelectResult
	heapBuf    []cacheNodeEntry
}

// CacheAwarePolicyConfig holds configuration for NewCacheAwarePolicy.
type CacheAwarePolicyConfig struct {
	CacheThreshold      float64
	BalanceAbsThreshold int64
	BalanceRelThreshold float64
	MaxTreeSize         int64
	EvictionIntervalSec int64
	MaxRequestLoad      int64
	CacheBlockSize      int
	HitRatioWeight      float64
	LoadBalanceWeight   float64
}

// NewCacheAwarePolicy creates a CacheAwarePolicy with the given config.
// Zero-valued fields use sglang-compatible defaults.
func NewCacheAwarePolicy(cfg CacheAwarePolicyConfig) *CacheAwarePolicy {
	if cfg.CacheThreshold <= 0 {
		cfg.CacheThreshold = 0.5
	}
	if cfg.BalanceAbsThreshold <= 0 {
		cfg.BalanceAbsThreshold = 32
	}
	if cfg.BalanceRelThreshold <= 0 {
		cfg.BalanceRelThreshold = 1.1
	}
	if cfg.MaxTreeSize <= 0 {
		cfg.MaxTreeSize = 10000
	}
	if cfg.EvictionIntervalSec <= 0 {
		cfg.EvictionIntervalSec = 60
	}
	if cfg.MaxRequestLoad <= 0 {
		cfg.MaxRequestLoad = 100
	}
	return &CacheAwarePolicy{
		tree:                NewRadixTree(),
		requestLoad:         make(map[string]int64, 16),
		CacheThreshold:      cfg.CacheThreshold,
		BalanceAbsThreshold: cfg.BalanceAbsThreshold,
		BalanceRelThreshold: cfg.BalanceRelThreshold,
		MaxTreeSize:         cfg.MaxTreeSize,
		EvictionIntervalSec: cfg.EvictionIntervalSec,
		MaxRequestLoad:      cfg.MaxRequestLoad,
		branches:            newCacheAwareBranches(),
	}
}

func (p *CacheAwarePolicy) Name() Name { return NameCacheAware }

// ConfigSummary implements ConfigDescriber.
func (p *CacheAwarePolicy) ConfigSummary() ConfigInfo {
	return ConfigInfo{MaxRequestLoad: p.MaxRequestLoad}
}

// Select picks the best instance for a single request.
func (p *CacheAwarePolicy) Select(_ context.Context, req *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	epoch := nextEpoch()

	// Compute load bounds across available nodes.
	minLoad, maxLoad, available := p.loadBoundsLocked(nodes)
	if available == 0 {
		return nil, ErrNoInstances
	}

	var chosen, branch string

	if p.isImbalanced(minLoad, maxLoad) {
		// Imbalanced: route to min-load instance.
		chosen = p.selectMinLoadLocked(nodes)
		branch = branchImbalanced
	} else {
		// Balanced: use radix tree prefix matching.
		chosen, branch = p.selectByPrefixLocked(req.RequestText, nodes)
	}

	if chosen == "" {
		return nil, ErrAllOverloaded
	}

	// Update tree with chosen instance and increment load.
	if req.RequestText != "" {
		p.tree.Insert(req.RequestText, chosen, epoch)
	}
	p.requestLoad[chosen]++
	p.branches.incByName(branch)

	// Inline eviction check.
	p.maybeEvictLocked(epoch)

	return nodes[chosen].Instance, nil
}

// Feedback decrements request load for the released instance.
func (p *CacheAwarePolicy) Feedback(instanceID string, _ *domain.CostMetrics) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.requestLoad[instanceID] > 0 {
		p.requestLoad[instanceID]--
		if p.requestLoad[instanceID] == 0 {
			delete(p.requestLoad, instanceID)
		}
	}
}

// Reset clears all state (tree, load counters). Implements Resettable.
func (p *CacheAwarePolicy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tree.Reset()
	clear(p.requestLoad)
	p.lastEvictionEpoch = 0
}

// RemoveSession is a no-op — CacheAwarePolicy doesn't use session bindings.
// Implements SessionRemover for interface compatibility.
func (p *CacheAwarePolicy) RemoveSession(_ string) {}

// ---------- BatchSelector ----------

type cacheNodeEntry struct {
	instID string
	inst   *domain.Instance
	load   int64
}

// BatchSelect allocates a batch of requests using two-mode routing.
//
// Complexity: O(N + K*textLen) for balanced mode, O(N + K*logN) for imbalanced.
func (p *CacheAwarePolicy) BatchSelect(reqs []*domain.RouteContext, nodes map[string]*domain.NodeState) []BatchSelectResult {
	// Reuse resultsBuf.
	if cap(p.resultsBuf) >= len(reqs) {
		p.resultsBuf = p.resultsBuf[:len(reqs)]
	} else {
		p.resultsBuf = make([]BatchSelectResult, len(reqs))
	}
	results := p.resultsBuf
	clear(results)

	if len(nodes) == 0 {
		for i := range results {
			results[i].Err = ErrNoInstances
		}
		return results
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Share one epoch for the entire batch (avoid per-request atomic).
	epoch := nextEpoch()

	// Compute load bounds once.
	minLoad, maxLoad, available := p.loadBoundsLocked(nodes)
	if available == 0 {
		for i := range results {
			results[i].Err = ErrNoInstances
		}
		return results
	}

	imbalanced := p.isImbalanced(minLoad, maxLoad)

	if imbalanced {
		// Build min-heap for shortest-queue allocation.
		results = p.batchSelectImbalanced(reqs, nodes, results, epoch)
	} else {
		// Per-request prefix matching.
		results = p.batchSelectBalanced(reqs, nodes, results, epoch)
	}

	// Inline eviction check.
	p.maybeEvictLocked(epoch)

	return results
}

// batchSelectImbalanced uses min-heap (shortest queue) for all requests.
func (p *CacheAwarePolicy) batchSelectImbalanced(
	reqs []*domain.RouteContext,
	nodes map[string]*domain.NodeState,
	results []BatchSelectResult,
	epoch uint64,
) []BatchSelectResult {
	// Build heap.
	if cap(p.heapBuf) >= len(nodes) {
		p.heapBuf = p.heapBuf[:0]
	} else {
		p.heapBuf = make([]cacheNodeEntry, 0, len(nodes))
	}
	heap := p.heapBuf
	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		load := p.requestLoad[id]
		if load >= p.MaxRequestLoad {
			continue
		}
		heap = append(heap, cacheNodeEntry{instID: id, inst: ns.Instance, load: load})
	}
	cacheHeapInit(heap)

	var branchImbalancedCount int

	for i, req := range reqs {
		if len(heap) == 0 {
			results[i].Err = ErrAllOverloaded
			continue
		}
		branchImbalancedCount++
		chosen := heap[0].instID
		results[i].Instance = heap[0].inst
		p.requestLoad[chosen]++
		heap[0].load++

		// Insert into tree to maintain cache state.
		if req.RequestText != "" {
			p.tree.Insert(req.RequestText, chosen, epoch)
		}

		if heap[0].load >= p.MaxRequestLoad {
			heap[0] = heap[len(heap)-1]
			heap = heap[:len(heap)-1]
			if len(heap) > 0 {
				cacheHeapDown(heap, 0, len(heap))
			}
		} else {
			cacheHeapDown(heap, 0, len(heap))
		}
	}
	p.heapBuf = heap
	if branchImbalancedCount > 0 {
		p.branches.Imbalanced.Add(float64(branchImbalancedCount))
	}
	return results
}

// batchSelectBalanced uses per-request prefix matching.
func (p *CacheAwarePolicy) batchSelectBalanced(
	reqs []*domain.RouteContext,
	nodes map[string]*domain.NodeState,
	results []BatchSelectResult,
	epoch uint64,
) []BatchSelectResult {
	// Branch counters — accumulate locally via map, flush once after loop.
	branchCounts := make(map[string]int, 5)

	for i, req := range reqs {
		chosen, branch := p.selectByPrefixLocked(req.RequestText, nodes)
		if chosen == "" {
			results[i].Err = ErrNoInstances
			continue
		}
		branchCounts[branch]++
		results[i].Instance = nodes[chosen].Instance
		if req.RequestText != "" {
			p.tree.Insert(req.RequestText, chosen, epoch)
		}
		p.requestLoad[chosen]++
	}

	for branch, count := range branchCounts {
		p.branches.addByName(branch, count)
	}
	return results
}

// ---------- internal helpers ----------

// loadBoundsLocked computes min/max requestLoad across available nodes. Caller holds mu.
func (p *CacheAwarePolicy) loadBoundsLocked(nodes map[string]*domain.NodeState) (minLoad, maxLoad int64, available int) {
	minLoad = math.MaxInt64
	maxLoad = 0
	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		available++
		load := p.requestLoad[id]
		if load < minLoad {
			minLoad = load
		}
		if load > maxLoad {
			maxLoad = load
		}
	}
	if available == 0 {
		minLoad = 0
	}
	return
}

// isImbalanced checks both absolute and relative thresholds (same as sglang).
func (p *CacheAwarePolicy) isImbalanced(minLoad, maxLoad int64) bool {
	return (maxLoad-minLoad) > p.BalanceAbsThreshold &&
		float64(maxLoad) > float64(minLoad)*p.BalanceRelThreshold
}

// selectMinLoadLocked picks the available instance with the lowest requestLoad.
func (p *CacheAwarePolicy) selectMinLoadLocked(nodes map[string]*domain.NodeState) string {
	var (
		bestID   string
		bestLoad int64 = math.MaxInt64
	)
	for id := range nodes {
		load := p.requestLoad[id]
		if load >= p.MaxRequestLoad {
			continue
		}
		if load < bestLoad {
			bestLoad = load
			bestID = id
		}
	}
	return bestID
}

// selectByPrefixLocked uses the radix tree for prefix-based routing.
// Falls back to min-tree-bytes instance on low match rate.
// Returns (instance_id, branch_label).
func (p *CacheAwarePolicy) selectByPrefixLocked(text string, nodes map[string]*domain.NodeState) (string, string) {
	if text == "" {
		// No text — route to instance with smallest tree footprint.
		return p.selectMinTreeBytesLocked(nodes), branchEmptyText
	}

	result := p.tree.PrefixMatch(text)

	if result.Tenant != "" && result.MatchRate() > p.CacheThreshold {
		// High match rate — route to the matching tenant if available.
		if ns, ok := nodes[result.Tenant]; ok && ns.LoadAvailable() {
			load := p.requestLoad[result.Tenant]
			if load < p.MaxRequestLoad {
				return result.Tenant, branchCacheHit
			}
		}
		// Matched tenant unavailable/overloaded — remove stale tenant from tree.
		p.tree.RemoveTenant(result.Tenant)
		return p.selectMinTreeBytesLocked(nodes), branchTenantEvict
	}

	// Low match rate or tenant unavailable — route to min-tree-bytes instance.
	return p.selectMinTreeBytesLocked(nodes), branchCacheMiss
}

// selectMinTreeBytesLocked picks the available instance with the smallest
// radix tree byte count (most available cache capacity).
func (p *CacheAwarePolicy) selectMinTreeBytesLocked(nodes map[string]*domain.NodeState) string {
	var (
		bestID    string
		bestBytes int64 = math.MaxInt64
	)
	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		load := p.requestLoad[id]
		if load >= p.MaxRequestLoad {
			continue
		}
		tb := p.tree.TenantByteCount(id)
		if tb < bestBytes {
			bestBytes = tb
			bestID = id
		}
	}
	return bestID
}

// maybeEvictLocked runs inline eviction if enough epochs have passed.
// EvictionIntervalSec is converted to an epoch count estimate (1 epoch ≈ 1 request).
func (p *CacheAwarePolicy) maybeEvictLocked(currentEpoch uint64) {
	// Use epoch delta as a proxy for time elapsed.
	// In practice, each epoch ≈ one request, so EvictionIntervalSec * ~100 req/s is reasonable.
	// For simplicity, we use a fixed interval of EvictionIntervalSec * 100 epochs.
	interval := uint64(p.EvictionIntervalSec) * 100
	if interval == 0 {
		interval = 6000
	}
	if currentEpoch-p.lastEvictionEpoch < interval {
		return
	}
	p.lastEvictionEpoch = currentEpoch
	p.tree.EvictBySize(p.MaxTreeSize)
}

// ---------- inline min-heap for cacheNodeEntry ----------

func cacheHeapInit(h []cacheNodeEntry) {
	n := len(h)
	for i := n/2 - 1; i >= 0; i-- {
		cacheHeapDown(h, i, n)
	}
}

func cacheHeapDown(h []cacheNodeEntry, i, n int) {
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		j := left
		if right := left + 1; right < n && h[right].load < h[left].load {
			j = right
		}
		if h[i].load <= h[j].load {
			break
		}
		h[i], h[j] = h[j], h[i]
		i = j
	}
}

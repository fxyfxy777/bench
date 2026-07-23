package policy

import (
	"context"
	"math"
	"sync"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/metrics"
)

// SessionAwareV4Policy combines V3's session affinity with cache_aware's
// dual-mode load balancing (imbalanced / balanced routing).
//
// Key improvements over V3:
//   - Global imbalance detection: isImbalanced() uses dual thresholds
//     (absolute + relative) to detect cluster-wide load skew. In imbalanced
//     mode, all requests route to the shortest queue, ignoring session affinity.
//   - Heap position tracking: BatchSelect maintains a heapIndex map so that
//     "stay" decisions update the heap entry in-place, eliminating V3's
//     cumulative heap staleness across large batches.
//   - Session LRU eviction: sessionAssign entries carry an epoch timestamp;
//     cold sessions are periodically evicted via maybeEvictSessionsLocked.
//   - Batch invalidation: when a dead instance is discovered, all sessions
//     bound to it are swept in one pass; deadInstCache prevents re-scanning
//     the same dead instance within a single batch.
//
// Implements: Policy, BatchSelector, Resettable, SessionRemover.
type SessionAwareV4Policy struct {
	mu sync.Mutex

	// sessionAssign maps session_id → {instance_id, lastEpoch}.
	// NOT a permanent binding — remembers the last instance + freshness.
	sessionAssign map[string]sessionEntry

	// requestLoad maps instance_id → current in-flight request count.
	requestLoad map[string]int64

	// sessionLoad maps instance_id → number of active sessions bound to it.
	// Used for session-level metrics tracking.
	sessionLoad map[string]int64

	// sessionCount tracks global active session count for admission control.
	sessionCount int64

	// deadInstCache records instance IDs whose sessions have already been
	// bulk-invalidated within the current batch, avoiding redundant sweeps.
	deadInstCache map[string]struct{}

	// Configuration (with defaults).
	MaxSessionLoad       int64   // per-instance request load cap (default 100)
	MaxRequestLoad       int64   // per-instance active-request cap (authoritative); 0 = no cap
	LoadDiffThreshold    int64   // migration sensitivity for balanced mode (default 1)
	BalanceAbsThreshold  int64   // absolute load diff for imbalance detection (default 32)
	BalanceRelThreshold  float64 // relative load ratio for imbalance detection (default 1.1)
	SessionEvictEpochs   uint64  // epoch interval for session LRU eviction (default 6000)
	SessionAffinityBoost int64   // max extra threshold for warm sessions (default 8)

	lastEvictionEpoch uint64

	// Pre-resolved branch counters for observability.
	branches *SessionAwareV4Branches

	// Reusable buffers — only accessed from the event-loop (single-threaded).
	resultsBuf   []BatchSelectResult
	heapBuf      []v4NodeEntry
	heapIndexBuf map[string]int // instance_id → heap position
}

// sessionEntry stores session mapping with freshness and warmth tracking.
type sessionEntry struct {
	instanceID   string
	lastEpoch    uint64
	requestCount uint32 // requests routed to current instance; reset on migrate
}

// SessionAwareV4Config holds configuration for NewSessionAwareV4Policy.
type SessionAwareV4Config struct {
	MaxSessionLoad       int64
	MaxRequestLoad       int64
	LoadDiffThreshold    int64
	BalanceAbsThreshold  int64
	BalanceRelThreshold  float64
	SessionEvictEpochs   uint64
	SessionAffinityBoost int64 // max extra threshold for warm sessions (default 8)
}

// NewSessionAwareV4Policy creates a SessionAwareV4Policy with the given config.
// Zero-valued fields use sensible defaults.
func NewSessionAwareV4Policy(cfg SessionAwareV4Config) *SessionAwareV4Policy {
	if cfg.MaxSessionLoad <= 0 {
		cfg.MaxSessionLoad = 100
	}
	if cfg.LoadDiffThreshold <= 0 {
		cfg.LoadDiffThreshold = 1
	}
	if cfg.BalanceAbsThreshold <= 0 {
		cfg.BalanceAbsThreshold = 32
	}
	if cfg.BalanceRelThreshold <= 0 {
		cfg.BalanceRelThreshold = 1.1
	}
	if cfg.SessionEvictEpochs == 0 {
		cfg.SessionEvictEpochs = 6000
	}
	if cfg.SessionAffinityBoost <= 0 {
		cfg.SessionAffinityBoost = 8
	}
	return &SessionAwareV4Policy{
		sessionAssign:        make(map[string]sessionEntry),
		requestLoad:          make(map[string]int64),
		sessionLoad:          make(map[string]int64),
		deadInstCache:        make(map[string]struct{}),
		MaxSessionLoad:       cfg.MaxSessionLoad,
		MaxRequestLoad:       cfg.MaxRequestLoad,
		LoadDiffThreshold:    cfg.LoadDiffThreshold,
		BalanceAbsThreshold:  cfg.BalanceAbsThreshold,
		BalanceRelThreshold:  cfg.BalanceRelThreshold,
		SessionEvictEpochs:   cfg.SessionEvictEpochs,
		SessionAffinityBoost: cfg.SessionAffinityBoost,
		branches:             newSessionAwareV4Branches(),
	}
}

func (p *SessionAwareV4Policy) Name() Name { return NameSessionAwareV4 }

// ConfigSummary implements ConfigDescriber.
func (p *SessionAwareV4Policy) ConfigSummary() ConfigInfo {
	return ConfigInfo{MaxRequestLoad: p.MaxRequestLoad, MaxSessionLoad: p.MaxSessionLoad}
}

// Select picks the best instance for a single request.
func (p *SessionAwareV4Policy) Select(_ context.Context, req *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	sessionID := req.SessionID
	if sessionID == "" {
		inst, err := p.selectMinRequestLoad(nodes)
		if err == nil {
			p.branches.NoSessionFallback.Inc()
		}
		return inst, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	epoch := nextEpoch()

	// Global imbalance detection.
	scan := p.scanNodesLocked(nodes)
	if scan.available == 0 {
		return nil, ErrNoInstances
	}
	imbalanced := p.isImbalanced(scan.minLoad, scan.maxLoad)

	// Existing session mapping.
	if entry, ok := p.sessionAssign[sessionID]; ok {
		if ns, exists := nodes[entry.instanceID]; exists && ns.LoadAvailable() {
			// MaxRequestLoad gate: force migrate if at authoritative cap.
			authOverload := p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad

			lastLoad := p.requestLoad[entry.instanceID]

			if imbalanced || authOverload {
				// Imbalanced mode or authoritative overload: route to min-load, ignore affinity.
				if scan.bestID == "" {
					if authOverload {
						return nil, ErrAllOverloaded
					}
					return nil, ErrNoInstances
				}
				// Update sessionLoad metrics for migration.
				if oldLoad, ok := p.sessionLoad[entry.instanceID]; ok {
					p.sessionLoad[entry.instanceID] = oldLoad - 1
					if p.sessionLoad[entry.instanceID] <= 0 {
						delete(p.sessionLoad, entry.instanceID)
					}
					metrics.InstanceSessions.WithLabelValues(entry.instanceID).Dec()
				}
				p.sessionLoad[scan.bestID]++
				metrics.InstanceSessions.WithLabelValues(scan.bestID).Inc()
				p.sessionAssign[sessionID] = sessionEntry{instanceID: scan.bestID, lastEpoch: epoch, requestCount: 0}
				p.requestLoad[scan.bestID]++
				p.branches.ImbalancedMigrate.Inc()
				return nodes[scan.bestID].Instance, nil
			}

			// Balanced mode: CompareAndSchedule with warmth-boosted threshold.
			if scan.bestID == "" {
				return nil, ErrNoInstances
			}

			effectiveThresh := p.effectiveThreshold(entry.requestCount)
			chosen := entry.instanceID
			if lastLoad >= p.MaxSessionLoad {
				chosen = scan.bestID
			} else if (lastLoad - scan.bestLoad) > effectiveThresh {
				chosen = scan.bestID
			}

			if chosen == entry.instanceID {
				// Stay: session cache grows warmer.
				p.sessionAssign[sessionID] = sessionEntry{instanceID: chosen, lastEpoch: epoch, requestCount: entry.requestCount + 1}
				p.branches.Stay.Inc()
			} else {
				// Migrate: new instance has cold cache.
				p.sessionAssign[sessionID] = sessionEntry{instanceID: chosen, lastEpoch: epoch, requestCount: 0}
				// Update sessionLoad metrics for migration.
				if oldLoad, ok := p.sessionLoad[entry.instanceID]; ok {
					p.sessionLoad[entry.instanceID] = oldLoad - 1
					if p.sessionLoad[entry.instanceID] <= 0 {
						delete(p.sessionLoad, entry.instanceID)
					}
					metrics.InstanceSessions.WithLabelValues(entry.instanceID).Dec()
				}
				p.sessionLoad[chosen]++
				metrics.InstanceSessions.WithLabelValues(chosen).Inc()
				if lastLoad >= p.MaxSessionLoad {
					p.branches.MigrateOverload.Inc()
				} else {
					p.branches.MigrateLoadDiff.Inc()
				}
			}
			p.requestLoad[chosen]++
			return nodes[chosen].Instance, nil
		}
		// Stale mapping — instance gone. Batch invalidate.
		// Note: invalidateInstanceSessionsLocked will handle sessionLoad and sessionCount cleanup.
		p.invalidateInstanceSessionsLocked(entry.instanceID)
		p.branches.StaleReassign.Inc()
		// Fall through as new session (sessionCount already decremented).
	}

	// New session (or stale mapping cleaned up).
	if scan.available == 0 {
		return nil, ErrNoInstances
	}

	// Admission control.
	maxSessions := p.MaxSessionLoad * int64(scan.available)
	if p.sessionCount >= maxSessions {
		return nil, ErrAllExceedSession
	}

	if scan.bestID == "" {
		return nil, ErrNoInstances
	}

	p.sessionAssign[sessionID] = sessionEntry{instanceID: scan.bestID, lastEpoch: epoch, requestCount: 0}
	p.sessionCount++
	p.sessionLoad[scan.bestID]++
	metrics.InstanceSessions.WithLabelValues(scan.bestID).Inc()
	p.branches.NewSession.Inc()
	p.requestLoad[scan.bestID]++
	p.maybeEvictSessionsLocked(epoch)

	return nodes[scan.bestID].Instance, nil
}

// selectMinRequestLoad picks the instance with the fewest in-flight requests.
// Used for non-session requests (empty SessionID).
func (p *SessionAwareV4Policy) selectMinRequestLoad(nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var (
		best     *domain.Instance
		bestLoad int64 = math.MaxInt64
		anyAvail bool
	)
	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		anyAvail = true
		if p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad {
			continue
		}
		load := p.requestLoad[id]
		if load < bestLoad {
			bestLoad = load
			best = ns.Instance
		}
	}
	if best == nil {
		if anyAvail {
			return nil, ErrAllOverloaded
		}
		return nil, ErrNoInstances
	}
	p.requestLoad[best.ID]++
	return best, nil
}

// Feedback decrements request load for the released instance.
func (p *SessionAwareV4Policy) Feedback(instanceID string, _ *domain.CostMetrics) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.requestLoad[instanceID] > 0 {
		p.requestLoad[instanceID]--
		if p.requestLoad[instanceID] == 0 {
			delete(p.requestLoad, instanceID)
		}
	}
}

// Reset clears all session mappings, request load counters, and eviction state.
// Implements Resettable. Called between training steps.
func (p *SessionAwareV4Policy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.sessionAssign)
	clear(p.requestLoad)
	clear(p.sessionLoad)
	clear(p.deadInstCache)
	p.sessionCount = 0
	p.lastEvictionEpoch = 0
	// Keep resultsBuf, heapBuf, heapIndexBuf backing arrays for reuse.
}

// HasSession reports whether the given session is currently tracked.
func (p *SessionAwareV4Policy) HasSession(sessionID string) bool {
	p.mu.Lock()
	_, ok := p.sessionAssign[sessionID]
	p.mu.Unlock()
	return ok
}

// RemoveSession unbinds a session and decrements the global session counter.
// Does NOT touch requestLoad — in-flight requests continue and will be
// decremented naturally via Feedback on release.
func (p *SessionAwareV4Policy) RemoveSession(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.sessionAssign[sessionID]
	if !ok {
		return
	}
	instID := entry.instanceID
	delete(p.sessionAssign, sessionID)
	if p.sessionCount > 0 {
		p.sessionCount--
	}
	// Decrement sessionLoad metrics for the instance.
	if oldLoad, ok := p.sessionLoad[instID]; ok {
		p.sessionLoad[instID] = oldLoad - 1
		if p.sessionLoad[instID] <= 0 {
			delete(p.sessionLoad, instID)
		}
		metrics.InstanceSessions.WithLabelValues(instID).Dec()
	}
}

// ---------- BatchSelector implementation ----------

// v4NodeEntry is a heap element for BatchSelect assignment.
type v4NodeEntry struct {
	instID string
	inst   *domain.Instance
	load   int64
}

// BatchSelect allocates a batch of requests using dual-mode routing.
//
// Mode A (imbalanced): all requests go through min-heap shortest queue,
// ignoring session affinity entirely.
// Mode B (balanced): per-request CompareAndSchedule with heap position
// tracking to eliminate V3's cumulative staleness.
//
// Total complexity: O(N + K*logN).
//
// NOT safe for concurrent use. Must be called from the event-loop only.
func (p *SessionAwareV4Policy) BatchSelect(reqs []*domain.RouteContext, nodes map[string]*domain.NodeState) []BatchSelectResult {
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

	epoch := nextEpoch()
	clear(p.deadInstCache)

	// Single-pass load scan (O(N), once).
	scan := p.scanNodesLocked(nodes)
	if scan.available == 0 {
		for i := range results {
			results[i].Err = ErrNoInstances
		}
		return results
	}

	if p.isImbalanced(scan.minLoad, scan.maxLoad) {
		results = p.batchSelectImbalanced(reqs, nodes, results, epoch)
	} else {
		results = p.batchSelectBalanced(reqs, nodes, results, epoch)
	}

	p.maybeEvictSessionsLocked(epoch)

	return results
}

// batchSelectImbalanced routes all requests via min-heap shortest queue,
// ignoring session affinity. Still updates sessionAssign for next batch.
func (p *SessionAwareV4Policy) batchSelectImbalanced(
	reqs []*domain.RouteContext,
	nodes map[string]*domain.NodeState,
	results []BatchSelectResult,
	epoch uint64,
) []BatchSelectResult {
	heap, _ := p.buildHeap(nodes)

	var branchImbalanced, branchNoSession int

	for i, req := range reqs {
		if len(heap) == 0 {
			results[i].Err = ErrAllExceedSession
			continue
		}

		chosen := heap[0].instID
		results[i].Instance = heap[0].inst
		p.requestLoad[chosen]++
		heap[0].load++

		// Update session mapping for future balanced-mode batches.
		if req.SessionID != "" {
			if _, exists := p.sessionAssign[req.SessionID]; !exists {
				p.sessionCount++
				p.sessionLoad[chosen]++
				metrics.InstanceSessions.WithLabelValues(chosen).Inc()
			} else {
				// Existing session, may be migrating.
				if oldEntry, ok := p.sessionAssign[req.SessionID]; ok && oldEntry.instanceID != chosen {
					if oldLoad, ok := p.sessionLoad[oldEntry.instanceID]; ok {
						p.sessionLoad[oldEntry.instanceID] = oldLoad - 1
						if p.sessionLoad[oldEntry.instanceID] <= 0 {
							delete(p.sessionLoad, oldEntry.instanceID)
						}
						metrics.InstanceSessions.WithLabelValues(oldEntry.instanceID).Dec()
					}
					p.sessionLoad[chosen]++
					metrics.InstanceSessions.WithLabelValues(chosen).Inc()
				}
			}
			p.sessionAssign[req.SessionID] = sessionEntry{instanceID: chosen, lastEpoch: epoch, requestCount: 0}
			branchImbalanced++
		} else {
			branchNoSession++
		}

		if heap[0].load >= p.MaxSessionLoad {
			heap[0] = heap[len(heap)-1]
			heap = heap[:len(heap)-1]
			if len(heap) > 0 {
				v4HeapDown(heap, 0, len(heap))
			}
		} else {
			v4HeapDown(heap, 0, len(heap))
		}
	}

	p.heapBuf = heap
	if branchImbalanced > 0 {
		p.branches.ImbalancedMigrate.Add(float64(branchImbalanced))
	}
	if branchNoSession > 0 {
		p.branches.NoSessionFallback.Add(float64(branchNoSession))
	}
	return results
}

// batchSelectBalanced uses per-request CompareAndSchedule with heap index
// tracking. "Stay" decisions update the heap entry in-place to maintain
// accurate min-load information for subsequent requests in the batch.
func (p *SessionAwareV4Policy) batchSelectBalanced(
	reqs []*domain.RouteContext,
	nodes map[string]*domain.NodeState,
	results []BatchSelectResult,
	epoch uint64,
) []BatchSelectResult {
	heap, heapIndex := p.buildHeapWithIndex(nodes)

	availableCount := int64(len(heap))

	// Branch counters — accumulate locally, flush once after loop.
	var branchStay, branchMigrateOverload, branchMigrateLoadDiff int
	var branchNewSession, branchStale, branchNoSession int

	for i, req := range reqs {
		sessionID := req.SessionID

		if sessionID == "" {
			// Non-session request: assign to min-load.
			if len(heap) == 0 {
				results[i].Err = ErrAllExceedSession
				continue
			}
			branchNoSession++
			results[i].Instance = heap[0].inst
			p.requestLoad[heap[0].instID]++
			heap[0].load++
			if heap[0].load >= p.MaxSessionLoad {
				p.removeHeapTop(&heap, heapIndex)
			} else {
				v4HeapDownIdx(heap, 0, len(heap), heapIndex)
			}
			continue
		}

		// Existing session mapping.
		if entry, ok := p.sessionAssign[sessionID]; ok {
			if ns, exists := nodes[entry.instanceID]; exists && ns.LoadAvailable() {
				lastLoad := p.requestLoad[entry.instanceID]
				authOverload := p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad

				effectiveThresh := p.effectiveThreshold(entry.requestCount)
				if len(heap) > 0 && (authOverload || lastLoad >= p.MaxSessionLoad || (lastLoad-heap[0].load) > effectiveThresh) {
					// Migrate to min-load instance (cold cache on new instance).
					if authOverload || lastLoad >= p.MaxSessionLoad {
						branchMigrateOverload++
					} else {
						branchMigrateLoadDiff++
					}
					chosen := heap[0].instID
					// Update sessionLoad metrics for migration.
					if oldLoad, ok := p.sessionLoad[entry.instanceID]; ok {
						p.sessionLoad[entry.instanceID] = oldLoad - 1
						if p.sessionLoad[entry.instanceID] <= 0 {
							delete(p.sessionLoad, entry.instanceID)
						}
						metrics.InstanceSessions.WithLabelValues(entry.instanceID).Dec()
					}
					p.sessionLoad[chosen]++
					metrics.InstanceSessions.WithLabelValues(chosen).Inc()
					p.sessionAssign[sessionID] = sessionEntry{instanceID: chosen, lastEpoch: epoch, requestCount: 0}
					results[i].Instance = heap[0].inst
					p.requestLoad[chosen]++
					heap[0].load++
					if heap[0].load >= p.MaxSessionLoad {
						p.removeHeapTop(&heap, heapIndex)
					} else {
						v4HeapDownIdx(heap, 0, len(heap), heapIndex)
					}
				} else {
					// Stay on lastInstance (cache affinity, warmth grows).
					branchStay++
					results[i].Instance = ns.Instance
					p.requestLoad[entry.instanceID]++
					p.sessionAssign[sessionID] = sessionEntry{instanceID: entry.instanceID, lastEpoch: epoch, requestCount: entry.requestCount + 1}

					// Update heap entry for this instance to maintain accuracy.
					if idx, inHeap := heapIndex[entry.instanceID]; inHeap {
						heap[idx].load++
						if heap[idx].load >= p.MaxSessionLoad {
							p.removeHeapAt(&heap, idx, heapIndex)
						} else {
							v4HeapDownIdx(heap, idx, len(heap), heapIndex)
							v4HeapUpIdx(heap, idx, heapIndex)
						}
					}
				}
				continue
			}
			// Stale mapping — instance gone. Batch invalidate.
			branchStale++
			deadInstID := entry.instanceID
			if _, alreadyCleaned := p.deadInstCache[deadInstID]; !alreadyCleaned {
				p.invalidateInstanceSessionsLocked(deadInstID)
				p.deadInstCache[deadInstID] = struct{}{}
			}
			// Fall through as new session (sessionCount already decremented by invalidate).
		} else {
			branchNewSession++
		}

		// New session (or stale mapping cleaned up).
		if len(heap) == 0 {
			results[i].Err = ErrAllExceedSession
			continue
		}

		// Admission control.
		if availableCount > 0 && p.sessionCount >= p.MaxSessionLoad*availableCount {
			results[i].Err = ErrAllExceedSession
			continue
		}

		chosen := heap[0].instID
		p.sessionAssign[sessionID] = sessionEntry{instanceID: chosen, lastEpoch: epoch, requestCount: 0}
		p.sessionCount++
		p.sessionLoad[chosen]++
		metrics.InstanceSessions.WithLabelValues(chosen).Inc()
		results[i].Instance = heap[0].inst
		p.requestLoad[chosen]++
		heap[0].load++
		if heap[0].load >= p.MaxSessionLoad {
			p.removeHeapTop(&heap, heapIndex)
		} else {
			v4HeapDownIdx(heap, 0, len(heap), heapIndex)
		}
	}

	p.heapBuf = heap
	p.flushBalancedBranchCounters(branchStay, branchMigrateOverload, branchMigrateLoadDiff, branchNewSession, branchStale, branchNoSession)
	return results
}

// flushBalancedBranchCounters writes accumulated balanced-mode batch branch counts to Prometheus.
func (p *SessionAwareV4Policy) flushBalancedBranchCounters(stay, migrateOverload, migrateLoadDiff, newSession, stale, noSession int) {
	if stay > 0 {
		p.branches.Stay.Add(float64(stay))
	}
	if migrateOverload > 0 {
		p.branches.MigrateOverload.Add(float64(migrateOverload))
	}
	if migrateLoadDiff > 0 {
		p.branches.MigrateLoadDiff.Add(float64(migrateLoadDiff))
	}
	if newSession > 0 {
		p.branches.NewSession.Add(float64(newSession))
	}
	if stale > 0 {
		p.branches.StaleReassign.Add(float64(stale))
	}
	if noSession > 0 {
		p.branches.NoSessionFallback.Add(float64(noSession))
	}
}

// ---------- internal helpers ----------

// effectiveThreshold returns the warmth-boosted migration threshold.
// Warmer sessions (higher requestCount) tolerate more load imbalance
// before migrating, inspired by sglang's cache-aware match_rate concept.
func (p *SessionAwareV4Policy) effectiveThreshold(requestCount uint32) int64 {
	boost := min(int64(requestCount), p.SessionAffinityBoost)
	return p.LoadDiffThreshold + boost
}

// loadSummary holds the result of a single scan over all available nodes.
type loadSummary struct {
	minLoad   int64  // lowest requestLoad among available nodes
	maxLoad   int64  // highest requestLoad among available nodes
	available int    // number of LoadAvailable() nodes
	bestID    string // instance with lowest load below MaxSessionLoad ("" if none)
	bestLoad  int64  // load of bestID
}

// scanNodesLocked computes min/max requestLoad, available count, and the
// best (min-load, below MaxSessionLoad) instance in a single O(N) pass.
// Replaces the previous loadBoundsLocked + findMinLoadLocked two-pass pattern.
func (p *SessionAwareV4Policy) scanNodesLocked(nodes map[string]*domain.NodeState) loadSummary {
	s := loadSummary{
		minLoad:  math.MaxInt64,
		bestLoad: math.MaxInt64,
	}
	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		if p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad {
			continue
		}
		s.available++
		load := p.requestLoad[id]
		if load < s.minLoad {
			s.minLoad = load
		}
		if load > s.maxLoad {
			s.maxLoad = load
		}
		if load < s.bestLoad && load < p.MaxSessionLoad {
			s.bestLoad = load
			s.bestID = id
		}
	}
	if s.available == 0 {
		s.minLoad = 0
	}
	return s
}

// isImbalanced checks both absolute and relative thresholds.
func (p *SessionAwareV4Policy) isImbalanced(minLoad, maxLoad int64) bool {
	return (maxLoad-minLoad) > p.BalanceAbsThreshold &&
		float64(maxLoad) > float64(minLoad)*p.BalanceRelThreshold
}

// invalidateInstanceSessionsLocked sweeps all sessions bound to instanceID
// and removes them. Decrements sessionCount for each removed session.
// Decrements sessionLoad by the total number of removed sessions once.
func (p *SessionAwareV4Policy) invalidateInstanceSessionsLocked(instanceID string) {
	// Count sessions to remove and metrics delta.
	var removedCount int
	for sid, entry := range p.sessionAssign {
		if entry.instanceID == instanceID {
			removedCount++
			if p.sessionCount > 0 {
				p.sessionCount--
			}
			delete(p.sessionAssign, sid)
		}
	}
	// Decrement sessionLoad and metrics once for all removed sessions.
	if removedCount > 0 {
		if oldLoad, ok := p.sessionLoad[instanceID]; ok {
			p.sessionLoad[instanceID] = oldLoad - int64(removedCount)
			if p.sessionLoad[instanceID] <= 0 {
				delete(p.sessionLoad, instanceID)
			}
			for i := 0; i < removedCount; i++ {
				metrics.InstanceSessions.WithLabelValues(instanceID).Dec()
			}
		}
	}
}

// maybeEvictSessionsLocked periodically evicts sessions that haven't been
// active for SessionEvictEpochs epochs. Updates sessionLoad metrics.
func (p *SessionAwareV4Policy) maybeEvictSessionsLocked(epoch uint64) {
	if p.SessionEvictEpochs == 0 {
		return
	}
	if epoch-p.lastEvictionEpoch < p.SessionEvictEpochs {
		return
	}
	p.lastEvictionEpoch = epoch
	threshold := epoch - p.SessionEvictEpochs

	// Collect evicted sessions by instance.
	evictedByInst := make(map[string]int)
	for sid, entry := range p.sessionAssign {
		if entry.lastEpoch < threshold {
			instID := entry.instanceID
			evictedByInst[instID]++
			delete(p.sessionAssign, sid)
			if p.sessionCount > 0 {
				p.sessionCount--
			}
		}
	}

	// Decrement sessionLoad and metrics once per instance.
	for instID, count := range evictedByInst {
		if oldLoad, ok := p.sessionLoad[instID]; ok {
			p.sessionLoad[instID] = oldLoad - int64(count)
			if p.sessionLoad[instID] <= 0 {
				delete(p.sessionLoad, instID)
			}
			for range count {
				metrics.InstanceSessions.WithLabelValues(instID).Dec()
			}
		}
	}
}

// buildHeap constructs a min-heap from available nodes (without index tracking).
func (p *SessionAwareV4Policy) buildHeap(nodes map[string]*domain.NodeState) ([]v4NodeEntry, int) {
	if cap(p.heapBuf) >= len(nodes) {
		p.heapBuf = p.heapBuf[:0]
	} else {
		p.heapBuf = make([]v4NodeEntry, 0, len(nodes))
	}
	heap := p.heapBuf
	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		if p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad {
			continue
		}
		load := p.requestLoad[id]
		if load >= p.MaxSessionLoad {
			continue
		}
		heap = append(heap, v4NodeEntry{instID: id, inst: ns.Instance, load: load})
	}
	v4HeapInit(heap)
	return heap, len(heap)
}

// buildHeapWithIndex constructs a min-heap with an instance_id → position index.
func (p *SessionAwareV4Policy) buildHeapWithIndex(nodes map[string]*domain.NodeState) ([]v4NodeEntry, map[string]int) {
	heap, _ := p.buildHeap(nodes)

	// Initialize or reuse heapIndex.
	if p.heapIndexBuf == nil {
		p.heapIndexBuf = make(map[string]int, len(heap))
	} else {
		clear(p.heapIndexBuf)
	}
	idx := p.heapIndexBuf
	for i := range heap {
		idx[heap[i].instID] = i
	}
	return heap, idx
}

// removeHeapTop removes the top element from the heap and updates the index.
func (p *SessionAwareV4Policy) removeHeapTop(heap *[]v4NodeEntry, idx map[string]int) {
	h := *heap
	n := len(h)
	if n == 0 {
		return
	}
	delete(idx, h[0].instID)
	if n == 1 {
		*heap = h[:0]
		return
	}
	h[0] = h[n-1]
	*heap = h[:n-1]
	idx[h[0].instID] = 0
	v4HeapDownIdx(*heap, 0, n-1, idx)
}

// removeHeapAt removes element at position i from the heap and updates the index.
func (p *SessionAwareV4Policy) removeHeapAt(heap *[]v4NodeEntry, i int, idx map[string]int) {
	h := *heap
	n := len(h)
	if i < 0 || i >= n {
		return
	}
	delete(idx, h[i].instID)
	if i == n-1 {
		*heap = h[:n-1]
		return
	}
	h[i] = h[n-1]
	*heap = h[:n-1]
	idx[h[i].instID] = i
	v4HeapDownIdx(*heap, i, n-1, idx)
	v4HeapUpIdx(*heap, i, idx)
}

// ---------- inline min-heap for v4NodeEntry ----------
// Two variants: without index (imbalanced mode) and with index (balanced mode).

func v4HeapInit(h []v4NodeEntry) {
	n := len(h)
	for i := n/2 - 1; i >= 0; i-- {
		v4HeapDown(h, i, n)
	}
}

// v4HeapDown sifts down without index tracking (for imbalanced mode).
func v4HeapDown(h []v4NodeEntry, i, n int) {
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

// v4HeapDownIdx sifts down with index tracking (for balanced mode).
func v4HeapDownIdx(h []v4NodeEntry, i, n int, idx map[string]int) {
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
		idx[h[i].instID] = i
		idx[h[j].instID] = j
		i = j
	}
}

// v4HeapUpIdx sifts up with index tracking (for balanced mode stay updates).
func v4HeapUpIdx(h []v4NodeEntry, i int, idx map[string]int) {
	for i > 0 {
		parent := (i - 1) / 2
		if h[parent].load <= h[i].load {
			break
		}
		h[i], h[parent] = h[parent], h[i]
		idx[h[i].instID] = i
		idx[h[parent].instID] = parent
		i = parent
	}
}

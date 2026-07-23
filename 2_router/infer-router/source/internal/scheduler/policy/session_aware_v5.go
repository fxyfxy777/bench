package policy

import (
	"context"
	"maps"
	"math"
	"sync"
	"sync/atomic"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/metrics"
)

// SessionAwareV5Policy combines V4's session affinity + cache_aware's
// block-aware scheduling with MinLoad's atomic metadata pattern.
//
// Key improvements over V3:
//   - Composite score: requestLoad + waitingCount + blockPressure + drift
//   - Block-aware gating: nodes below BlockMinThreshold excluded from new sessions
//   - Block emergency migration: sessions force-migrated when node blocks critical
//   - Mooncake migration cost estimation: warm sessions have higher migration threshold
//
// Implements: Policy, BatchSelector, Resettable, SessionRemover, MetaUpdater, DriftAware.
type SessionAwareV5Policy struct {
	mu sync.Mutex

	// sessionAssign maps session_id -> {instance_id, lastEpoch}.
	// NOT a permanent binding — remembers to last instance + freshness.
	sessionAssign map[string]sessionEntry

	// requestLoad maps instance_id -> current in-flight request count.
	requestLoad map[string]int64

	// sessionLoad maps instance_id -> number of active sessions bound to it.
	// Used for session-level metrics tracking.
	sessionLoad map[string]int64

	// sessionCount tracks global active session count for admission control.
	sessionCount int64

	// deadInstCache records instance IDs whose sessions have already been
	// bulk-invalidated within the current batch, avoiding redundant sweeps.
	deadInstCache map[string]struct{}

	// metaPtr holds per-instance load metadata via atomic.Pointer for lock-free reads.
	// Map structure changes (add/remove key) use COW swap (low frequency: instance registration).
	// Field value updates use atomic.Store on v5InstanceMeta fields (high frequency: every 500ms).
	// Event-loop reads via atomic.Pointer.Load + atomic.Int64.Load — zero lock, zero alloc.
	metaPtr atomic.Pointer[v5InstanceMetaMap]

	// Configuration (with defaults).
	MaxSessionLoad       int64   // per-instance request load cap (default 32 for SWE)
	MaxRequestLoad       int64   // per-instance active-request cap (authoritative); 0 = no cap
	LoadDiffThreshold    int64   // migration sensitivity for balanced mode (default 2)
	BalanceAbsThreshold  int64   // absolute load diff for imbalance detection (default 32)
	BalanceRelThreshold  float64 // relative load ratio for imbalance detection (default 1.1)
	SessionEvictEpochs   uint64  // epoch interval for session LRU eviction (default 6000)
	SessionAffinityBoost int64   // max extra threshold for warm sessions (default 8)

	BlockMinThreshold    int64 // minimum available GPU blocks for new session admission (default 256)
	BlockEmergencyThresh int64 // threshold below which sessions are force-migrated (default 128)

	// Composite score weights (sum should be 1.0).
	WeightRequestLoad   float64 // default 0.3
	WeightWaitingCount  float64 // default 0.3
	WeightBlockPressure float64 // default 0.3
	WeightDrift         float64 // default 0.1

	lastEvictionEpoch uint64

	// Pre-resolved branch counters for observability.
	branches *SessionAwareV5Branches

	// Reusable buffers — only accessed from the event-loop (single-threaded).
	resultsBuf   []BatchSelectResult
	heapBuf      []v5NodeEntry
	heapIndexBuf map[string]int // instance_id -> heap position
}

// v5InstanceMeta holds per-instance load metadata with atomic fields.
// Field values are updated at high frequency (every 2s) via atomic.Store
// from the MetricsCollector goroutine, while the event-loop reads them via
// atomic.Load — zero lock contention on the hot path.
type v5InstanceMeta struct {
	waitingCount    atomic.Int64
	availableBlocks atomic.Int64
	totalBlocks     atomic.Int64 // total GPU KV blocks capacity
	localDrift      atomic.Int64 // Acquire +1, Release -1, metrics refresh resets to 0
}

// v5InstanceMetaMap is the type stored in the atomic.Pointer for COW access.
type v5InstanceMetaMap = map[string]*v5InstanceMeta

// v5NodeEntry is a heap element for BatchSelect assignment.
type v5NodeEntry struct {
	instID          string
	inst            *domain.Instance
	compositeScore  float64 // multi-dimensional score (lower is better)
	availableBlocks int64
	totalBlocks     int64
	waitingCount    int64
	drift           int64
}

// SessionAwareV5Config holds configuration for NewSessionAwareV5Policy.
type SessionAwareV5Config struct {
	MaxSessionLoad       int64
	MaxRequestLoad       int64
	LoadDiffThreshold    int64
	BalanceAbsThreshold  int64
	BalanceRelThreshold  float64
	SessionEvictEpochs   uint64
	SessionAffinityBoost int64
	BlockMinThreshold    int64
	BlockEmergencyThresh int64
	WeightRequestLoad    float64
	WeightWaitingCount   float64
	WeightBlockPressure  float64
	WeightDrift          float64
}

// normBounds holds min/max values for score normalization.
type normBounds struct {
	minLoad       int64
	maxLoad       int64
	minWaiting    int64
	maxWaiting    int64
	minBlockPress float64
	maxBlockPress float64
	minDrift      int64
	maxDrift      int64
}

// NewSessionAwareV5Policy creates a SessionAwareV5Policy with given config.
// Zero-valued fields use sensible defaults.
func NewSessionAwareV5Policy(cfg SessionAwareV5Config) *SessionAwareV5Policy {
	// Apply defaults for SWE multi-round scenario.
	if cfg.MaxSessionLoad <= 0 {
		cfg.MaxSessionLoad = 32 // SWE default: max_num_seqs
	}
	if cfg.LoadDiffThreshold <= 0 {
		cfg.LoadDiffThreshold = 2
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
	if cfg.BlockMinThreshold <= 0 {
		cfg.BlockMinThreshold = 256
	}
	if cfg.BlockEmergencyThresh <= 0 {
		cfg.BlockEmergencyThresh = 128
	}
	// Default weights: 0.3 each, drift 0.1 (sum = 1.0).
	if cfg.WeightRequestLoad <= 0 {
		cfg.WeightRequestLoad = 0.3
	}
	if cfg.WeightWaitingCount <= 0 {
		cfg.WeightWaitingCount = 0.3
	}
	if cfg.WeightBlockPressure <= 0 {
		cfg.WeightBlockPressure = 0.3
	}
	if cfg.WeightDrift <= 0 {
		cfg.WeightDrift = 0.1
	}

	// Initialize atomic metadata with empty map.
	empty := make(v5InstanceMetaMap)
	p := &SessionAwareV5Policy{
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
		BlockMinThreshold:    cfg.BlockMinThreshold,
		BlockEmergencyThresh: cfg.BlockEmergencyThresh,
		WeightRequestLoad:    cfg.WeightRequestLoad,
		WeightWaitingCount:   cfg.WeightWaitingCount,
		WeightBlockPressure:  cfg.WeightBlockPressure,
		WeightDrift:          cfg.WeightDrift,
		branches:             newSessionAwareV5Branches(),
	}
	p.metaPtr.Store(&empty)
	return p
}

func (p *SessionAwareV5Policy) Name() Name { return NameSessionAwareV5 }

// ConfigSummary implements ConfigDescriber.
func (p *SessionAwareV5Policy) ConfigSummary() ConfigInfo {
	return ConfigInfo{MaxRequestLoad: p.MaxRequestLoad, MaxSessionLoad: p.MaxSessionLoad}
}

// Select picks best instance for a single request.
func (p *SessionAwareV5Policy) Select(_ context.Context, req *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	sessionID := req.SessionID
	if sessionID == "" {
		// Non-session request: route to min composite score.
		inst, err := p.selectMinCompositeScore(nodes)
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
				p.sessionAssign[sessionID] = sessionEntry{instanceID: scan.bestID, lastEpoch: epoch, requestCount: 0}
				p.requestLoad[scan.bestID]++
				p.branches.Imbalanced.Inc()
				return nodes[scan.bestID].Instance, nil
			}

			// Balanced mode: Check block emergency + warmth-boosted threshold.
			blockCritical := p.isBlockEmergency(entry.instanceID)
			effectiveThresh := p.effectiveThreshold(entry.requestCount)

			if scan.bestID == "" {
				return nil, ErrNoInstances
			}
			if lastLoad >= p.MaxSessionLoad || blockCritical {
				// Force migrate: overload or block critical.
				chosen := scan.bestID
				if blockCritical {
					chosen = p.bestEmergencyMigrationTargetLocked(nodes, entry.instanceID)
					if chosen == "" {
						return nil, ErrAllOverloaded
					}
				}
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
				if lastLoad >= p.MaxSessionLoad {
					p.branches.MigrateOverload.Inc()
				} else {
					p.branches.MigrateEmergency.Inc()
				}
				p.requestLoad[chosen]++
				return nodes[chosen].Instance, nil
			}

			// Compare composite scores.
			metaSnap := p.metaPtr.Load()
			lastScore, _ := p.calculateCompositeScore(metaSnap, entry.instanceID, lastLoad)
			bestScore, _ := p.calculateCompositeScore(metaSnap, scan.bestID, scan.bestLoad)

			// Migrate if score diff exceeds warmth-boosted threshold.
			if (lastScore - bestScore) > float64(effectiveThresh)*0.01 {
				chosen := scan.bestID
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
				p.branches.MigrateScoreDiff.Inc()
				p.requestLoad[chosen]++
				return nodes[chosen].Instance, nil
			}

			// Stay: session cache grows warmer.
			p.sessionAssign[sessionID] = sessionEntry{instanceID: entry.instanceID, lastEpoch: epoch, requestCount: entry.requestCount + 1}
			p.branches.Stay.Inc()
			p.requestLoad[entry.instanceID]++
			return ns.Instance, nil
		}
		// Stale mapping — instance gone. Batch invalidate.
		deadInstID := entry.instanceID
		if _, alreadyCleaned := p.deadInstCache[deadInstID]; !alreadyCleaned {
			// Note: invalidateInstanceSessionsLocked will handle sessionLoad and sessionCount cleanup.
			p.invalidateInstanceSessionsLocked(deadInstID)
			p.deadInstCache[deadInstID] = struct{}{}
		}
		p.branches.StaleReassign.Inc()
		// Fall through as new session (sessionCount already decremented).
	} else {
		p.branches.NewSession.Inc()
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

	// Check block gating for new sessions.
	if scan.bestID == "" {
		return nil, ErrNoInstances
	}
	metaSnap := p.metaPtr.Load()
	if meta, ok := (*metaSnap)[scan.bestID]; ok {
		if meta.availableBlocks.Load() < p.BlockMinThreshold {
			p.branches.BlockGated.Inc()
			// Try next best if available
			for id, ns := range nodes {
				if !ns.LoadAvailable() || id == scan.bestID {
					continue
				}
				load := p.requestLoad[id]
				if load >= p.MaxSessionLoad {
					continue
				}
				if m, ok := (*metaSnap)[id]; ok {
					if m.availableBlocks.Load() >= p.BlockMinThreshold {
						p.sessionAssign[sessionID] = sessionEntry{instanceID: id, lastEpoch: epoch, requestCount: 0}
						p.sessionCount++
						p.requestLoad[id]++
						return ns.Instance, nil
					}
				}
			}
			return nil, ErrAllExceedSession // All nodes gated
		}
	}

	p.sessionAssign[sessionID] = sessionEntry{instanceID: scan.bestID, lastEpoch: epoch, requestCount: 0}
	p.sessionCount++
	p.sessionLoad[scan.bestID]++
	metrics.InstanceSessions.WithLabelValues(scan.bestID).Inc()
	p.requestLoad[scan.bestID]++
	return nodes[scan.bestID].Instance, nil
}

// selectMinCompositeScore picks instance with lowest composite score.
func (p *SessionAwareV5Policy) selectMinCompositeScore(nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	metaSnap := p.metaPtr.Load()
	if metaSnap == nil {
		// Fallback to request load if no metadata.
		return p.selectMinRequestLoad(nodes)
	}

	bounds := p.calculateNormBounds(metaSnap, nodes)

	var best *domain.Instance
	bestScore := math.MaxFloat64

	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		if p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad {
			continue
		}
		active := ns.LoadActiveRequests()
		if active >= p.MaxSessionLoad {
			continue
		}

		meta, ok := (*metaSnap)[id]
		if !ok {
			// No metadata: use request load only.
			if float64(active) < bestScore {
				bestScore = float64(active)
				best = ns.Instance
			}
			continue
		}

		// Block gating: exclude nodes below threshold.
		if meta.availableBlocks.Load() < p.BlockMinThreshold {
			continue
		}

		score, eligible := p.calculateCompositeScoreWithBounds(metaSnap, id, active, bounds)
		if !eligible {
			continue
		}
		if score < bestScore {
			bestScore = score
			best = ns.Instance
		}
	}

	if best == nil {
		return nil, ErrAllOverloaded
	}
	p.requestLoad[best.ID]++
	return best, nil
}

// selectMinRequestLoad picks instance with fewest in-flight requests.
func (p *SessionAwareV5Policy) selectMinRequestLoad(nodes map[string]*domain.NodeState) (*domain.Instance, error) {
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
		if load < bestLoad && load < p.MaxSessionLoad {
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

// Feedback decrements request load for released instance.
func (p *SessionAwareV5Policy) Feedback(instanceID string, _ *domain.CostMetrics) {
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
func (p *SessionAwareV5Policy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.sessionAssign)
	clear(p.requestLoad)
	clear(p.sessionLoad)
	clear(p.deadInstCache)
	p.sessionCount = 0
	p.lastEvictionEpoch = 0
	empty := make(v5InstanceMetaMap)
	p.metaPtr.Store(&empty)
	// Keep resultsBuf, heapBuf, heapIndexBuf backing arrays for reuse.
}

// HasSession reports whether the given session is currently tracked.
func (p *SessionAwareV5Policy) HasSession(sessionID string) bool {
	p.mu.Lock()
	_, ok := p.sessionAssign[sessionID]
	p.mu.Unlock()
	return ok
}

// RemoveSession unbinds a session and decrements global session counter.
func (p *SessionAwareV5Policy) RemoveSession(sessionID string) {
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

// BatchSelect allocates a batch of requests in a single pass using a min-heap.
// Uses composite score for heap ordering and session affinity for STAY decisions.
//
// Complexity: O(N_nodes + K*log(N_nodes)).
//
// NOT safe for concurrent use. Must be called from the event-loop only.
func (p *SessionAwareV5Policy) BatchSelect(reqs []*domain.RouteContext, nodes map[string]*domain.NodeState) []BatchSelectResult {
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

	// Build heap by composite score.
	metaSnap := p.metaPtr.Load()
	bounds := p.calculateNormBounds(metaSnap, nodes)
	heap, heapIndex := p.buildHeapWithIndex(nodes, metaSnap, bounds)

	imbalanced := p.isImbalanced(scan.minLoad, scan.maxLoad)

	availableCount := int64(len(heap))

	var branchStay, branchMigrateOverload, branchMigrateScoreDiff, branchMigrateEmergency int
	var branchNewSession, branchStale, branchNoSession, branchBlockGated int

	for i, req := range reqs {
		sessionID := req.SessionID

		if sessionID == "" {
			// Non-session request: assign to min-score.
			if len(heap) == 0 {
				results[i].Err = ErrAllExceedSession
				continue
			}
			branchNoSession++
			results[i].Instance = heap[0].inst
			p.requestLoad[heap[0].instID]++
			heap[0].availableBlocks-- // Simulate decrement for next selection
			p.updateHeapEntry(heap, heapIndex, 0)
			continue
		}

		// Existing session mapping.
		if entry, ok := p.sessionAssign[sessionID]; ok {
			if ns, exists := nodes[entry.instanceID]; exists && ns.LoadAvailable() {
				lastLoad := p.requestLoad[entry.instanceID]
				blockCritical := p.isBlockEmergency(entry.instanceID)
				authOverload := p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad

				if imbalanced || authOverload {
					// Imbalanced mode: route to min-score.
					if len(heap) == 0 {
						results[i].Err = ErrAllExceedSession
						continue
					}
					// Update sessionLoad metrics for migration.
					if oldLoad, ok := p.sessionLoad[entry.instanceID]; ok {
						p.sessionLoad[entry.instanceID] = oldLoad - 1
						if p.sessionLoad[entry.instanceID] <= 0 {
							delete(p.sessionLoad, entry.instanceID)
						}
						metrics.InstanceSessions.WithLabelValues(entry.instanceID).Dec()
					}
					branchMigrateEmergency++
					chosen := heap[0].instID
					p.sessionLoad[chosen]++
					metrics.InstanceSessions.WithLabelValues(chosen).Inc()
					p.sessionAssign[sessionID] = sessionEntry{instanceID: chosen, lastEpoch: epoch, requestCount: 0}
					results[i].Instance = heap[0].inst
					p.requestLoad[chosen]++
					p.updateHeapEntry(heap, heapIndex, 0)
					continue
				}

				effectiveThresh := p.effectiveThreshold(entry.requestCount)

				// Check emergency migration condition.
				if lastLoad >= p.MaxSessionLoad || blockCritical {
					// Force migrate.
					if len(heap) == 0 {
						results[i].Err = ErrAllExceedSession
						continue
					}
					if lastLoad >= p.MaxSessionLoad {
						branchMigrateOverload++
					} else {
						branchMigrateEmergency++
					}
					chosen := heap[0].instID
					p.sessionAssign[sessionID] = sessionEntry{instanceID: chosen, lastEpoch: epoch, requestCount: 0}
					results[i].Instance = heap[0].inst
					p.requestLoad[chosen]++
					p.updateHeapEntry(heap, heapIndex, 0)
					continue
				}

				// Compare scores for migration decision.
				lastScore, _ := p.calculateCompositeScoreWithBounds(metaSnap, entry.instanceID, lastLoad, bounds)
				bestScore, _ := p.calculateCompositeScoreWithBounds(metaSnap, heap[0].instID, p.requestLoad[heap[0].instID], bounds)

				// Migrate if score diff exceeds threshold.
				if (lastScore - bestScore) > float64(effectiveThresh)*0.01 {
					if len(heap) == 0 {
						results[i].Err = ErrAllExceedSession
						continue
					}
					branchMigrateScoreDiff++
					chosen := heap[0].instID
					p.sessionAssign[sessionID] = sessionEntry{instanceID: chosen, lastEpoch: epoch, requestCount: 0}
					results[i].Instance = heap[0].inst
					p.requestLoad[chosen]++
					p.updateHeapEntry(heap, heapIndex, 0)
					continue
				}

				// Stay: session cache grows warmer.
				branchStay++
				results[i].Instance = ns.Instance
				p.requestLoad[entry.instanceID]++
				p.sessionAssign[sessionID] = sessionEntry{instanceID: entry.instanceID, lastEpoch: epoch, requestCount: entry.requestCount + 1}

				// Update heap entry for this instance to maintain accuracy.
				if idx, inHeap := heapIndex[entry.instanceID]; inHeap {
					heap = p.updateHeapEntryAtIndex(heap, heapIndex, idx, 1)
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
			// Fall through as new session.
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

		// Block gating for new sessions.
		if meta, ok := (*metaSnap)[heap[0].instID]; ok {
			if meta.availableBlocks.Load() < p.BlockMinThreshold {
				branchBlockGated++
				// Find next eligible node
				heap = p.removeAndFindNextGated(heap, heapIndex, metaSnap, bounds)
				if len(heap) == 0 {
					results[i].Err = ErrAllExceedSession
					continue
				}
			}
		}

		chosen := heap[0].instID
		if _, isNew := p.sessionAssign[sessionID]; !isNew {
			p.sessionCount++
		}
		p.sessionAssign[sessionID] = sessionEntry{instanceID: chosen, lastEpoch: epoch, requestCount: 0}
		results[i].Instance = heap[0].inst
		p.requestLoad[chosen]++
		p.updateHeapEntry(heap, heapIndex, 0)
	}

	p.maybeEvictSessionsLocked(epoch)
	p.flushBranchCounters(branchStay, branchMigrateOverload, branchMigrateScoreDiff, branchMigrateEmergency, branchNewSession, branchStale, branchNoSession, branchBlockGated)

	p.heapBuf = heap
	return results
}

// flushBranchCounters writes accumulated branch counters to Prometheus.
func (p *SessionAwareV5Policy) flushBranchCounters(stay, migrateOverload, migrateScoreDiff, migrateEmergency, newSession, stale, noSession, blockGated int) {
	if stay > 0 {
		p.branches.Stay.Add(float64(stay))
	}
	if migrateOverload > 0 {
		p.branches.MigrateOverload.Add(float64(migrateOverload))
	}
	if migrateScoreDiff > 0 {
		p.branches.MigrateScoreDiff.Add(float64(migrateScoreDiff))
	}
	if migrateEmergency > 0 {
		p.branches.MigrateEmergency.Add(float64(migrateEmergency))
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
	if blockGated > 0 {
		p.branches.BlockGated.Add(float64(blockGated))
	}
}

// updateHeapEntry updates heap[0] after selection and sifts down.
func (p *SessionAwareV5Policy) updateHeapEntry(heap []v5NodeEntry, heapIndex map[string]int, idx int) {
	heap[idx].availableBlocks--
	v5HeapDownIdx(heap, idx, len(heap), heapIndex)
}

// updateHeapEntryAtIndex updates heap entry at specific position.
func (p *SessionAwareV5Policy) updateHeapEntryAtIndex(heap []v5NodeEntry, heapIndex map[string]int, idx int, loadDelta int64) []v5NodeEntry {
	heap[idx].availableBlocks -= int64(loadDelta)
	if heap[idx].availableBlocks < 0 {
		p.removeHeapAt(&heap, idx, heapIndex)
	} else {
		v5HeapDownIdx(heap, idx, len(heap), heapIndex)
		v5HeapUpIdx(heap, idx, heapIndex)
	}
	return heap
}

// removeAndFindNextGated removes gated node and finds next eligible.
func (p *SessionAwareV5Policy) removeAndFindNextGated(heap []v5NodeEntry, heapIndex map[string]int, metaSnap *v5InstanceMetaMap, bounds *normBounds) []v5NodeEntry {
	p.removeHeapTop(&heap, heapIndex)
	// Remove blocked nodes until we find an eligible one.
	for len(heap) > 0 {
		meta, ok := (*metaSnap)[heap[0].instID]
		if !ok || meta.availableBlocks.Load() >= p.BlockMinThreshold {
			break
		}
		p.removeHeapTop(&heap, heapIndex)
	}
	return heap
}

// ---------- internal helpers ----------

// effectiveThreshold returns the warmth-boosted migration threshold.
// Warmer sessions (higher requestCount) tolerate more load imbalance
// before migrating.
func (p *SessionAwareV5Policy) effectiveThreshold(requestCount uint32) int64 {
	boost := min(int64(requestCount), p.SessionAffinityBoost)
	return p.LoadDiffThreshold + boost
}

// scanNodesLocked computes min/max requestLoad, available count, and
// best (min-load, below MaxSessionLoad) instance in a single O(N) pass.
func (p *SessionAwareV5Policy) scanNodesLocked(nodes map[string]*domain.NodeState) loadSummary {
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
func (p *SessionAwareV5Policy) isImbalanced(minLoad, maxLoad int64) bool {
	return (maxLoad-minLoad) > p.BalanceAbsThreshold &&
		float64(maxLoad) > float64(minLoad)*p.BalanceRelThreshold
}

// isBlockEmergency checks if an instance has critical block shortage.
func (p *SessionAwareV5Policy) isBlockEmergency(instanceID string) bool {
	metaSnap := p.metaPtr.Load()
	if metaSnap == nil {
		return false
	}
	if meta, ok := (*metaSnap)[instanceID]; ok {
		return meta.availableBlocks.Load() <= p.BlockEmergencyThresh
	}
	return false
}

// bestEmergencyMigrationTargetLocked picks a non-critical destination for a
// session whose current instance is below BlockEmergencyThresh. This path is
// rare and intentionally scans nodes once instead of complicating the normal
// new-session fast path.
func (p *SessionAwareV5Policy) bestEmergencyMigrationTargetLocked(nodes map[string]*domain.NodeState, currentID string) string {
	metaSnap := p.metaPtr.Load()
	bestID := ""
	bestLoad := int64(math.MaxInt64)
	for id, ns := range nodes {
		if id == currentID || !ns.LoadAvailable() {
			continue
		}
		load := p.requestLoad[id]
		if load >= p.MaxSessionLoad {
			continue
		}
		if !p.hasEnoughBlocksForMigration(metaSnap, id) {
			continue
		}
		if load < bestLoad || (load == bestLoad && (bestID == "" || id < bestID)) {
			bestID = id
			bestLoad = load
		}
	}
	return bestID
}

func (p *SessionAwareV5Policy) hasEnoughBlocksForMigration(metaSnap *v5InstanceMetaMap, instID string) bool {
	if metaSnap == nil {
		return true
	}
	meta, ok := (*metaSnap)[instID]
	if !ok {
		return true
	}
	return meta.availableBlocks.Load() >= p.BlockMinThreshold
}

// invalidateInstanceSessionsLocked sweeps all sessions bound to instanceID
// and removes them. Decrements sessionCount for each removed session.
// Decrements sessionLoad by the total number of removed sessions once.
func (p *SessionAwareV5Policy) invalidateInstanceSessionsLocked(instanceID string) {
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
func (p *SessionAwareV5Policy) maybeEvictSessionsLocked(epoch uint64) {
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

// ---------- composite score calculation ----------

// calculateCompositeScore computes a multi-dimensional load score for a node.
// Lower is better. Returns (score, eligible).
func (p *SessionAwareV5Policy) calculateCompositeScore(metaMap *v5InstanceMetaMap, instID string, active int64) (float64, bool) {
	if metaMap == nil {
		return float64(active), true
	}
	meta, ok := (*metaMap)[instID]
	if !ok {
		return float64(active), true
	}

	waiting := max(int64(0), meta.waitingCount.Load()+meta.localDrift.Load())
	avail := meta.availableBlocks.Load()
	total := meta.totalBlocks.Load()

	// Normalize to [0, 1] range.
	normLoad := float64(active) / float64(p.MaxSessionLoad)
	normWaiting := math.Min(float64(waiting)/float64(p.MaxSessionLoad), 1.0)

	var normBlockPress float64
	if total > 0 {
		normBlockPress = 1.0 - float64(avail)/float64(total)
	} else {
		normBlockPress = 0.0 // Safe fallback
	}

	normDrift := math.Min(float64(max(0, meta.localDrift.Load()))/float64(p.MaxSessionLoad), 1.0)

	score := p.WeightRequestLoad*normLoad +
		p.WeightWaitingCount*normWaiting +
		p.WeightBlockPressure*normBlockPress +
		p.WeightDrift*normDrift

	eligible := avail > p.BlockEmergencyThresh
	return score, eligible
}

// calculateCompositeScoreWithBounds computes score with pre-calculated bounds for O(1) normalization.
func (p *SessionAwareV5Policy) calculateCompositeScoreWithBounds(metaMap *v5InstanceMetaMap, instID string, active int64, bounds *normBounds) (float64, bool) {
	if metaMap == nil {
		return float64(active), true
	}
	meta, ok := (*metaMap)[instID]
	if !ok {
		return float64(active), true
	}

	waiting := max(int64(0), meta.waitingCount.Load()+meta.localDrift.Load())
	avail := meta.availableBlocks.Load()
	total := meta.totalBlocks.Load()

	var normLoad, normWaiting, normBlockPress, normDrift float64

	// Normalize with bounds to avoid division by zero.
	if bounds.maxLoad > bounds.minLoad {
		normLoad = float64(active-bounds.minLoad) / float64(bounds.maxLoad-bounds.minLoad)
	} else {
		normLoad = 0.0
	}

	if bounds.maxWaiting > bounds.minWaiting {
		normWaiting = float64(waiting-bounds.minWaiting) / float64(bounds.maxWaiting-bounds.minWaiting)
	} else {
		normWaiting = 0.0
	}

	var blockPress float64
	if total > 0 {
		blockPress = 1.0 - float64(avail)/float64(total)
	}
	if bounds.maxBlockPress > bounds.minBlockPress {
		normBlockPress = (blockPress - bounds.minBlockPress) / (bounds.maxBlockPress - bounds.minBlockPress)
	} else {
		normBlockPress = 0.0
	}

	if bounds.maxDrift > bounds.minDrift {
		normDrift = float64(max(0, meta.localDrift.Load())-bounds.minDrift) / float64(bounds.maxDrift-bounds.minDrift)
	} else {
		normDrift = 0.0
	}

	score := p.WeightRequestLoad*normLoad +
		p.WeightWaitingCount*normWaiting +
		p.WeightBlockPressure*normBlockPress +
		p.WeightDrift*normDrift

	eligible := avail > p.BlockEmergencyThresh
	return score, eligible
}

// calculateNormBounds computes min/max values for score normalization in a single pass.
func (p *SessionAwareV5Policy) calculateNormBounds(metaMap *v5InstanceMetaMap, nodes map[string]*domain.NodeState) *normBounds {
	b := normBounds{
		minLoad:       math.MaxInt64,
		maxLoad:       math.MinInt64,
		minWaiting:    math.MaxInt64,
		maxWaiting:    math.MinInt64,
		minBlockPress: math.MaxFloat64,
		maxBlockPress: -math.MaxFloat64,
		minDrift:      math.MaxInt64,
		maxDrift:      math.MinInt64,
	}

	for id := range nodes {
		if !nodes[id].LoadAvailable() {
			continue
		}

		active := nodes[id].LoadActiveRequests()

		if active < b.minLoad {
			b.minLoad = active
		}
		if active > b.maxLoad {
			b.maxLoad = active
		}

		meta, ok := (*metaMap)[id]
		if !ok {
			continue
		}

		waiting := meta.waitingCount.Load() + meta.localDrift.Load()
		if waiting < b.minWaiting {
			b.minWaiting = waiting
		}
		if waiting > b.maxWaiting {
			b.maxWaiting = waiting
		}

		total := meta.totalBlocks.Load()
		if total > 0 {
			avail := meta.availableBlocks.Load()
			blockPress := 1.0 - float64(avail)/float64(total)
			if blockPress < b.minBlockPress {
				b.minBlockPress = blockPress
			}
			if blockPress > b.maxBlockPress {
				b.maxBlockPress = blockPress
			}
		}

		drift := meta.localDrift.Load()
		if drift < b.minDrift {
			b.minDrift = drift
		}
		if drift > b.maxDrift {
			b.maxDrift = drift
		}
	}

	return &b
}

// ---------- heap operations with index tracking ----------

// buildHeap constructs a min-heap from available nodes by composite score.
func (p *SessionAwareV5Policy) buildHeap(nodes map[string]*domain.NodeState, metaMap *v5InstanceMetaMap, bounds *normBounds) ([]v5NodeEntry, map[string]int) {
	if cap(p.heapBuf) >= len(nodes) {
		p.heapBuf = p.heapBuf[:0]
	} else {
		p.heapBuf = make([]v5NodeEntry, 0, len(nodes))
	}
	heap := p.heapBuf

	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		if p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad {
			continue
		}
		active := p.requestLoad[id]
		if active >= p.MaxSessionLoad {
			continue
		}

		meta, ok := (*metaMap)[id]
		if !ok {
			// No metadata: use request load as score.
			heap = append(heap, v5NodeEntry{
				instID:         id,
				inst:           ns.Instance,
				compositeScore: float64(active),
			})
			continue
		}

		// Block gating for heap construction.
		if meta.availableBlocks.Load() < p.BlockMinThreshold {
			continue
		}

		score, _ := p.calculateCompositeScoreWithBounds(metaMap, id, active, bounds)
		heap = append(heap, v5NodeEntry{
			instID:          id,
			inst:            ns.Instance,
			compositeScore:  score,
			availableBlocks: meta.availableBlocks.Load(),
			totalBlocks:     meta.totalBlocks.Load(),
			waitingCount:    meta.waitingCount.Load(),
			drift:           meta.localDrift.Load(),
		})
	}
	v5HeapInit(heap)

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

// buildHeapWithIndex constructs a min-heap with an instance_id -> position index.
func (p *SessionAwareV5Policy) buildHeapWithIndex(nodes map[string]*domain.NodeState, metaMap *v5InstanceMetaMap, bounds *normBounds) ([]v5NodeEntry, map[string]int) {
	heap, idx := p.buildHeap(nodes, metaMap, bounds)
	return heap, idx
}

// removeHeapTop removes the top element from the heap and updates the index.
func (p *SessionAwareV5Policy) removeHeapTop(heap *[]v5NodeEntry, idx map[string]int) {
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
	v5HeapDownIdx(*heap, 0, n-1, idx)
}

// removeHeapAt removes element at position i from the heap and updates the index.
func (p *SessionAwareV5Policy) removeHeapAt(heap *[]v5NodeEntry, i int, idx map[string]int) {
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
	v5HeapDownIdx(*heap, i, n-1, idx)
	v5HeapUpIdx(*heap, i, idx)
}

// ---------- inline min-heap for v5NodeEntry ----------
// Avoids container/heap's interface{} boxing overhead entirely.

func v5HeapInit(h []v5NodeEntry) {
	n := len(h)
	for i := n/2 - 1; i >= 0; i-- {
		v5HeapDown(h, i, n)
	}
}

func v5HeapDown(h []v5NodeEntry, i, n int) {
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		j := left
		if right := left + 1; right < n && h[right].compositeScore < h[left].compositeScore {
			j = right
		}
		if h[i].compositeScore <= h[j].compositeScore {
			break
		}
		h[i], h[j] = h[j], h[i]
		i = j
	}
}

func v5HeapDownIdx(h []v5NodeEntry, i, n int, idx map[string]int) {
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		j := left
		if right := left + 1; right < n && h[right].compositeScore < h[left].compositeScore {
			j = right
		}
		if h[i].compositeScore <= h[j].compositeScore {
			break
		}
		h[i], h[j] = h[j], h[i]
		idx[h[i].instID] = i
		idx[h[j].instID] = j
		i = j
	}
}

func v5HeapUpIdx(h []v5NodeEntry, i int, idx map[string]int) {
	for i > 0 {
		parent := (i - 1) / 2
		if h[parent].compositeScore <= h[i].compositeScore {
			break
		}
		h[i], h[parent] = h[parent], h[i]
		idx[h[i].instID] = i
		idx[h[parent].instID] = parent
		i = parent
	}
}

// ---------- MetaUpdater implementation (from MinLoad pattern) ----------

// BatchUpdateInstanceMeta applies a batch of load metadata updates.
// High-frequency path: atomic in-place writes, zero allocation.
// Low-frequency path: COW swap to add new keys.
// Implements MetaUpdater interface.
func (p *SessionAwareV5Policy) BatchUpdateInstanceMeta(updates []InstanceMetaUpdate) {
	curPtr := p.metaPtr.Load()
	if curPtr == nil {
		p.cowSwapWithUpdates(updates)
		return
	}
	cur := *curPtr

	// Fast check: are there any new instances not in the current map?
	needRebuild := false
	for i := range updates {
		if _, ok := cur[updates[i].InstanceID]; !ok {
			needRebuild = true
			break
		}
	}

	if needRebuild {
		// Low-frequency path: COW swap to add new keys.
		p.cowSwapWithUpdates(updates)
		return
	}

	// High-frequency path: atomic in-place writes, zero allocation.
	for i := range updates {
		u := &updates[i]
		if meta, ok := cur[u.InstanceID]; ok {
			meta.waitingCount.Store(u.WaitingCount)
			meta.availableBlocks.Store(u.AvailableBlocks)
			if u.TotalBlocks > 0 {
				meta.totalBlocks.Store(u.TotalBlocks)
			}
			meta.localDrift.Store(0) // metrics refresh -> calibration reset
		}
	}
}

// cowSwapWithUpdates performs a COW clone of the current meta map, applies updates,
// and atomically swaps the pointer. Called only when the map structure changes
// (new instance IDs) - low frequency.
func (p *SessionAwareV5Policy) cowSwapWithUpdates(updates []InstanceMetaUpdate) {
	oldPtr := p.metaPtr.Load()
	size := len(updates)
	if oldPtr != nil && len(*oldPtr) > size {
		size = len(*oldPtr)
	}
	newMeta := make(v5InstanceMetaMap, size)

	// Copy existing entries (shared pointers — fields are atomic, safe for concurrent access).
	if oldPtr != nil {
		maps.Copy(newMeta, *oldPtr)
	}

	// Add new entries + update values.
	for i := range updates {
		u := &updates[i]
		if meta, ok := newMeta[u.InstanceID]; ok {
			meta.waitingCount.Store(u.WaitingCount)
			meta.availableBlocks.Store(u.AvailableBlocks)
			if u.TotalBlocks > 0 {
				meta.totalBlocks.Store(u.TotalBlocks)
			}
			meta.localDrift.Store(0)
		} else {
			m := &v5InstanceMeta{}
			m.waitingCount.Store(u.WaitingCount)
			m.availableBlocks.Store(u.AvailableBlocks)
			if u.TotalBlocks > 0 {
				m.totalBlocks.Store(u.TotalBlocks)
			}
			// localDrift defaults to 0
			newMeta[u.InstanceID] = m
		}
	}

	p.metaPtr.Store(&newMeta) // atomic swap
}

// ---------- DriftAware implementation (from MinLoad pattern) ----------

// TrackAcquire increments the local drift counter for the given instance.
// Called from the scheduler event-loop after each allocation.
// Implements DriftAware.
func (p *SessionAwareV5Policy) TrackAcquire(instanceID string) {
	if metaMap := p.metaPtr.Load(); metaMap != nil {
		if meta, ok := (*metaMap)[instanceID]; ok {
			meta.localDrift.Add(1)
		}
	}
}

// TrackRelease decrements the local drift counter for the given instance.
// Called from the scheduler event-loop after each release.
// Implements DriftAware.
func (p *SessionAwareV5Policy) TrackRelease(instanceID string) {
	if metaMap := p.metaPtr.Load(); metaMap != nil {
		if meta, ok := (*metaMap)[instanceID]; ok {
			meta.localDrift.Add(-1)
		}
	}
}

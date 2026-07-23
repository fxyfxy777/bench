package policy

import (
	"context"
	"math"
	"sync"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/metrics"
)

// SessionAwareV3Policy implements dynamic session-aware scheduling.
// Unlike SessionAwarePolicy (v2) which strongly binds a session to one instance,
// v3 decouples the binding: each request dynamically decides whether to STAY on
// the last-used instance or MIGRATE to a lower-load instance, based on real-time
// request-level load (requestLoad) rather than session-level load (sessionLoad).
//
// This is especially effective in Mooncake (global KV-Cache) scenarios where
// session migration cost is low (L3 RDMA fetch instead of full recomputation),
// making aggressive load rebalancing more beneficial.
//
// Algorithm (per-request CompareAndSchedule):
//   - If lastInstance.requestLoad >= MaxSessionLoad → migrate
//   - If (lastInstance.requestLoad - minLoad) > LoadDiffThreshold → migrate
//   - Otherwise → stay (cache affinity preserved)
//
// Implements Policy, BatchSelector, Resettable, SessionRemover.
type SessionAwareV3Policy struct {
	mu sync.Mutex

	// sessionAssign maps session_id → lastInstanceID.
	// NOT a permanent binding — just remembers the last instance used.
	sessionAssign map[string]string

	// requestLoad maps instance_id → current in-flight request count.
	// Incremented on Select/BatchSelect, decremented on Feedback (release).
	requestLoad map[string]int64

	// sessionLoad maps instance_id → number of active sessions bound to it.
	// Used for session-level metrics tracking.
	sessionLoad map[string]int64

	// sessionCount tracks global active session count for admission control.
	sessionCount int64

	// MaxSessionLoad is the per-instance request load cap.
	// Also used for global admission: maxSessions = MaxSessionLoad * instanceCount.
	// Default: 100.
	MaxSessionLoad int64

	// MaxRequestLoad is the per-instance active-request cap (authoritative counter).
	// 0 = no cap.
	MaxRequestLoad int64

	// LoadDiffThreshold controls migration sensitivity.
	// If (lastInstance.load - minLoad) > LoadDiffThreshold, migrate to minLoad instance.
	// Default: 1.
	LoadDiffThreshold int64

	// Pre-resolved branch counters for observability.
	branches *SessionAwareV3Branches

	// Reusable buffers for BatchSelect — only accessed from the event-loop.
	resultsBuf []BatchSelectResult
	heapBuf    []v3NodeEntry
}

func NewSessionAwareV3Policy(maxSessionLoad, loadDiffThreshold int64) *SessionAwareV3Policy {
	if maxSessionLoad <= 0 {
		maxSessionLoad = 100
	}
	if loadDiffThreshold <= 0 {
		loadDiffThreshold = 1
	}
	return &SessionAwareV3Policy{
		sessionAssign:     make(map[string]string),
		requestLoad:       make(map[string]int64),
		sessionLoad:       make(map[string]int64),
		MaxSessionLoad:    maxSessionLoad,
		LoadDiffThreshold: loadDiffThreshold,
		branches:          newSessionAwareV3Branches(),
	}
}

func (p *SessionAwareV3Policy) Name() Name { return NameSessionAwareV3 }

// ConfigSummary implements ConfigDescriber.
func (p *SessionAwareV3Policy) ConfigSummary() ConfigInfo {
	return ConfigInfo{MaxRequestLoad: p.MaxRequestLoad, MaxSessionLoad: p.MaxSessionLoad}
}

func (p *SessionAwareV3Policy) Select(_ context.Context, req *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	sessionID := req.SessionID
	if sessionID == "" {
		// No session affinity — pick instance with lowest requestLoad.
		inst, err := p.selectMinRequestLoad(nodes)
		if err == nil {
			p.branches.NoSessionFallback.Inc()
		}
		return inst, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 1. Subsequent request: session has a previous mapping.
	if lastInstID, ok := p.sessionAssign[sessionID]; ok {
		if ns, exists := nodes[lastInstID]; exists && ns.LoadAvailable() {
			// MaxRequestLoad gate: force migrate if at authoritative cap.
			authOverload := p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad

			// Instance still alive — run CompareAndSchedule.
			lastLoad := p.requestLoad[lastInstID]

			// Find the globally minimum-load instance.
			minInstID, minLoad, found := p.findMinLoadLocked(nodes)
			if !found {
				if authOverload {
					return nil, ErrAllOverloaded
				}
				return nil, ErrNoInstances
			}

			chosen := lastInstID
			if authOverload || lastLoad >= p.MaxSessionLoad {
				// Overloaded — force migrate.
				chosen = minInstID
				p.branches.MigrateOverload.Inc()
			} else if (lastLoad - minLoad) > p.LoadDiffThreshold {
				// Load imbalance — migrate to min-load instance.
				chosen = minInstID
				p.branches.MigrateLoadDiff.Inc()
			} else {
				// Stay on lastInstance (cache affinity).
				p.branches.Stay.Inc()
			}

			// Update sessionLoad for migration cases.
			if chosen != lastInstID {
				if oldLoad, ok := p.sessionLoad[lastInstID]; ok {
					p.sessionLoad[lastInstID] = oldLoad - 1
					if p.sessionLoad[lastInstID] <= 0 {
						delete(p.sessionLoad, lastInstID)
					}
					metrics.InstanceSessions.WithLabelValues(lastInstID).Dec()
				}
				p.sessionLoad[chosen]++
				metrics.InstanceSessions.WithLabelValues(chosen).Inc()
			}

			p.sessionAssign[sessionID] = chosen
			p.requestLoad[chosen]++
			return nodes[chosen].Instance, nil
		}
		// Stale mapping — instance gone. Clean up and fall through as new session.
		delete(p.sessionAssign, sessionID)
		if oldLoad, ok := p.sessionLoad[lastInstID]; ok {
			p.sessionLoad[lastInstID] = oldLoad - 1
			if p.sessionLoad[lastInstID] <= 0 {
				delete(p.sessionLoad, lastInstID)
			}
			metrics.InstanceSessions.WithLabelValues(lastInstID).Dec()
		}
		// sessionCount stays: this session was already counted.
		p.branches.StaleReassign.Inc()
	}

	// 2. New session (or stale mapping cleaned up above).
	availableCount := p.countAvailable(nodes)
	if availableCount == 0 {
		return nil, ErrNoInstances
	}

	// Admission control: global session cap.
	maxSessions := p.MaxSessionLoad * int64(availableCount)
	if p.sessionCount >= maxSessions {
		return nil, ErrAllExceedSession
	}

	minInstID, _, found := p.findMinLoadLocked(nodes)
	if !found {
		return nil, ErrNoInstances
	}

	p.sessionAssign[sessionID] = minInstID
	p.sessionCount++
	p.requestLoad[minInstID]++
	p.sessionLoad[minInstID]++
	metrics.InstanceSessions.WithLabelValues(minInstID).Inc()
	p.branches.NewSession.Inc()
	return nodes[minInstID].Instance, nil
}

// selectMinRequestLoad picks the instance with the fewest in-flight requests.
// Used for non-session requests (empty SessionID). Reads requestLoad under lock.
func (p *SessionAwareV3Policy) selectMinRequestLoad(nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var (
		best        *domain.Instance
		bestLoad    int64 = math.MaxInt64
		anyAvail    bool
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

// findMinLoadLocked scans all available nodes to find the one with the lowest
// requestLoad that is below MaxSessionLoad. Caller must hold p.mu.
func (p *SessionAwareV3Policy) findMinLoadLocked(nodes map[string]*domain.NodeState) (string, int64, bool) {
	var (
		bestID   string
		bestLoad int64 = math.MaxInt64
	)
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
		if load < bestLoad {
			bestLoad = load
			bestID = id
		}
	}
	return bestID, bestLoad, bestID != ""
}

// countAvailable returns the number of nodes that are LoadAvailable.
func (p *SessionAwareV3Policy) countAvailable(nodes map[string]*domain.NodeState) int {
	count := 0
	for _, ns := range nodes {
		if ns.LoadAvailable() {
			count++
		}
	}
	return count
}

// Feedback is called by the scheduler event-loop on every handleRelease.
// It decrements the request load counter for the given instance.
func (p *SessionAwareV3Policy) Feedback(instanceID string, _ *domain.CostMetrics) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.requestLoad[instanceID] > 0 {
		p.requestLoad[instanceID]--
		if p.requestLoad[instanceID] == 0 {
			delete(p.requestLoad, instanceID)
		}
	}
}

// Reset clears all session mappings and request load counters.
// Implements policy.Resettable. Called between training steps.
func (p *SessionAwareV3Policy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.sessionAssign)
	clear(p.requestLoad)
	clear(p.sessionLoad)
	p.sessionCount = 0
	// Keep resultsBuf, heapBuf backing arrays for reuse.
}

// HasSession reports whether the given session is currently tracked.
func (p *SessionAwareV3Policy) HasSession(sessionID string) bool {
	p.mu.Lock()
	_, ok := p.sessionAssign[sessionID]
	p.mu.Unlock()
	return ok
}

// RemoveSession unbinds a session and decrements the global session counter.
// Does NOT touch requestLoad — in-flight requests continue and will be
// decremented naturally via Feedback on release.
func (p *SessionAwareV3Policy) RemoveSession(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	instID, ok := p.sessionAssign[sessionID]
	if !ok {
		return
	}
	delete(p.sessionAssign, sessionID)
	if p.sessionCount > 0 {
		p.sessionCount--
	}
	// Decrement sessionLoad for the instance.
	if oldLoad, ok := p.sessionLoad[instID]; ok {
		p.sessionLoad[instID] = oldLoad - 1
		if p.sessionLoad[instID] <= 0 {
			delete(p.sessionLoad, instID)
		}
		metrics.InstanceSessions.WithLabelValues(instID).Dec()
	}
}

// ---------- BatchSelector implementation ----------

// v3NodeEntry is a heap element for BatchSelect assignment.
type v3NodeEntry struct {
	instID string
	inst   *domain.Instance
	load   int64
}

// BatchSelect allocates a batch of requests using a min-heap over requestLoad.
//
// For each request with a session mapping, CompareAndSchedule decides whether
// to stay on the last instance or migrate to the global minimum.
//
// Total complexity: O(N + K*logN).
//
// NOT safe for concurrent use. Must be called from the event-loop only.
// The returned slice is backed by an internal buffer; callers must finish
// reading results before the next BatchSelect call.
func (p *SessionAwareV3Policy) BatchSelect(reqs []*domain.RouteContext, nodes map[string]*domain.NodeState) []BatchSelectResult {
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

	// Build min-heap by requestLoad for all available nodes.
	if cap(p.heapBuf) >= len(nodes) {
		p.heapBuf = p.heapBuf[:0]
	} else {
		p.heapBuf = make([]v3NodeEntry, 0, len(nodes))
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
		heap = append(heap, v3NodeEntry{
			instID: id,
			inst:   ns.Instance,
			load:   load,
		})
	}
	v3HeapInit(heap)

	availableCount := int64(len(heap))

	// Branch counters — accumulate locally, flush once after loop.
	var branchStay, branchMigrateOverload, branchMigrateLoadDiff int
	var branchNewSession, branchStale, branchNoSession int

	for i, req := range reqs {
		sessionID := req.SessionID

		if sessionID == "" {
			// Non-session request: assign to min-load, no session tracking.
			if len(heap) == 0 {
				results[i].Err = ErrAllExceedSession
				continue
			}
			branchNoSession++
			results[i].Instance = heap[0].inst
			p.requestLoad[heap[0].instID]++
			heap[0].load++
			if heap[0].load >= p.MaxSessionLoad {
				heap[0] = heap[len(heap)-1]
				heap = heap[:len(heap)-1]
				if len(heap) > 0 {
					v3HeapDown(heap, 0, len(heap))
				}
			} else {
				v3HeapDown(heap, 0, len(heap))
			}
			continue
		}

		// Check existing session mapping.
		if lastInstID, ok := p.sessionAssign[sessionID]; ok {
			if ns, exists := nodes[lastInstID]; exists && ns.LoadAvailable() {
				// CompareAndSchedule: compare lastInstance load with heap min.
				lastLoad := p.requestLoad[lastInstID]
				authOverload := p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad

				if len(heap) > 0 && (authOverload || lastLoad >= p.MaxSessionLoad || (lastLoad-heap[0].load) > p.LoadDiffThreshold) {
					// Migrate to min-load instance.
					if authOverload || lastLoad >= p.MaxSessionLoad {
						branchMigrateOverload++
					} else {
						branchMigrateLoadDiff++
					}
					chosen := heap[0].instID
					// Update sessionLoad for migration.
					if oldLoad, ok := p.sessionLoad[lastInstID]; ok {
						p.sessionLoad[lastInstID] = oldLoad - 1
						if p.sessionLoad[lastInstID] <= 0 {
							delete(p.sessionLoad, lastInstID)
						}
						metrics.InstanceSessions.WithLabelValues(lastInstID).Dec()
					}
					p.sessionLoad[chosen]++
					metrics.InstanceSessions.WithLabelValues(chosen).Inc()
					p.sessionAssign[sessionID] = chosen
					results[i].Instance = heap[0].inst
					p.requestLoad[chosen]++
					heap[0].load++
					if heap[0].load >= p.MaxSessionLoad {
						heap[0] = heap[len(heap)-1]
						heap = heap[:len(heap)-1]
						if len(heap) > 0 {
							v3HeapDown(heap, 0, len(heap))
						}
					} else {
						v3HeapDown(heap, 0, len(heap))
					}
				} else {
					// Stay on lastInstance (cache affinity).
					branchStay++
					results[i].Instance = ns.Instance
					p.requestLoad[lastInstID]++
					// Heap entry for lastInstance becomes stale — acceptable:
					// future comparisons may slightly underestimate min,
					// leading to more aggressive migration (safe bias).
				}
				continue
			}
			// Stale mapping — instance gone. Clean up.
			branchStale++
			delete(p.sessionAssign, sessionID)
			// Update sessionLoad for stale instance.
			if oldLoad, ok := p.sessionLoad[lastInstID]; ok {
				p.sessionLoad[lastInstID] = oldLoad - 1
				if p.sessionLoad[lastInstID] <= 0 {
					delete(p.sessionLoad, lastInstID)
				}
				metrics.InstanceSessions.WithLabelValues(lastInstID).Dec()
			}
			// sessionCount stays: session was already counted, will be removed via RemoveSession.
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
		p.sessionAssign[sessionID] = chosen
		p.sessionCount++
		p.sessionLoad[chosen]++
		metrics.InstanceSessions.WithLabelValues(chosen).Inc()
		results[i].Instance = heap[0].inst
		p.requestLoad[chosen]++
		heap[0].load++
		if heap[0].load >= p.MaxSessionLoad {
			heap[0] = heap[len(heap)-1]
			heap = heap[:len(heap)-1]
			if len(heap) > 0 {
				v3HeapDown(heap, 0, len(heap))
			}
		} else {
			v3HeapDown(heap, 0, len(heap))
		}
	}

	p.heapBuf = heap
	p.flushBranchCounters(branchStay, branchMigrateOverload, branchMigrateLoadDiff, branchNewSession, branchStale, branchNoSession)
	return results
}

// flushBranchCounters writes accumulated batch branch counts to Prometheus in one shot.
func (p *SessionAwareV3Policy) flushBranchCounters(stay, migrateOverload, migrateLoadDiff, newSession, stale, noSession int) {
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

// ---------- inline min-heap for v3NodeEntry ----------
// NOT unified via generics — benchmarked and generic version showed ~80% regression.

func v3HeapInit(h []v3NodeEntry) {
	n := len(h)
	for i := n/2 - 1; i >= 0; i-- {
		v3HeapDown(h, i, n)
	}
}

func v3HeapDown(h []v3NodeEntry, i, n int) {
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

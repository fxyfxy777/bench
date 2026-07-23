package policy

import (
	"context"
	"math"
	"sync"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/metrics"
)

// SessionAwarePolicy implements session-affinity scheduling.
// The same session_id is routed to the same instance to maximise KV-Cache hit rate.
// New sessions are assigned to the instance with the fewest active sessions.
// All state is held in-memory, replacing the original Redis HSET/ZSET approach.
//
// Implements both Policy and BatchSelector interfaces.
type SessionAwarePolicy struct {
	mu sync.Mutex

	// sessionAssign maps session_id -> instance_id
	sessionAssign map[string]string

	// sessionLoad maps instance_id -> number of active sessions bound to it
	sessionLoad map[string]int64

	MaxSessionLoad int64
	MaxRequestLoad int64 // per-instance active-request cap (authoritative); 0 = no cap

	// Pre-resolved branch counters for observability.
	branches *SessionAwareBranches

	// Reusable buffers for BatchSelect to avoid per-batch allocation.
	// Only accessed from the single-threaded event-loop.
	resultsBuf     []BatchSelectResult
	missIndicesBuf []int
	heapBuf        []sessionNodeEntry
}

func NewSessionAwarePolicy(maxSessionLoad int64) *SessionAwarePolicy {
	if maxSessionLoad <= 0 {
		maxSessionLoad = 100
	}
	return &SessionAwarePolicy{
		sessionAssign:  make(map[string]string),
		sessionLoad:    make(map[string]int64),
		MaxSessionLoad: maxSessionLoad,
		branches:       newSessionAwareBranches(),
	}
}

func (p *SessionAwarePolicy) Name() Name { return NameSessionAware }

// ConfigSummary implements ConfigDescriber.
func (p *SessionAwarePolicy) ConfigSummary() ConfigInfo {
	return ConfigInfo{MaxRequestLoad: p.MaxRequestLoad, MaxSessionLoad: p.MaxSessionLoad}
}

func (p *SessionAwarePolicy) Select(_ context.Context, req *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	sessionID := req.SessionID
	if sessionID == "" {
		// No session affinity requested; fall back to min-session-load selection.
		inst, err := p.selectMinLoad(nodes)
		if err == nil {
			p.branches.NoSessionFallback.Inc()
		}
		return inst, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 1. Check existing assignment (cache hit).
	stale := false
	if instID, ok := p.sessionAssign[sessionID]; ok {
		if ns, exists := nodes[instID]; exists {
			if p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad {
				// Instance at hard cap — fall through to reassignment.
			} else {
				p.branches.Stay.Inc()
				return ns.Instance, nil
			}
		}
		// Instance gone; clean up stale mapping.
		delete(p.sessionAssign, sessionID)
		p.sessionLoad[instID]--
		if p.sessionLoad[instID] <= 0 {
			delete(p.sessionLoad, instID)
		}
		metrics.InstanceSessions.WithLabelValues(instID).Dec()
		stale = true
	}

	// 2. Cache miss — assign to instance with fewest sessions.
	var (
		bestID   string
		bestLoad int64 = math.MaxInt64
	)
	for id := range nodes {
		if !nodes[id].LoadAvailable() {
			continue
		}
		if p.MaxRequestLoad > 0 && nodes[id].LoadActiveRequests() >= p.MaxRequestLoad {
			continue
		}
		load := p.sessionLoad[id] // zero if not present
		if load < bestLoad {
			bestLoad = load
			bestID = id
		}
	}

	if bestID == "" {
		return nil, ErrNoInstances
	}
	if bestLoad >= p.MaxSessionLoad {
		return nil, ErrAllExceedSession
	}

	p.sessionAssign[sessionID] = bestID
	p.sessionLoad[bestID]++
	metrics.InstanceSessions.WithLabelValues(bestID).Inc()
	if stale {
		p.branches.StaleReassign.Inc()
	} else {
		p.branches.NewSession.Inc()
	}
	return nodes[bestID].Instance, nil
}

// selectMinLoad picks the instance with the fewest active requests.
// Called only from the single-threaded event-loop. Does NOT access
// sessionAssign or sessionLoad, so no lock is needed — it only reads
// the COW nodes snapshot passed by the caller.
func (p *SessionAwarePolicy) selectMinLoad(nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	var (
		best     *domain.Instance
		bestLoad int64 = math.MaxInt64
		anyAvail bool
	)
	for _, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		anyAvail = true
		if p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad {
			continue
		}

		active := ns.LoadActiveRequests()
		inst := ns.Instance

		if active < bestLoad {
			bestLoad = active
			best = inst
		}
	}
	if best == nil {
		if anyAvail {
			return nil, ErrAllOverloaded
		}
		return nil, ErrNoInstances
	}
	return best, nil
}

func (p *SessionAwarePolicy) Feedback(_ string, _ *domain.CostMetrics) {
	// no-op
}

// Reset clears all session mappings. Implements policy.Resettable.
// Called between training steps to ensure state isolation.
func (p *SessionAwarePolicy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.sessionAssign)
	clear(p.sessionLoad)
	// Keep resultsBuf, missIndicesBuf, heapBuf backing arrays for reuse.
}

// HasSession reports whether the given session is currently tracked.
func (p *SessionAwarePolicy) HasSession(sessionID string) bool {
	p.mu.Lock()
	_, ok := p.sessionAssign[sessionID]
	p.mu.Unlock()
	return ok
}

// RemoveSession unbinds a session and decrements the session load counter.
// Should be called when a session terminates.
func (p *SessionAwarePolicy) RemoveSession(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	instID, ok := p.sessionAssign[sessionID]
	if !ok {
		return
	}
	delete(p.sessionAssign, sessionID)
	p.sessionLoad[instID]--
	metrics.InstanceSessions.WithLabelValues(instID).Dec()
	if p.sessionLoad[instID] <= 0 {
		delete(p.sessionLoad, instID)
	}
}

// ---------- BatchSelector implementation ----------

// sessionNodeEntry is a heap element for BatchSelect miss-phase assignment.
type sessionNodeEntry struct {
	instID string
	inst   *domain.Instance
	load   int64
}

// BatchSelect allocates a batch of requests in two phases:
//
//  1. Cache hits: O(1) per request via sessionAssign lookup.
//  2. Cache misses: build a min-heap by sessionLoad, assign misses in O(N + K_miss * logN).
//
// Total complexity: O(N + K_miss * logN) instead of O(K * N) for sequential Select.
//
// NOT safe for concurrent use. Must be called from the event-loop only.
// The returned slice is backed by an internal buffer; callers must finish
// reading results before the next BatchSelect call.
func (p *SessionAwarePolicy) BatchSelect(reqs []*domain.RouteContext, nodes map[string]*domain.NodeState) []BatchSelectResult {
	// Reuse resultsBuf — safe because only the event-loop calls BatchSelect.
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

	// Lock protects sessionAssign/sessionLoad against concurrent Reset/RemoveSession.
	// resultsBuf/missIndicesBuf/heapBuf reuse is outside lock scope (event-loop only).
	p.mu.Lock()
	defer p.mu.Unlock()

	// Phase 1: resolve cache hits and collect miss indices.
	// Reuse missIndicesBuf to avoid per-batch allocation (~64KB for 8192 reqs).
	if cap(p.missIndicesBuf) >= len(reqs) {
		p.missIndicesBuf = p.missIndicesBuf[:0]
	} else {
		p.missIndicesBuf = make([]int, 0, len(reqs))
	}
	missIndices := p.missIndicesBuf

	// Branch counters — accumulate locally, flush once after loop.
	var branchStay, branchNewSession, branchStale, branchNoSession int

	for i, req := range reqs {
		sessionID := req.SessionID
		if sessionID == "" {
			// No session: treat as miss for min-load assignment.
			branchNoSession++
			missIndices = append(missIndices, i)
			continue
		}

		// Check existing assignment.
		if instID, ok := p.sessionAssign[sessionID]; ok {
			if ns, exists := nodes[instID]; exists {
				if p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad {
					// Instance at hard cap — treat as miss for reassignment.
				} else {
					results[i].Instance = ns.Instance
					branchStay++
					continue
				}
			}
			// Stale assignment: clean up.
			delete(p.sessionAssign, sessionID)
			p.sessionLoad[instID]--
			metrics.InstanceSessions.WithLabelValues(instID).Dec()
			if p.sessionLoad[instID] <= 0 {
				delete(p.sessionLoad, instID)
			}
			branchStale++
		} else {
			branchNewSession++
		}
		missIndices = append(missIndices, i)
	}

	if len(missIndices) == 0 {
		p.missIndicesBuf = missIndices // save back in case header changed
		p.flushBranchCounters(branchStay, branchNewSession, branchStale, branchNoSession)
		return results
	}

	// Phase 2: build min-heap by sessionLoad for miss assignment.
	// Reuse heapBuf to avoid per-batch allocation (~320KB for 10K nodes).
	if cap(p.heapBuf) >= len(nodes) {
		p.heapBuf = p.heapBuf[:0]
	} else {
		p.heapBuf = make([]sessionNodeEntry, 0, len(nodes))
	}
	heap := p.heapBuf
	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		if p.MaxRequestLoad > 0 && ns.LoadActiveRequests() >= p.MaxRequestLoad {
			continue
		}
		load := p.sessionLoad[id]
		if load >= p.MaxSessionLoad {
			continue
		}
		heap = append(heap, sessionNodeEntry{
			instID: id,
			inst:   ns.Instance,
			load:   load,
		})
	}
	sessionHeapInit(heap)

	// Assign each miss to the node with the fewest sessions.
	for _, idx := range missIndices {
		if len(heap) == 0 {
			results[idx].Err = ErrAllExceedSession
			continue
		}

		top := &heap[0]
		results[idx].Instance = top.inst

		// Update session state: only for session-bearing requests.
		// Non-session requests do NOT consume session slots: they need no sessionAssign
		// entry and must not increment sessionLoad (either persistent or heap-local).
		// If we incremented top.load for non-session requests, the heap would evict
		// nodes that hit MaxSessionLoad purely due to non-session traffic, and the
		// persistent sessionLoad would never be decremented (no RemoveSession key).
		sessionID := reqs[idx].SessionID
		if sessionID != "" {
			p.sessionAssign[sessionID] = top.instID
			top.load++
			p.sessionLoad[top.instID] = top.load
			metrics.InstanceSessions.WithLabelValues(top.instID).Inc()
		}
		// Non-session requests: no load update — they don't occupy session slots.

		if top.load >= p.MaxSessionLoad {
			// Remove from heap.
			heap[0] = heap[len(heap)-1]
			heap = heap[:len(heap)-1]
			if len(heap) > 0 {
				sessionHeapDown(heap, 0, len(heap))
			}
		} else {
			sessionHeapDown(heap, 0, len(heap))
		}
	}

	// Save back buffer headers for reuse in next batch.
	p.heapBuf = heap
	p.missIndicesBuf = missIndices

	p.flushBranchCounters(branchStay, branchNewSession, branchStale, branchNoSession)
	return results
}

// flushBranchCounters writes accumulated batch branch counts to Prometheus in one shot.
func (p *SessionAwarePolicy) flushBranchCounters(stay, newSession, stale, noSession int) {
	if stay > 0 {
		p.branches.Stay.Add(float64(stay))
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

// ---------- inline min-heap for sessionNodeEntry ----------
// NOT unified via generics — benchmarked and generic version showed ~80% regression
// due to interface method call overhead in the hot loop (even with inlining).

func sessionHeapInit(h []sessionNodeEntry) {
	n := len(h)
	for i := n/2 - 1; i >= 0; i-- {
		sessionHeapDown(h, i, n)
	}
}

func sessionHeapDown(h []sessionNodeEntry, i, n int) {
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

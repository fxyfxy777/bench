package policy

import (
	"context"
	"maps"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"github.com/yzx/rl-router/internal/domain"
)

const (
	// p2cThreshold is the minimum node count to use P2C sampling.
	// Below this, full scan is cheaper because the sampling overhead
	// (2 random numbers + 2 map lookups) exceeds linear iteration.
	p2cThreshold = 16

	// p2cMaxRetries limits consecutive P2C sampling attempts before
	// falling back to full scan (all sampled nodes may be over limit).
	p2cMaxRetries = 16
)

const (
	defaultWaitingCoefficient int64 = 10000
	defaultBlockSize          int64 = 64
)

// MinLoadPolicy selects the instance with the lowest composite load score.
// score = ActiveRequests + WaitingCount × WaitingCoefficient
//
// For single-request Select(), uses Power-of-2-Choices (P2C) when len(nodes) > p2cThreshold,
// achieving O(1) per-call instead of O(N). P2C's load-balancing quality is O(log(log(N))),
// which is excellent for 10K+ nodes.
//
// Instance load metadata is stored via atomic.Pointer (COW for map structure changes)
// with atomic.Int64 fields (in-place writes for high-frequency value updates).
// This hybrid approach gives the event-loop zero-lock reads (~5ns/instance) while
// the MetricsCollector writes at 500ms intervals without blocking scheduling.
type MinLoadPolicy struct {
	// mu protects cachedIDs and generation fields against concurrent Reset/SetGeneration.
	// instanceMeta is fully atomic (metaPtr) and does NOT need this lock.
	mu                 sync.RWMutex
	MaxRequestLoad     int64
	WaitingCoefficient int64
	BlockSize          int64

	// metaPtr holds per-instance load metadata via atomic.Pointer for lock-free reads.
	// Map structure changes (add/remove key) use COW swap (low frequency: instance registration).
	// Field value updates use atomic.Store on instanceLoadMeta fields (high frequency: every 500ms).
	// Event-loop reads via atomic.Pointer.Load + atomic.Int64.Load — zero lock, zero alloc.
	metaPtr atomic.Pointer[instanceMetaMap]

	// cachedIDs is a slice of node IDs for P2C random-index sampling.
	// Rebuilt when the node-set generation changes. Protected by mu (RLock for reads
	// in Select, Lock for writes in Reset/EnsureCachedIDs/SetGeneration).
	cachedIDs         []string
	cachedGeneration  uint64
	currentGeneration uint64

	// resultsBuf and heapBuf are reused across BatchSelect calls to avoid per-call allocation.
	// Only accessed from the event-loop (single-threaded).
	resultsBuf []BatchSelectResult
	heapBuf    []nodeEntry
}

// instanceLoadMeta holds per-instance load metadata with atomic fields.
// Field values are updated at high frequency (every 2s) via atomic.Store
// from the MetricsCollector goroutine, while the event-loop reads them via
// atomic.Load — zero lock contention on the hot path.
type instanceLoadMeta struct {
	waitingCount    atomic.Int64
	availableBlocks atomic.Int64
	avgIOLength     atomic.Int64
	localDrift      atomic.Int64 // Acquire +1, Release -1, metrics refresh resets to 0
}

// instanceMetaMap is the type stored in the atomic.Pointer for COW access.
type instanceMetaMap = map[string]*instanceLoadMeta

func NewMinLoadPolicy() *MinLoadPolicy {
	p := &MinLoadPolicy{
		MaxRequestLoad:     defaultMaxRequestLoad,
		WaitingCoefficient: defaultWaitingCoefficient,
		BlockSize:          defaultBlockSize,
	}
	empty := make(instanceMetaMap)
	p.metaPtr.Store(&empty)
	return p
}

func (p *MinLoadPolicy) Name() Name { return NameMinLoad }

// ConfigSummary implements ConfigDescriber.
func (p *MinLoadPolicy) ConfigSummary() ConfigInfo {
	return ConfigInfo{MaxRequestLoad: p.MaxRequestLoad}
}

func (p *MinLoadPolicy) calculateScore(activeRequests int64, waitingCount int64) int64 {
	return activeRequests + waitingCount*p.WaitingCoefficient
}

func (p *MinLoadPolicy) calcConcurrencyLimit(availableBlocks, avgIOLength int64) int64 {
	if availableBlocks <= 0 {
		return 0
	}
	if avgIOLength <= 0 {
		return p.MaxRequestLoad
	}
	limit := (availableBlocks * p.BlockSize) / avgIOLength
	if limit > p.MaxRequestLoad {
		return p.MaxRequestLoad
	}
	return limit
}

func (p *MinLoadPolicy) Select(_ context.Context, _ *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	n := len(nodes)
	if n == 0 {
		return nil, ErrNoInstances
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	// P2C requires ≥2 nodes and a valid cachedIDs (same generation as nodes).
	// cachedIDs/generation are protected by mu.RLock against concurrent Reset/SetGeneration.
	if n > p2cThreshold && p.currentGeneration == p.cachedGeneration && len(p.cachedIDs) >= 2 {
		return p.selectP2C(nodes)
	}
	return p.selectFullScan(nodes)
}

// SetGeneration updates the current node-set generation.
// Called by the scheduler event-loop before each batch.
// Implements GenerationAware.
func (p *MinLoadPolicy) SetGeneration(gen uint64) {
	p.mu.Lock()
	p.currentGeneration = gen
	p.mu.Unlock()
}

// EnsureCachedIDs rebuilds the P2C sampling slice from node map keys
// if the generation has changed since the last rebuild.
// Should be called from the event-loop before Select is invoked with a new
// node set to ensure the P2C fast path is available.
// Exported only for testing — not part of the Policy interface.
func (p *MinLoadPolicy) EnsureCachedIDs(nodes map[string]*domain.NodeState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.currentGeneration != p.cachedGeneration || len(p.cachedIDs) == 0 {
		p.rebuildCachedIDs(nodes)
	}
}

// selectP2C uses Power-of-2-Choices: randomly sample 2 nodes, pick the one
// with the lower score. O(1) per call. Falls back to full scan if all sampled
// nodes are over limit after p2cMaxRetries attempts.
//
// Precondition: cachedIDs is valid and len(cachedIDs) >= 2.
func (p *MinLoadPolicy) selectP2C(nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	ids := p.cachedIDs
	n := len(ids)

	for range p2cMaxRetries {
		i := rand.IntN(n)
		j := rand.IntN(n - 1)
		if j >= i {
			j++ // ensure i != j
		}

		id1 := ids[i]
		id2 := ids[j]

		ns1 := nodes[id1]
		ns2 := nodes[id2]
		if ns1 == nil || ns2 == nil {
			// Node removed between cachedIDs rebuild and now; full scan.
			return p.selectFullScan(nodes)
		}

		score1, ok1 := p.scoreNode(id1, ns1)
		score2, ok2 := p.scoreNode(id2, ns2)

		if ok1 && ok2 {
			if score1 <= score2 {
				return ns1.Instance, nil
			}
			return ns2.Instance, nil
		}
		if ok1 {
			return ns1.Instance, nil
		}
		if ok2 {
			return ns2.Instance, nil
		}
		// Both over limit — retry with new random pair.
	}

	// All retries exhausted — fall back to full scan as last resort.
	return p.selectFullScan(nodes)
}

// selectFullScan iterates all nodes to find the one with the lowest score.
// O(N). Used for small node sets or as P2C fallback.
func (p *MinLoadPolicy) selectFullScan(nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	var (
		best      *domain.Instance
		bestScore int64 = math.MaxInt64
	)

	for id, ns := range nodes {
		score, ok := p.scoreNode(id, ns)
		if !ok {
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
	return best, nil
}

// scoreNode computes the composite score for a single node and returns whether
// the node is eligible (within concurrency and MaxRequestLoad limits).
// Shared by P2C and full-scan paths.
// Reads metaPtr via atomic.Pointer.Load + atomic.Int64.Load — zero lock.
func (p *MinLoadPolicy) scoreNode(id string, ns *domain.NodeState) (int64, bool) {
	if !ns.LoadAvailable() {
		return 0, false
	}

	active := ns.LoadActiveRequests()

	var waitingCount int64
	metaMap := p.metaPtr.Load() // atomic load, ~1ns
	if metaMap != nil {
		if meta, ok := (*metaMap)[id]; ok {
			polledWaiting := meta.waitingCount.Load()
			drift := meta.localDrift.Load()
			waitingCount = max(int64(0), polledWaiting+drift)
			limit := p.calcConcurrencyLimit(meta.availableBlocks.Load(), meta.avgIOLength.Load())
			if limit > 0 && active >= limit {
				return 0, false
			}
		}
	}

	score := p.calculateScore(active, waitingCount)
	if score > p.MaxRequestLoad {
		return 0, false
	}
	return score, true
}

// rebuildCachedIDs rebuilds the P2C sampling slice from node map keys.
// Caller must hold p.mu (write lock). Called from EnsureCachedIDs.
func (p *MinLoadPolicy) rebuildCachedIDs(nodes map[string]*domain.NodeState) {
	if cap(p.cachedIDs) >= len(nodes) {
		p.cachedIDs = p.cachedIDs[:0]
	} else {
		p.cachedIDs = make([]string, 0, len(nodes))
	}
	for id := range nodes {
		p.cachedIDs = append(p.cachedIDs, id)
	}
	p.cachedGeneration = p.currentGeneration
}

// Feedback receives post-request metrics. For MinLoadPolicy, it currently is a no-op.
// Use UpdateInstanceMeta to feed waiting/block metrics from the backend.
func (p *MinLoadPolicy) Feedback(_ string, _ *domain.CostMetrics) {}

// TrackAcquire increments the local drift counter for the given instance.
// Called from the scheduler event-loop after each allocation.
// Implements DriftAware.
func (p *MinLoadPolicy) TrackAcquire(instanceID string) {
	if metaMap := p.metaPtr.Load(); metaMap != nil {
		if meta, ok := (*metaMap)[instanceID]; ok {
			meta.localDrift.Add(1)
		}
	}
}

// TrackRelease decrements the local drift counter for the given instance.
// Called from the scheduler event-loop after each release.
// Implements DriftAware.
func (p *MinLoadPolicy) TrackRelease(instanceID string) {
	if metaMap := p.metaPtr.Load(); metaMap != nil {
		if meta, ok := (*metaMap)[instanceID]; ok {
			meta.localDrift.Add(-1)
		}
	}
}

// UpdateInstanceMeta updates the waiting-count and block-availability metadata
// for a specific instance. Delegates to BatchUpdateInstanceMeta.
func (p *MinLoadPolicy) UpdateInstanceMeta(instanceID string, waitingCount, availableBlocks, avgIOLength int64) {
	p.BatchUpdateInstanceMeta([]InstanceMetaUpdate{{
		InstanceID:      instanceID,
		WaitingCount:    waitingCount,
		AvailableBlocks: availableBlocks,
		AvgIOLength:     avgIOLength,
	}})
}

// BatchUpdateInstanceMeta applies a batch of load metadata updates.
// High-frequency path (every 500ms): if all instance IDs exist in the current map,
// updates are performed via atomic.Store on existing entries — zero allocation, zero lock.
// Low-frequency path (new instance): performs a COW swap to add new map entries.
// Implements MetaUpdater interface.
func (p *MinLoadPolicy) BatchUpdateInstanceMeta(updates []InstanceMetaUpdate) {
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
			meta.avgIOLength.Store(u.AvgIOLength)
			meta.localDrift.Store(0) // metrics refresh → calibration reset
		}
	}
}

// cowSwapWithUpdates performs a COW clone of the current meta map, applies updates,
// and atomically swaps the pointer. Called only when the map structure changes
// (new instance IDs) — low frequency.
func (p *MinLoadPolicy) cowSwapWithUpdates(updates []InstanceMetaUpdate) {
	oldPtr := p.metaPtr.Load()
	size := len(updates)
	if oldPtr != nil && len(*oldPtr) > size {
		size = len(*oldPtr)
	}
	newMeta := make(instanceMetaMap, size)

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
			meta.avgIOLength.Store(u.AvgIOLength)
			meta.localDrift.Store(0) // metrics refresh → calibration reset
		} else {
			m := &instanceLoadMeta{}
			m.waitingCount.Store(u.WaitingCount)
			m.availableBlocks.Store(u.AvailableBlocks)
			m.avgIOLength.Store(u.AvgIOLength)
			// localDrift defaults to 0
			newMeta[u.InstanceID] = m
		}
	}

	p.metaPtr.Store(&newMeta) // atomic swap
}

// BatchSelect allocates a batch of requests in a single pass using a min-heap.
// Uses value-type nodeEntry slice with inline heap operations to avoid per-node
// pointer allocations and interface{} boxing from container/heap.
// Complexity: O(N_nodes + K*log(N_nodes)) instead of O(K*N_nodes).
//
// NOT safe for concurrent use. Must be called from the event-loop only.
// The returned slice is backed by an internal buffer; callers must finish
// reading results before the next BatchSelect call.
func (p *MinLoadPolicy) BatchSelect(reqs []*domain.RouteContext, nodes map[string]*domain.NodeState) []BatchSelectResult {
	// Reuse resultsBuf to avoid per-batch allocation (~192KB for 8192 reqs).
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

	// Snapshot the meta map pointer once for the entire batch — atomic load, ~1ns.
	var metaSnap instanceMetaMap
	if ptr := p.metaPtr.Load(); ptr != nil {
		metaSnap = *ptr
	}

	// Reuse heap buffer from struct field to avoid per-batch allocation.
	h := p.heapBuf[:0]
	if cap(h) < len(nodes) {
		h = make([]nodeEntry, 0, len(nodes))
	}
	for id, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}

		active := ns.LoadActiveRequests()
		inst := ns.Instance

		var waitingCount int64
		var concurrencyLimit int64
		if meta, ok := metaSnap[id]; ok {
			polledWaiting := meta.waitingCount.Load()
			drift := meta.localDrift.Load()
			waitingCount = max(int64(0), polledWaiting+drift)
			concurrencyLimit = p.calcConcurrencyLimit(meta.availableBlocks.Load(), meta.avgIOLength.Load())
			if concurrencyLimit > 0 && active >= concurrencyLimit {
				continue
			}
		}

		score := p.calculateScore(active, waitingCount)
		if score > p.MaxRequestLoad {
			continue
		}

		h = append(h, nodeEntry{
			inst:             inst,
			score:            score,
			active:           active,
			concurrencyLimit: concurrencyLimit,
			waitingCount:     waitingCount,
		})
	}
	heapInit(h)

	// Assign each request to the current lowest-score node: O(K*log(N_nodes)).
	// Uses peek-and-fix pattern: update h[0] in place and sift down,
	// avoiding the Pop+Push overhead and interface{} boxing.
	for i := range reqs {
		if len(h) == 0 {
			results[i].Err = ErrAllOverloaded
			continue
		}

		results[i].Instance = h[0].inst

		// Simulate Acquire locally: update score for next iteration.
		h[0].active++
		h[0].score = p.calculateScore(h[0].active, h[0].waitingCount)

		withinScore := h[0].score <= p.MaxRequestLoad
		withinConcurrency := h[0].concurrencyLimit == 0 || h[0].active < h[0].concurrencyLimit
		if withinScore && withinConcurrency {
			// Still eligible: sift down to restore heap property.
			heapDown(h, 0, len(h))
		} else {
			// Exceeded limit: remove from heap by swapping with last element.
			h[0] = h[len(h)-1]
			h = h[:len(h)-1]
			if len(h) > 0 {
				heapDown(h, 0, len(h))
			}
		}
	}

	// Save back heap buffer header for reuse in next batch.
	p.heapBuf = h

	return results
}

// Reset clears per-instance metadata and P2C caches. Implements policy.Resettable.
// Called from the event-loop at step transitions. The atomic.Pointer swap ensures
// any in-flight Collector writes to the old map are harmless (atomic field writes
// to a map that will be GC'd).
func (p *MinLoadPolicy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	empty := make(instanceMetaMap)
	p.metaPtr.Store(&empty) // atomic swap
	p.cachedIDs = p.cachedIDs[:0]
	p.cachedGeneration = 0
	// Keep resultsBuf backing array for reuse.
}

// ---------- inline min-heap operations on []nodeEntry ----------
// Avoids container/heap's interface{} boxing overhead entirely.
// NOT unified via generics — benchmarked and generic version showed ~2x regression
// due to interface method call overhead in the hot loop (even with inlining).

type nodeEntry struct {
	inst             *domain.Instance
	score            int64
	active           int64
	concurrencyLimit int64
	waitingCount     int64
}

// heapInit establishes the heap invariant on h. O(n).
func heapInit(h []nodeEntry) {
	n := len(h)
	for i := n/2 - 1; i >= 0; i-- {
		heapDown(h, i, n)
	}
}

// heapDown sifts element at index i down to restore the min-heap property.
func heapDown(h []nodeEntry, i, n int) {
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		j := left
		if right := left + 1; right < n && h[right].score < h[left].score {
			j = right
		}
		if h[i].score <= h[j].score {
			break
		}
		h[i], h[j] = h[j], h[i]
		i = j
	}
}

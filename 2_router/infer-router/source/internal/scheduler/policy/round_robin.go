package policy

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/yzx/rl-router/internal/domain"
)

const defaultMaxRequestLoad int64 = 128

// RoundRobinPolicy selects instances in a round-robin fashion,
// skipping any instance whose ActiveRequests >= MaxRequestLoad.
//
// The sorted node list is cached and rebuilt only when the node-set generation
// changes (tracked by NodeStateStore), eliminating the O(N*logN) sort overhead
// on every Select call.
type RoundRobinPolicy struct {
	mu           sync.Mutex
	index        int
	MaxRequestLoad int64

	// Cached sorted node list — rebuilt when currentGeneration != cachedGeneration.
	cachedSorted     []*domain.NodeState
	cachedGeneration uint64

	// currentGeneration is set by the scheduler via SetGeneration before each batch.
	currentGeneration uint64
}

func NewRoundRobinPolicy() *RoundRobinPolicy {
	return &RoundRobinPolicy{
		MaxRequestLoad: defaultMaxRequestLoad,
	}
}

func (p *RoundRobinPolicy) Name() Name { return NameRoundRobin }

// ConfigSummary implements ConfigDescriber.
func (p *RoundRobinPolicy) ConfigSummary() ConfigInfo {
	return ConfigInfo{MaxRequestLoad: p.MaxRequestLoad}
}

// SetGeneration updates the current node-set generation.
// Called by the scheduler event-loop before each Select/BatchSelect batch.
// Implements GenerationAware.
func (p *RoundRobinPolicy) SetGeneration(gen uint64) {
	p.mu.Lock()
	p.currentGeneration = gen
	p.mu.Unlock()
}

func (p *RoundRobinPolicy) Select(_ context.Context, _ *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Rebuild sorted list when node set has changed (generation mismatch)
	// or cache is empty (first call / after Reset).
	if p.currentGeneration != p.cachedGeneration || len(p.cachedSorted) == 0 {
		p.rebuildSorted(nodes)
	}

	n := len(p.cachedSorted)
	for i := range n {
		idx := (p.index + i) % n
		ns := p.cachedSorted[idx]

		if !ns.LoadAvailable() {
			continue
		}

		active := ns.LoadActiveRequests()

		if active >= p.MaxRequestLoad {
			continue
		}

		p.index = (idx + 1) % n
		return ns.Instance, nil
	}

	return nil, ErrAllOverloaded
}

// rebuildSorted rebuilds the cached sorted node list from the given map.
// Caller must hold p.mu.
func (p *RoundRobinPolicy) rebuildSorted(nodes map[string]*domain.NodeState) {
	if cap(p.cachedSorted) >= len(nodes) {
		p.cachedSorted = p.cachedSorted[:0]
	} else {
		p.cachedSorted = make([]*domain.NodeState, 0, len(nodes))
	}
	for _, ns := range nodes {
		p.cachedSorted = append(p.cachedSorted, ns)
	}
	slices.SortFunc(p.cachedSorted, func(a, b *domain.NodeState) int {
		return cmp.Compare(a.Instance.ID, b.Instance.ID)
	})
	p.cachedGeneration = p.currentGeneration
	if p.index >= len(p.cachedSorted) {
		p.index = 0
	}
}

// Reset clears the cached sorted list and resets the round-robin index.
// Implements the Resettable interface for step transitions.
func (p *RoundRobinPolicy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.index = 0
	p.cachedSorted = p.cachedSorted[:0]
	p.cachedGeneration = 0
}

func (p *RoundRobinPolicy) Feedback(_ string, _ *domain.CostMetrics) {
	// no-op
}

package policy

import (
	"context"
	"math"

	"github.com/yzx/rl-router/internal/domain"
)

// ProcessTokensPolicy selects the instance with the fewest tokens being processed.
// Uses splitwise.CounterManager for per-instance token tracking via atomic counters,
// mirroring the splitwise processTokensSelect algorithm.
// On Select, tokens are estimated from RequestText and tracked internally;
// on Feedback, the tracked amount is released (FIFO).
// Both Select and Feedback are called from the event-loop only (single-threaded).
type ProcessTokensPolicy struct {
	counterMgr     *CounterManager
	pendingTokens  map[string][]uint64 // instanceID → FIFO of estimated token counts (event-loop only)
	MaxRequestLoad int64
}

func NewProcessTokensPolicy() *ProcessTokensPolicy {
	return &ProcessTokensPolicy{
		counterMgr:     NewCounterManager(),
		pendingTokens:  make(map[string][]uint64),
		MaxRequestLoad: defaultMaxRequestLoad,
	}
}

func (p *ProcessTokensPolicy) Name() Name { return NameProcessTokens }

func (p *ProcessTokensPolicy) Select(_ context.Context, req *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	var (
		best      *domain.Instance
		minTokens uint64 = math.MaxUint64
	)
	for _, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		tc := p.counterMgr.GetOrCreateTokenCounter(ns.Instance.ID)
		load := tc.Get()
		if load < minTokens {
			minTokens = load
			best = ns.Instance
		}
	}

	if best == nil {
		return nil, ErrNoInstances
	}

	var tokens uint64
	if len(req.RequestTokenIDs) > 0 {
		tokens = uint64(len(req.RequestTokenIDs))
	} else {
		tokens = EstimateTokens(req.RequestText)
	}
	if tokens > 0 {
		p.counterMgr.GetOrCreateTokenCounter(best.ID).Add(tokens)
	}
	p.pendingTokens[best.ID] = append(p.pendingTokens[best.ID], tokens)

	return best, nil
}

func (p *ProcessTokensPolicy) Feedback(instanceID string, _ *domain.CostMetrics) {
	// Release the estimated token count for the oldest pending request (FIFO).
	if q := p.pendingTokens[instanceID]; len(q) > 0 {
		tokens := q[0]
		p.pendingTokens[instanceID] = q[1:]
		if len(p.pendingTokens[instanceID]) == 0 {
			delete(p.pendingTokens, instanceID)
		}
		if tokens > 0 {
			p.counterMgr.GetOrCreateTokenCounter(instanceID).Sub(tokens)
		}
	}
}

// Reset clears all state. Implements Resettable for step isolation.
func (p *ProcessTokensPolicy) Reset() {
	p.counterMgr = NewCounterManager()
	clear(p.pendingTokens)
}

// RequestNumPolicy selects the instance with the fewest concurrent requests.
// Uses splitwise.CounterManager for per-instance request tracking via atomic counters,
// mirroring the splitwise requestNumSelect algorithm.
type RequestNumPolicy struct {
	counterMgr     *CounterManager
	MaxRequestLoad int64
}

func NewRequestNumPolicy() *RequestNumPolicy {
	return &RequestNumPolicy{
		counterMgr:     NewCounterManager(),
		MaxRequestLoad: defaultMaxRequestLoad,
	}
}

func (p *RequestNumPolicy) Name() Name { return NameRequestNum }

func (p *RequestNumPolicy) Select(_ context.Context, _ *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	var (
		best       *domain.Instance
		minCount   uint64 = math.MaxUint64
		hasHealthy bool
	)
	for _, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		hasHealthy = true
		c := p.counterMgr.GetOrCreateCounter(ns.Instance.ID)
		load := c.Get()
		if load >= uint64(p.MaxRequestLoad) {
			continue
		}
		if load < minCount {
			minCount = load
			best = ns.Instance
		}
	}

	if best == nil {
		if !hasHealthy {
			return nil, ErrNoInstances
		}
		// All healthy instances are at or above MaxRequestLoad.
		return nil, ErrAllOverloaded
	}

	p.counterMgr.GetOrCreateCounter(best.ID).Inc()
	return best, nil
}

func (p *RequestNumPolicy) Feedback(instanceID string, _ *domain.CostMetrics) {
	p.counterMgr.Release(instanceID)
}

// Reset clears all state. Implements Resettable for step isolation.
func (p *RequestNumPolicy) Reset() {
	p.counterMgr = NewCounterManager()
}

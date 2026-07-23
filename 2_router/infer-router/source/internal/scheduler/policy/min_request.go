package policy

import (
	"context"
	"math"

	"github.com/yzx/rl-router/internal/domain"
)

// MinRequestPolicy selects the instance with the fewest active requests.
// This is the in-memory equivalent of the Redis ZSet-based MinRequest strategy:
// it strictly picks the node with the smallest ActiveRequests count,
// prioritising nodes not yet tracked (zero load).
type MinRequestPolicy struct {
	MaxRequestLoad int64
}

func NewMinRequestPolicy() *MinRequestPolicy {
	return &MinRequestPolicy{
		MaxRequestLoad: defaultMaxRequestLoad,
	}
}

func (p *MinRequestPolicy) Name() Name { return NameMinRequest }

// ConfigSummary implements ConfigDescriber.
func (p *MinRequestPolicy) ConfigSummary() ConfigInfo {
	return ConfigInfo{MaxRequestLoad: p.MaxRequestLoad}
}

func (p *MinRequestPolicy) Select(_ context.Context, _ *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	var (
		best     *domain.Instance
		bestLoad int64 = math.MaxInt64
	)

	for _, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}

		active := ns.LoadActiveRequests()
		inst := ns.Instance

		// Prioritise instances with zero load (equivalent to "not in ZSet").
		if active == 0 {
			return inst, nil
		}

		if active < bestLoad {
			bestLoad = active
			best = inst
		}
	}

	if best == nil {
		return nil, ErrNoInstances
	}
	if bestLoad > p.MaxRequestLoad {
		return nil, ErrAllOverloaded
	}
	return best, nil
}

func (p *MinRequestPolicy) Feedback(_ string, _ *domain.CostMetrics) {
	// no-op
}

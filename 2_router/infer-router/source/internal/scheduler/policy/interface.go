package policy

import (
	"context"

	"github.com/yzx/rl-router/internal/domain"
)

// Policy is the scheduling algorithm interface.
// Implementations decide which backend instance should serve a request.
//
// Select is designed for single-threaded event-loop use. Implementations
// may read shared state (e.g., instanceMeta) under a read-lock to protect
// against concurrent writers, but callers MUST NOT invoke Select from
// multiple goroutines simultaneously.
type Policy interface {
	// Name returns the policy's registered name (e.g. "round_robin", "min_load").
	Name() Name
	// Select picks the best instance from the available nodes.
	// NOT safe for concurrent use — must be called from the event-loop only.
	Select(ctx context.Context, req *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error)
	// Feedback receives post-request metrics for adaptive learning.
	// Called from the event-loop after each Release. Current implementations
	// are all no-ops; future policies (e.g. cost-aware auto-tuning) will use
	// this to adjust scoring parameters at runtime.
	Feedback(instanceID string, metrics *domain.CostMetrics)
}

// Resettable is an optional interface that policies can implement
// to reset internal state between training steps.
type Resettable interface {
	Reset()
}

// BatchSelectResult holds the outcome for a single request within a batch.
type BatchSelectResult struct {
	Instance *domain.Instance
	Err      error
}

// BatchSelector is an optional interface that policies can implement to
// allocate a batch of requests in one pass over the node set.
// Complexity is typically O(N_nodes + K*log(N_nodes)) instead of O(K*N_nodes).
//
// NOT safe for concurrent use. BatchSelect must be called exclusively from
// the single-threaded scheduler event-loop. The returned []BatchSelectResult
// is backed by a reusable internal buffer; callers must finish reading the
// results before the next BatchSelect call.
type BatchSelector interface {
	BatchSelect(reqs []*domain.RouteContext, nodes map[string]*domain.NodeState) []BatchSelectResult
}

// GenerationAware is an optional interface for policies that cache derived data
// from the node set (e.g. sorted lists, ID slices). The scheduler calls
// SetGeneration with the current NodeStateStore generation counter before each
// batch, so the policy can detect node-set changes (add/remove/replace) and
// rebuild its cache. This replaces fragile count-based invalidation.
type GenerationAware interface {
	SetGeneration(gen uint64)
}

// CachedIDEnsurer is an optional interface for policies that need a reusable
// random-access slice of node IDs for O(1) sampling paths such as P2C.
// Implementations must be concurrency-safe with Select as documented by the policy.
type CachedIDEnsurer interface {
	EnsureCachedIDs(nodes map[string]*domain.NodeState)
}

// SessionRemover is an optional interface for policies that track session affinity.
// The scheduler calls RemoveSession when a client signals session completion
// (e.g. /api/v2/session_finish). SessionAwarePolicy implements this.
type SessionRemover interface {
	RemoveSession(sessionID string)
}

// SessionQuerier is an optional interface for policies that track session mappings.
// Used by the scheduler to classify queued requests (new vs existing session)
// in diagnostic logs.
type SessionQuerier interface {
	HasSession(sessionID string) bool
}

// InstanceMetaUpdate carries per-instance load metadata from the metrics collector.
type InstanceMetaUpdate struct {
	InstanceID      string
	WaitingCount    int64
	AvailableBlocks int64
	AvgIOLength     int64
	TotalBlocks     int64 // total GPU KV blocks capacity
}

// MetaUpdater is an optional interface for policies that consume backend load
// metrics (e.g. waiting count, available KV blocks). The MetricsCollector calls
// BatchUpdateInstanceMeta after each sweep to feed fresh data into the policy.
// Policies that do NOT implement this interface still benefit from health checks
// via NodeState.Healthy (universal, atomic).
type MetaUpdater interface {
	BatchUpdateInstanceMeta(updates []InstanceMetaUpdate)
}

// DriftAware is an optional interface for policies that track local allocation drift
// to compensate for staleness between polled metrics and real-time scheduling.
// The scheduler calls TrackAcquire after each allocation and TrackRelease after each
// release, so the policy can adjust its effective waiting count between metric sweeps.
type DriftAware interface {
	TrackAcquire(instanceID string)
	TrackRelease(instanceID string)
}

// ConfigDescriber is an optional interface that policies implement to expose
// their configured limits. The scheduler uses this to build human-readable
// error messages without needing to know policy internals.
type ConfigDescriber interface {
	ConfigSummary() ConfigInfo
}

// ConfigInfo holds the limit values relevant for error enrichment.
type ConfigInfo struct {
	MaxRequestLoad int64 // per-instance request cap; 0 = not applicable
	MaxSessionLoad int64 // per-instance session cap; 0 = not applicable
}

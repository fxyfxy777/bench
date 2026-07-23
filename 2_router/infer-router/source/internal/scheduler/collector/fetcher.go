package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
)

// InstanceMetrics holds raw metrics scraped from a backend instance's /metrics endpoint.
type InstanceMetrics struct {
	WaitingCount      int64
	RunningCount      int64
	AvailableBlocks   int64
	GpuCacheUsagePerc float64
}

// MetricsFetcher scrapes metrics from a single backend instance.
// Each backend type (fastdeploy, sglang, vllm) implements this interface
// with its own Prometheus metric name mapping.
type MetricsFetcher interface {
	// Fetch scrapes given metricsEndpoint and returns parsed metrics.
	// The endpoint is in "host:port" format (no scheme).
	Fetch(ctx context.Context, metricsEndpoint string) (InstanceMetrics, error)
	// Name returns the backend type name (e.g. "fastdeploy").
	Name() string
}

// MetaUpdater is re-exported from policy for use by collector.
// Policies that consume backend load metrics implement this interface.
type MetaUpdater = policy.MetaUpdater

// InstanceMetaUpdate is re-exported from policy for convenience.
type InstanceMetaUpdate = policy.InstanceMetaUpdate

// NodeStateGetter provides read access to current set of backend nodes.
// Implemented by NodeStateStore.
type NodeStateGetter interface {
	GetNodes() map[string]*domain.NodeState
}

// fetcherFactory is a constructor function for a MetricsFetcher.
type fetcherFactory func(client *http.Client) MetricsFetcher

// ErrUnknownBackendType is returned when no fetcher is registered for a backend type.
var ErrUnknownBackendType = errors.New("unknown backend type")

// HttpStatusError represents a non-200 HTTP response from a metrics endpoint.
// This is distinct from transport errors (connection refused, timeout, DNS) and
// should NOT count toward consecutive failure threshold for health marking.
type HttpStatusError struct {
	StatusCode int
	URL        string
}

func (e *HttpStatusError) Error() string {
	return fmt.Sprintf("metrics endpoint %s returned status %d", e.URL, e.StatusCode)
}

// IsTransportError returns true if error is a transport-level failure
// (connection refused, timeout, DNS) rather than an HTTP status error.
func IsTransportError(err error) bool {
	if _, ok := errors.AsType[*HttpStatusError](err); ok {
		return false
	}
	return true
}

// RegisterFetcher adds a fetcher factory for a backend type.
// Not concurrency-safe — call during init() only.
func RegisterFetcher(backendType string, factory fetcherFactory) {
	fetcherRegistry[backendType] = factory
}

var fetcherRegistry = map[string]fetcherFactory{
	"fastdeploy": func(c *http.Client) MetricsFetcher { return NewFastDeployFetcher(c) },
}

// RegisteredBackendTypes returns a slice of all registered backend type names.
func RegisteredBackendTypes() []string {
	types := make([]string, 0, len(fetcherRegistry))
	for k := range fetcherRegistry {
		types = append(types, k)
	}
	return types
}

// BuildFetcher creates a MetricsFetcher for given backend type using shared HTTP client.
func BuildFetcher(backendType string, client *http.Client) (MetricsFetcher, error) {
	factory, ok := fetcherRegistry[backendType]
	if !ok {
		return nil, fmt.Errorf("%w: unknown backend type: %v, registered: %v",
			ErrUnknownBackendType, backendType, RegisteredBackendTypes())
	}
	return factory(client), nil
}

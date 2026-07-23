package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "requests_total",
		Help:      "Total number of routed requests.",
	}, []string{"instance_id", "status"})

	ActiveRequests = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "active_requests",
		Help:      "Current number of in-flight requests per instance.",
	}, []string{"instance_id"})

	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "request_duration_ms",
		Help:      "Request duration in milliseconds.",
		Buckets:   prometheus.ExponentialBuckets(10, 2, 12),
	}, []string{"instance_id"})

	RegisteredGateways = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "registered_gateways",
		Help:      "Current number of registered gateway nodes.",
	})

	StepPhaseGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "step_phase",
		Help:      "Current step phase (0=IDLE, 1=SERVING, 2=DRAINING).",
	})

	StepIDGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "step_id",
		Help:      "Current step ID.",
	})

	HeartbeatTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "heartbeat_total",
		Help:      "Total heartbeats received per gateway.",
	}, []string{"gateway_id"})

	AllocDedupHits = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "alloc_dedup_hits_total",
		Help:      "Total Allocate requests that hit the dedup cache (same request_id).",
	})

	AllocCallerGoneSkips = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "alloc_caller_gone_skips_total",
		Help:      "Allocations skipped because caller context was already cancelled.",
	})

	PDAllocCompensationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "pd_alloc_compensations_total",
		Help:      "Total AllocatePD results released because the caller cancelled after a fresh allocation was committed.",
	})

	ReleaseDedupHits = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "release_dedup_hits_total",
		Help:      "Total Release requests skipped due to duplicate allocation_id.",
	})

	ReleaseRetries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "release_retries_total",
		Help:      "Total Release retry attempts from gateway.",
	})

	// Event-loop observability.
	EventChannelDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "event_channel_depth",
		Help:      "Current number of pending events in the scheduler event channel.",
	})

	EventLoopBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "event_loop_batch_size",
		Help:      "Number of events processed per event-loop iteration.",
		Buckets:   prometheus.ExponentialBuckets(1, 2, 14), // 1, 2, 4, ..., 8192
	})

	EventLoopBatchDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "event_loop_batch_duration_ms",
		Help:      "Time spent processing one event-loop batch in milliseconds.",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 16), // 0.01ms .. 327ms
	})

	AllocQueueWaitMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "alloc_queue_wait_ms",
		Help:      "Time an Allocate event spends waiting in the event channel before processing.",
		Buckets:   prometheus.ExponentialBuckets(0.1, 2, 14), // 0.1ms .. 409ms
	})

	RemoteAllocLatencyMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "remote_alloc_latency_ms",
		Help:      "RPC latency for RemoteAllocator Allocate/Release calls.",
		Buckets:   prometheus.ExponentialBuckets(0.5, 2, 12), // 0.5ms .. 1024ms
	}, []string{"method"}) // "allocate" or "release"

	// StepNotifier metrics.
	StepNotifyTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "step_notify_total",
		Help:      "Step-state notifications sent to gateways.",
	}, []string{"result"}) // "success", "failure"

	// Ghost cleanup metrics.
	GhostCleanupTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "ghost_cleanup_total",
		Help:      "Total ghost allocation cleanup events for expired gateways.",
	})

	GhostCleanupReleasedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "ghost_cleanup_released_total",
		Help:      "Total individual allocations released during ghost cleanup.",
	})

	// Control-plane handler metrics.
	ControlPlaneOpsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "control_plane_ops_total",
		Help:      "Control-plane HTTP handler invocations.",
	}, []string{"op", "result"}) // op: "start_step", "end_step", etc.

	// Dedup map size gauges (memory pressure indicators).
	AllocDedupSize = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "alloc_dedup_size",
		Help:      "Current number of entries in the Allocate dedup cache.",
	})

	ReleaseDedupSize = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "release_dedup_size",
		Help:      "Current number of entries in the Release dedup cache.",
	})

	// Proxy backend response status.
	ProxyBackendStatus = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "proxy_backend_status_total",
		Help:      "Backend HTTP response status codes observed by the proxy.",
	}, []string{"instance_id", "status_class"}) // status_class: "2xx", "4xx", "5xx"

	// Panic recovery counter.
	PanicRecoveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "panic_recoveries_total",
		Help:      "Total panics recovered by middleware.",
	}, []string{"layer"}) // layer: "http", "grpc"

	// Metrics collector observability (no per-instance labels to avoid cardinality explosion).
	MetricsSweepDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "metrics_sweep_duration_seconds",
		Help:      "Duration of each metrics collector sweep.",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms .. 5s
	})

	MetricsSweepInstances = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "metrics_sweep_instances",
		Help:      "Number of instances scraped in the last sweep.",
	})

	MetricsSweepFailures = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "metrics_sweep_failures",
		Help:      "Number of instances that failed scraping in the last sweep.",
	})

	// Health checker observability.
	HealthProbeResults = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "health_probe_results_total",
		Help:      "Total health probe results by outcome.",
	}, []string{"result"}) // result: "success", "failure", "unhealthy_mark", "recovery"

	HealthProbeUnhealthyInstances = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "health_probe_unhealthy_instances",
		Help:      "Current number of instances marked unhealthy by the health checker.",
	})

	HealthSweepDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "health_sweep_duration_seconds",
		Help:      "Duration of each health checker sweep.",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms .. 5s
	})

	// Circuit breaker observability.
	CircuitBreakerTrips = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "circuit_breaker_trips_total",
		Help:      "Total circuit breaker state transitions.",
	}, []string{"instance_id", "transition"}) // transition: "open", "half_open", "closed"

	// Token usage counters (extracted from non-streaming responses).
	TokensPromptTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "tokens_prompt_total",
		Help:      "Total prompt tokens processed per instance.",
	}, []string{"instance_id"})

	TokensCompletionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "tokens_completion_total",
		Help:      "Total completion tokens generated per instance.",
	}, []string{"instance_id"})

	// Session count per instance (active sessions bound to each instance).
	InstanceSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "instance_sessions",
		Help:      "Current number of active sessions assigned to each instance.",
	}, []string{"instance_id"})

	// Time to first byte for both streaming (TTFT) and non-streaming responses.
	TimeToFirstByte = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "time_to_first_byte_ms",
		Help:      "Time from proxy start to first byte of response.",
		Buckets:   prometheus.ExponentialBuckets(10, 2, 14), // 10ms .. 81.9s
	}, []string{"instance_id", "mode"}) // mode: "stream", "non_stream"

	// SSE stream completion status.
	StreamCompletionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "stream_completion_total",
		Help:      "SSE stream completion status per instance.",
	}, []string{"instance_id", "result"}) // result: "done", "interrupted", "error"

	// Non-streaming retry counter.
	NonStreamRetries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "non_stream_retries_total",
		Help:      "Total non-streaming request retries due to backend failure.",
	})

	// Policy selection latency.
	PolicySelectDurationMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "policy_select_duration_ms",
		Help:      "Time spent in policy Select/BatchSelect per event-loop batch.",
		Buckets:   prometheus.ExponentialBuckets(0.001, 2, 17), // 1us .. 65ms
	})

	// Policy routing decision branch counters.
	PolicyBranchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "policy_branch_total",
		Help:      "Routing decisions by policy and decision branch.",
	}, []string{"policy", "branch"})

	// gRPC server metrics.
	GRPCServerRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "grpc_server_requests_total",
		Help:      "Total gRPC server requests by method and status code.",
	}, []string{"method", "code"})

	GRPCServerRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "grpc_server_request_duration_ms",
		Help:      "gRPC server request duration in milliseconds.",
		Buckets:   prometheus.ExponentialBuckets(0.1, 2, 16), // 0.1ms .. 3.2s
	}, []string{"method"})

	// gRPC client metrics.
	GRPCClientRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "grpc_client_requests_total",
		Help:      "Total gRPC client requests by method and status code.",
	}, []string{"method", "code"})

	GRPCClientRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "grpc_client_request_duration_ms",
		Help:      "gRPC client request duration in milliseconds.",
		Buckets:   prometheus.ExponentialBuckets(0.1, 2, 16), // 0.1ms .. 3.2s
	}, []string{"method"})

	// --- Rate limiting / concurrency control ---

	// Scheduler-side global rate limit.
	GlobalRateLimitRejects = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "global_rate_limit_rejects_total",
		Help:      "Allocations rejected by the scheduler global rate limit.",
	})

	GlobalRateLimitHeadroom = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "global_rate_limit_headroom",
		Help:      "Remaining global inflight capacity (globalMax - activeCount). -1 if unlimited.",
	})

	// Gateway-side local rate limit.
	RateLimitTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "rate_limit_total",
		Help:      "Total gateway rate limit decisions.",
	}, []string{"result"}) // "acquired", "queued_acquired", "rejected_full", "rejected_timeout"

	RateLimitConcurrentRequests = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "rate_limit_concurrent_requests",
		Help:      "Current number of in-flight requests held by the gateway rate limiter.",
	})

	RateLimitQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "rate_limit_queue_depth",
		Help:      "Current number of requests waiting in the gateway rate limit queue.",
	})

	RateLimitWaitDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "rate_limit_wait_duration_ms",
		Help:      "Time spent waiting in the gateway rate limit queue.",
		Buckets:   prometheus.ExponentialBuckets(1, 2, 14), // 1ms .. 8.2s
	})

	// --- Request forensics ---

	// Disconnect source attribution.
	DisconnectTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "disconnect_total",
		Help:      "Request disconnections by source, path, and phase.",
	}, []string{"source", "path", "phase"}) // source: client/backend/timeout; path: stream/non_stream/queue; phase: queue/allocate/forward/stream/response

	// StaleConnRetries counts retries triggered by stale backend connections
	// (e.g., "use of closed network connection", "connection reset by peer").
	StaleConnRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "stale_conn_retries_total",
		Help:      "Retries triggered by stale backend connections.",
	}, []string{"path"}) // labels: "stream", "non_stream"

	// Inflight request age — sampled periodically by InflightTracker.
	InflightRequestAge = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "inflight_request_age_seconds",
		Help:      "Age of currently in-flight requests, sampled periodically.",
		Buckets:   []float64{1, 5, 10, 30, 60, 120, 300, 600},
	})

	// --- Waiting queue ---

	WaitingQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "waiting_queue_depth",
		Help:      "Current number of requests in the scheduler waiting queue.",
	})

	WaitingQueueEnqueueTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "waiting_queue_enqueue_total",
		Help:      "Total requests enqueued to the waiting queue.",
	})

	WaitingQueueDrainTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "waiting_queue_drain_total",
		Help:      "Waiting queue drain outcomes.",
	}, []string{"reason"}) // reason: "success", "timeout", "cancelled", "error", "step_reject"

	WaitingQueueDrainCycles = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "waiting_queue_drain_cycles_total",
		Help:      "Total drain sweep invocations.",
	})

	WaitingQueueDrainCapped = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "waiting_queue_drain_capped_total",
		Help:      "Drain sweeps that exited early because all instances were overloaded.",
	})

	WaitingQueueWaitDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "waiting_queue_wait_duration_ms",
		Help:      "Time a request spent in the waiting queue before being drained.",
		Buckets:   prometheus.ExponentialBuckets(10, 2, 16), // 10ms .. 327s
	})

	// Event-loop last active timestamp (unix seconds) — proof-of-life for hang detection.
	EventLoopLastActiveTS = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "event_loop_last_active_ts",
		Help:      "Unix timestamp of the last event-loop processBatch completion.",
	})

	// --- PD waiting queue ---

	PDWaitingQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "pd_waiting_queue_depth",
		Help:      "Current number of PD requests in the scheduler PD waiting queue.",
	})

	PDWaitingQueueEnqueueTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "pd_waiting_queue_enqueue_total",
		Help:      "Total PD requests enqueued to the PD waiting queue.",
	})

	PDWaitingQueueDrainTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "pd_waiting_queue_drain_total",
		Help:      "PD waiting queue drain outcomes.",
	}, []string{"reason"}) // reason: "success", "timeout", "cancelled", "error", "step_reject"

	PDWaitingQueueWaitDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "pd_waiting_queue_wait_duration_ms",
		Help:      "Time a PD request spent in the PD waiting queue before being drained.",
		Buckets:   prometheus.ExponentialBuckets(10, 2, 16), // 10ms .. 327s
	})

	// --- Pending release (heartbeat piggyback) ---

	PendingReleaseEnqueued = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "pending_release_enqueued_total",
		Help:      "Releases enqueued for heartbeat piggyback after retry exhaustion.",
	})

	PendingReleaseDelivered = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "pending_release_delivered_total",
		Help:      "Pending releases successfully piggybacked on heartbeat.",
	})

	// --- Normal allocate compensation ---

	AllocCompensationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rl_router",
		Name:      "alloc_compensations_total",
		Help:      "Normal Allocate results released because the caller cancelled after a fresh allocation was committed.",
	})
)

// ---------- InstanceMetrics pre-cache ----------
//
// Replaces per-request WithLabelValues(instanceID) map+mutex lookups
// with a single sync.Map.Load (amortized lock-free). Reduces 4 mutex
// acquisitions per request to ~1 atomic load.

// InstanceMetrics holds pre-resolved metric handles for a single instance.
type InstanceMetrics struct {
	ActiveRequests   prometheus.Gauge
	DurationMs       prometheus.Observer
	RequestsOK       prometheus.Counter
	RequestsError    prometheus.Counter
	TokensPrompt     prometheus.Counter
	TokensCompletion prometheus.Counter
	Sessions         prometheus.Gauge
}

var instanceMetricsCache sync.Map // string → *InstanceMetrics

// GetInstanceMetrics returns cached metric handles for the given instance ID.
// First call per instance populates the cache; subsequent calls return the
// cached entry via a single atomic load.
func GetInstanceMetrics(instanceID string) *InstanceMetrics {
	if val, ok := instanceMetricsCache.Load(instanceID); ok {
		return val.(*InstanceMetrics)
	}

	im := &InstanceMetrics{
		ActiveRequests:   ActiveRequests.WithLabelValues(instanceID),
		DurationMs:       RequestDuration.WithLabelValues(instanceID),
		RequestsOK:       RequestsTotal.WithLabelValues(instanceID, "ok"),
		RequestsError:    RequestsTotal.WithLabelValues(instanceID, "error"),
		TokensPrompt:     TokensPromptTotal.WithLabelValues(instanceID),
		TokensCompletion: TokensCompletionTotal.WithLabelValues(instanceID),
		Sessions:         InstanceSessions.WithLabelValues(instanceID),
	}
	actual, _ := instanceMetricsCache.LoadOrStore(instanceID, im)
	return actual.(*InstanceMetrics)
}

// PurgeInstanceMetrics removes cached metrics for an instance.
// Call when an instance is deregistered to prevent stale entries.
func PurgeInstanceMetrics(instanceID string) {
	instanceMetricsCache.Delete(instanceID)
	ActiveRequests.DeleteLabelValues(instanceID)
	RequestDuration.DeleteLabelValues(instanceID)
	RequestsTotal.DeleteLabelValues(instanceID, "ok")
	RequestsTotal.DeleteLabelValues(instanceID, "error")
	TokensPromptTotal.DeleteLabelValues(instanceID)
	TokensCompletionTotal.DeleteLabelValues(instanceID)
	InstanceSessions.DeleteLabelValues(instanceID)
}

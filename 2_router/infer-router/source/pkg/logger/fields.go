package logger

import "go.uber.org/zap"

// Standard event constants for structured log parsing and aggregation.
// Use with Event() helper to produce zero-allocation zap fields.
const (
	EventRequestStart      = "REQUEST_START"
	EventRequestComplete   = "REQUEST_COMPLETE"
	EventSlowRequest       = "SLOW_REQUEST"
	EventAllocate          = "ALLOCATE"
	EventAllocateSlow      = "ALLOCATE_SLOW"
	EventProxyForward      = "PROXY_FORWARD"
	EventProxyRequest      = "PROXY_REQUEST"
	EventProxyComplete     = "PROXY_COMPLETE"
	EventProxyReceiving    = "PROXY_RECEIVING"
	EventProxyPDForward    = "PROXY_PD_FORWARD"
	EventRelease           = "RELEASE"
	EventBatchProcessed    = "BATCH_PROCESSED"
	EventStepStart         = "STEP_START"
	EventStepEnd           = "STEP_END"
	EventGatewayRegister   = "GATEWAY_REGISTER"
	EventGatewayExpired    = "GATEWAY_EXPIRED"
	EventGRPCCall          = "GRPC_CALL"
	EventSchedulerAlive    = "SCHEDULER_ALIVE" // periodic heartbeat from event-loop
	EventPanicRecovered    = "PANIC_RECOVERED"
	EventMetricsSweep      = "METRICS_SWEEP"      // one collector sweep completed
	EventInstanceUnhealthy = "INSTANCE_UNHEALTHY" // instance marked unhealthy after N failures
	EventInstanceRecovered = "INSTANCE_RECOVERED" // instance recovered from unhealthy state
	EventHealthProbe       = "HEALTH_PROBE"       // one health checker sweep completed
	EventHealthRecovery    = "HEALTH_RECOVERY"    // instance recovered via health probe
	EventCircuitOpen       = "CIRCUIT_OPEN"       // circuit breaker tripped to Open
	EventCircuitClose      = "CIRCUIT_CLOSE"      // circuit breaker recovered to Closed
	EventCircuitHalfOpen   = "CIRCUIT_HALF_OPEN"  // circuit breaker transitioned to HalfOpen
	EventNonStreamForward  = "NON_STREAM_FORWARD" // non-streaming request forwarded to backend
	EventNonStreamRetry    = "NON_STREAM_RETRY"   // non-streaming request retried on different instance
	EventStreamComplete    = "STREAM_COMPLETE"    // SSE stream ended with [DONE]
	EventStreamInterrupt   = "STREAM_INTERRUPT"   // SSE stream interrupted (client disconnect / backend error)
	EventAudit             = "AUDIT"              // single-line request lifecycle audit record (never sampled)
	EventBackendNon2xx     = "BACKEND_NON_2XX"    // backend returned non-2xx status (already committed 200 to client)
	EventControlPlaneHTTP  = "CONTROL_PLANE_HTTP" // scheduler control-plane HTTP request/response dump

	// Waiting queue events.
	EventQueueEnqueue    = "QUEUE_ENQUEUE"       // request enqueued to waiting queue
	EventQueueDrainCycle = "QUEUE_DRAIN_CYCLE"   // one drain sweep completed
	EventQueueDrained    = "QUEUE_DRAIN_SUCCESS" // queued request successfully allocated
	EventQueueTimeout    = "QUEUE_TIMEOUT"       // queued request timed out
	EventQueueFullReject = "QUEUE_FULL_REJECT"   // enqueue rejected: queue at max_size
	EventQueueCancel     = "QUEUE_CLIENT_CANCEL" // queued request cancelled by client
	EventQueueStepReject = "QUEUE_STEP_REJECT"   // all queued requests rejected on step end/reset
	EventNoReleaseAlarm  = "NO_RELEASE_ALARM"    // queue non-empty but no Release in N drain cycles
	EventPDPause         = "PD_PAUSE"            // PD allocation paused, queue drained
	EventPDContinue      = "PD_CONTINUE"         // PD allocation resumed
)

// Standard status constants for structured log parsing.
const (
	StatusOK      = "OK"
	StatusFail    = "FAIL"
	StatusTimeout = "TIMEOUT"
	StatusRetry   = "RETRY"
)

// Standard reason constants for error attribution.
// When level >= WARN, logs should carry a reason to enable fast triage.
const (
	ReasonUpstreamTimeout     = "UPSTREAM_TIMEOUT"
	ReasonUpstreamConnRefused = "UPSTREAM_CONN_REFUSED"
	ReasonUpstream5xx         = "UPSTREAM_5XX"
	ReasonNoHealthyBackend    = "NO_HEALTHY_BACKEND"
	ReasonAllOverloaded       = "ALL_OVERLOADED"
	ReasonSchedulerSlow       = "SCHEDULER_SLOW"
	ReasonSchedulerNotServing = "SCHEDULER_NOT_SERVING"
	ReasonReleaseFailed       = "RELEASE_FAILED"
	ReasonInternalError       = "INTERNAL_ERROR"
	ReasonProxyError          = "PROXY_ERROR"
)

// Event returns a zero-allocation zap field for event identification.
func Event(e string) zap.Field { return zap.String("event", e) }

// Status returns a zero-allocation zap field for result status.
func Status(s string) zap.Field { return zap.String("status", s) }

// Reason returns a zero-allocation zap field for error attribution.
func Reason(r string) zap.Field { return zap.String("reason", r) }

// DisconnectSourceField returns a zero-allocation zap field for disconnect attribution.
func DisconnectSourceField(s string) zap.Field { return zap.String("disconnect_source", s) }

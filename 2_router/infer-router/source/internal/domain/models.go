package domain

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	// LabelResourceType preserves upstream instance role metadata (for example
	// PaddleRL's "resource_type"). It is informational; scheduling isolation is
	// controlled by the explicit ResourceGroup field.
	LabelResourceType = "resource_type"

	// DefaultResourceGroup is used for callers that do not provide a resource_group.
	DefaultResourceGroup = "default"
)

// NormalizeResourceGroup returns a valid scheduling resource group name.
// Empty values are mapped to the default single-pool group.
func NormalizeResourceGroup(group string) string {
	if group != "" {
		return group
	}
	return DefaultResourceGroup
}

// StepPhase represents the current phase of a training step.
type StepPhase int32

const (
	StepIdle     StepPhase = 0
	StepServing  StepPhase = 1
	StepDraining StepPhase = 2
)

func (p StepPhase) String() string {
	switch p {
	case StepIdle:
		return "IDLE"
	case StepServing:
		return "SERVING"
	case StepDraining:
		return "DRAINING"
	default:
		return "UNKNOWN"
	}
}

// StepState is an immutable snapshot of scheduler step-level state.
// It bundles the three fields that are pushed/queried together at every
// step transition boundary (notifier broadcast, status endpoints, gateway
// push notifications), eliminating positional-parameter drift across the
// (Phase, StepID, Paused) tuple. JSON tags match the legacy
// StepStateNotification payload for wire compatibility.
type StepState struct {
	ResourceGroup string    `json:"resource_group,omitempty"`
	Phase         StepPhase `json:"phase"`
	StepID        int64     `json:"step_id"`
	Paused        bool      `json:"paused"`
}

// IsServing reports whether the step is currently in the SERVING phase.
func (s StepState) IsServing() bool { return s.Phase == StepServing }

// Instance represents a backend inference service instance (e.g. vllm, sglang, fastdeploy).
// All fields are value types — no pointers on the hot path.
type Instance struct {
	ID              string            `json:"id"`
	Endpoint        string            `json:"endpoint"`                 // inference address, host:port
	MetricsEndpoint string            `json:"metrics_endpoint"`         // metrics address, host:port
	BackendType     string            `json:"backend_type,omitzero"`    // "fastdeploy", "sglang", "vllm"
	GPUNum          int               `json:"gpu_num,omitzero"`         // number of GPUs, used as load-balancing weight
	TotalKVBlocks   int               `json:"total_kv_blocks,omitzero"` // KV cache capacity, used by cache-aware policy
	ModelVersion    int64             `json:"model_version,omitzero"`   // training step, used for version validation
	ResourceGroup   string            `json:"resource_group,omitzero"`  // scheduling isolation group
	Labels          map[string]string `json:"labels,omitzero"`

	// PD separation (splitwise) fields.
	Role             string   `json:"role,omitempty"`              // "mixed"/"prefill"/"decode"
	Host             string   `json:"host,omitempty"`              // bare host IP (without port)
	ConnectorPort    string   `json:"connector_port,omitempty"`    // connector port
	TransferProtocol []string `json:"transfer_protocol,omitempty"` // e.g. ["ipc","rdma"]
	RDMAPorts        []string `json:"rdma_ports,omitempty"`        // RDMA port list
	DeviceIDs        []string `json:"device_ids,omitempty"`        // GPU device ID list
	TpSize           int      `json:"tp_size,omitempty"`           // tensor parallelism size
	DPSize           int      `json:"dp_size,omitempty"`           // data parallelism size (fetched from sglang server_info)
	DPRank           int      `json:"dp_rank"`                     // dp rank index; -1 means non-DP instance
}

// NodeState holds the real-time load status of a single backend instance.
// All field mutations happen exclusively in the scheduler event-loop.
// Concurrent readers (monitoring, snapshots) use atomic accessor methods.
type NodeState struct {
	Instance       *Instance `json:"instance"`
	ActiveRequests int64     `json:"active_requests"`
	LockedMemory   int64     `json:"locked_memory"`
	ActualLoad     float64   `json:"actual_load"`
	Healthy        int32     `json:"healthy"`      // 1=healthy(default), 0=unhealthy; written by HealthChecker goroutine
	CircuitOpen    int32     `json:"circuit_open"` // 0=closed(default), 1=open; written by scheduler event-loop
}

// Atomic accessors for ActiveRequests — safe for concurrent read from any goroutine.

func (n *NodeState) LoadActiveRequests() int64   { return atomic.LoadInt64(&n.ActiveRequests) }
func (n *NodeState) AddActiveRequests(d int64)   { atomic.AddInt64(&n.ActiveRequests, d) }
func (n *NodeState) StoreActiveRequests(v int64) { atomic.StoreInt64(&n.ActiveRequests, v) }

// Atomic accessors for LockedMemory.

func (n *NodeState) LoadLockedMemory() int64   { return atomic.LoadInt64(&n.LockedMemory) }
func (n *NodeState) StoreLockedMemory(v int64) { atomic.StoreInt64(&n.LockedMemory, v) }

// Atomic accessors for ActualLoad (float64 via bit-casting).

func (n *NodeState) LoadActualLoad() float64 {
	bits := atomic.LoadUint64((*uint64)(unsafe.Pointer(&n.ActualLoad)))
	return math.Float64frombits(bits)
}

func (n *NodeState) StoreActualLoad(v float64) {
	atomic.StoreUint64((*uint64)(unsafe.Pointer(&n.ActualLoad)), math.Float64bits(v))
}

// Atomic accessors for Healthy — safe for concurrent read/write from any goroutine.
// The HealthChecker goroutine writes Healthy based on /health probe results;
// all scheduling policies read Healthy via LoadAvailable() to skip unhealthy nodes.

func (n *NodeState) LoadHealthy() bool {
	return atomic.LoadInt32(&n.Healthy) == 1
}

func (n *NodeState) StoreHealthy(v bool) {
	var val int32
	if v {
		val = 1
	}
	atomic.StoreInt32(&n.Healthy, val)
}

// Atomic accessors for CircuitOpen — written by the scheduler event-loop,
// read by policies (same goroutine) and monitoring endpoints (different goroutines).

func (n *NodeState) LoadCircuitOpen() bool {
	return atomic.LoadInt32(&n.CircuitOpen) == 1
}

func (n *NodeState) StoreCircuitOpen(v bool) {
	var val int32
	if v {
		val = 1
	}
	atomic.StoreInt32(&n.CircuitOpen, val)
}

// LoadAvailable returns true when the instance is healthy AND its circuit breaker
// is not open. This is the single availability check used by all scheduling policies.
func (n *NodeState) LoadAvailable() bool {
	return atomic.LoadInt32(&n.Healthy) == 1 && atomic.LoadInt32(&n.CircuitOpen) == 0
}

// Clone returns a snapshot copy using atomic reads.
func (n *NodeState) Clone() *NodeState {
	var healthy int32
	if n.LoadHealthy() {
		healthy = 1
	}
	var circuitOpen int32
	if n.LoadCircuitOpen() {
		circuitOpen = 1
	}
	return &NodeState{
		Instance:       n.Instance,
		ActiveRequests: n.LoadActiveRequests(),
		LockedMemory:   n.LoadLockedMemory(),
		ActualLoad:     n.LoadActualLoad(),
		Healthy:        healthy,
		CircuitOpen:    circuitOpen,
	}
}

// RouteContext carries metadata for a single routing decision.
type RouteContext struct {
	TraceID         string            `json:"trace_id"`
	RequestID       string            `json:"request_id,omitzero"` // idempotency key for Allocate dedup
	SessionID       string            `json:"session_id,omitzero"`
	GatewayID       string            `json:"gateway_id,omitzero"`
	ResourceGroup   string            `json:"resource_group,omitzero"`
	Labels          map[string]string `json:"labels,omitzero"`
	RequestText     string            `json:"request_text,omitzero"`      // concatenated message content for cache-aware prefix matching
	RequestTokenIDs []int             `json:"request_token_ids,omitzero"` // optional real token IDs for token-level cache routing
}

// CostMetrics contains post-request feedback used by the scheduler for load tracking.
type CostMetrics struct {
	DurationMs       int64   `json:"duration_ms"`
	GPUUsage         float64 `json:"gpu_usage"`
	ErrorCode        string  `json:"error_code,omitzero"`
	PromptTokens     int64   `json:"prompt_tokens,omitzero"`
	CompletionTokens int64   `json:"completion_tokens,omitzero"`
	TotalTokens      int64   `json:"total_tokens,omitzero"`
}

// ---------- sync.Pool for hot-path structs ----------

var routeContextPool = sync.Pool{New: func() any { return new(RouteContext) }}
var costMetricsPool = sync.Pool{New: func() any { return new(CostMetrics) }}

// AcquireRouteContext returns a zeroed RouteContext from the pool.
func AcquireRouteContext() *RouteContext {
	rc := routeContextPool.Get().(*RouteContext)
	*rc = RouteContext{}
	return rc
}

// ReleaseRouteContext returns a RouteContext to the pool.
func ReleaseRouteContext(rc *RouteContext) {
	if rc != nil {
		*rc = RouteContext{}
		routeContextPool.Put(rc)
	}
}

// AcquireCostMetrics returns a zeroed CostMetrics from the pool.
func AcquireCostMetrics() *CostMetrics {
	cm := costMetricsPool.Get().(*CostMetrics)
	*cm = CostMetrics{}
	return cm
}

// ReleaseCostMetrics returns a CostMetrics to the pool.
func ReleaseCostMetrics(cm *CostMetrics) {
	if cm != nil {
		costMetricsPool.Put(cm)
	}
}

// GatewayInfo holds the registration and liveness state of a gateway node.
// The GatewayAddr serves as both the unique identity and the push-notification target.
type GatewayInfo struct {
	GatewayAddr   string            `json:"gateway_addr"`
	Labels        map[string]string `json:"labels,omitzero"`
	LastHeartbeat time.Time         `json:"last_heartbeat"`
	ActiveConns   int64             `json:"active_connections"`
}

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/controlplane"
	"github.com/yzx/rl-router/internal/domain"
	stepnotifier "github.com/yzx/rl-router/internal/scheduler/notifier"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/internal/scheduler/registry"
	"github.com/yzx/rl-router/pkg/jsonutil"
	routerlogger "github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

// HTTPHandler exposes the scheduler control-plane over HTTP.
// Used by the training framework to manage step lifecycle.
type HTTPHandler struct {
	server     *Server
	registry   *registry.GatewayRegistry
	notifier   *stepnotifier.StepNotifier
	logger     *zap.Logger
	httpClient *http.Client // used for fetching backend server_info (e.g. sglang dp_size)
}

type resourceGroupSelectorRequest struct {
	ResourceGroup string                             `json:"resource_group,omitempty"`
	Metadata      controlplane.ResourceGroupMetadata `json:"metadata"`
}

func decodeOptionalResourceGroupSelector(r *http.Request) (resourceGroupSelectorRequest, error) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return resourceGroupSelectorRequest{}, nil
	}
	var req resourceGroupSelectorRequest
	if err := jsonutil.NewDecoder(r.Body).Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return resourceGroupSelectorRequest{}, nil
		}
		return resourceGroupSelectorRequest{}, err
	}
	return req, nil
}

func controlErrorStatus(err error, fallback int) int {
	switch {
	case errors.Is(err, ErrUnknownResourceGroup):
		return http.StatusNotFound
	case errors.Is(err, ErrResourceGroupInstanceConflict):
		return http.StatusConflict
	default:
		return fallback
	}
}

func NewHTTPHandler(server *Server, registry *registry.GatewayRegistry, notifier *stepnotifier.StepNotifier, logger *zap.Logger) *HTTPHandler {
	return &HTTPHandler{
		server:   server,
		registry: registry,
		notifier: notifier,
		logger:   logger,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// RegisterRoutes registers all control-plane HTTP routes on the given mux.
func (h *HTTPHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/steps/start", h.logHTTP("POST /v1/steps/start", h.handleStartStep))
	mux.HandleFunc("POST /v1/steps/end", h.logHTTP("POST /v1/steps/end", h.handleEndStep))
	mux.HandleFunc("POST /v1/steps/pause", h.logHTTP("POST /v1/steps/pause", h.handlePause))
	mux.HandleFunc("POST /v1/steps/continue", h.logHTTP("POST /v1/steps/continue", h.handleContinue))
	mux.HandleFunc("GET /v1/steps/current", h.logHTTP("GET /v1/steps/current", h.handleGetStep))
	mux.HandleFunc("POST /v1/instances", h.logHTTP("POST /v1/instances", h.handleRegisterInstances))
	mux.HandleFunc("PUT /v1/instances", h.logHTTP("PUT /v1/instances", h.handleSyncInstances))
	mux.HandleFunc("DELETE /v1/instances", h.logHTTP("DELETE /v1/instances", h.handleUnregisterInstances))
	mux.HandleFunc("GET /v1/instances", h.logHTTP("GET /v1/instances", h.handleListInstances))
	mux.HandleFunc("GET /v1/status", h.logHTTP("GET /v1/status", h.handleStatus))
	mux.HandleFunc("GET /v1/resource-groups", h.logHTTP("GET /v1/resource-groups", h.handleListResourceGroups))
}

// RegisterAdminRoutes registers scheduler admin-only endpoints onto the given mux.
func (h *HTTPHandler) RegisterAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/waiting-queue", h.logHTTP("GET /v1/admin/waiting-queue", h.handleWaitingQueueDiag))
}

func (h *HTTPHandler) logHTTP(route string, next http.HandlerFunc) http.HandlerFunc {
	return routerlogger.ControlPlaneHTTPLogHandler(h.logger, route, next)
}

// ---------- /v1/steps/start ----------

type startStepRequest struct {
	ResourceGroup     string                             `json:"resource_group,omitempty"`
	Metadata          controlplane.ResourceGroupMetadata `json:"metadata"`
	StepID            int64                              `json:"step_id"`
	Policy            string                             `json:"policy,omitzero"`
	MaxSessionLoad    int64                              `json:"max_session_load,omitempty"`
	LoadDiffThreshold int64                              `json:"load_diff_threshold,omitempty"`
	// Cache-aware policy fields.
	CacheThreshold      float64 `json:"cache_threshold,omitempty"`
	BalanceAbsThreshold int64   `json:"balance_abs_threshold,omitempty"`
	BalanceRelThreshold float64 `json:"balance_rel_threshold,omitempty"`
	CacheBlockSize      int     `json:"cache_block_size,omitempty"`
	HitRatioWeight      float64 `json:"hit_ratio_weight,omitempty"`
	LoadBalanceWeight   float64 `json:"load_balance_weight,omitempty"`
	MaxTreeSize         int64   `json:"max_tree_size,omitempty"`
	EvictionIntervalSec int64   `json:"eviction_interval_sec,omitempty"`
	// Session-aware V4 fields.
	SessionEvictEpochs   uint64 `json:"session_evict_epochs,omitempty"`
	SessionAffinityBoost int64  `json:"session_affinity_boost,omitempty"`
	// Per-instance active-request / load-score cap.
	MaxRequestLoad int64 `json:"max_request_load,omitempty"`
	// Per-resource-group in-flight allocation cap.
	MaxInflight int64 `json:"max_inflight,omitempty"`
	// Waiting queue overrides (per-step).
	WaitingQueueEnabled    *bool  `json:"waiting_queue_enabled,omitempty"`
	WaitingQueueMaxSize    *int64 `json:"waiting_queue_max_size,omitempty"`
	WaitingQueueTimeoutSec *int64 `json:"waiting_queue_timeout_sec,omitempty"`
}

type startStepResponse struct {
	Success           bool   `json:"success"`
	Message           string `json:"message,omitzero"`
	StepID            int64  `json:"step_id"`
	Phase             string `json:"phase"`
	Policy            string `json:"policy,omitempty"`
	MaxSessionLoad    int64  `json:"max_session_load,omitempty"`
	LoadDiffThreshold int64  `json:"load_diff_threshold,omitempty"`
	// Cache-aware policy fields (echoed back for observability).
	CacheThreshold      float64 `json:"cache_threshold,omitempty"`
	BalanceAbsThreshold int64   `json:"balance_abs_threshold,omitempty"`
	BalanceRelThreshold float64 `json:"balance_rel_threshold,omitempty"`
	CacheBlockSize      int     `json:"cache_block_size,omitempty"`
	HitRatioWeight      float64 `json:"hit_ratio_weight,omitempty"`
	LoadBalanceWeight   float64 `json:"load_balance_weight,omitempty"`
	MaxTreeSize         int64   `json:"max_tree_size,omitempty"`
	EvictionIntervalSec int64   `json:"eviction_interval_sec,omitempty"`
	// Session-aware V4 fields.
	SessionEvictEpochs   uint64 `json:"session_evict_epochs,omitempty"`
	SessionAffinityBoost int64  `json:"session_affinity_boost,omitempty"`
	// Per-instance active-request / load-score cap (echoed back for observability).
	MaxRequestLoad int64 `json:"max_request_load,omitempty"`
	MaxInflight    int64 `json:"max_inflight,omitempty"`
}

func (req startStepRequest) toPolicyConfig() policy.PolicyConfig {
	return policy.PolicyConfig{
		MaxSessionLoad:         req.MaxSessionLoad,
		LoadDiffThreshold:      req.LoadDiffThreshold,
		CacheThreshold:         req.CacheThreshold,
		BalanceAbsThreshold:    req.BalanceAbsThreshold,
		BalanceRelThreshold:    req.BalanceRelThreshold,
		CacheBlockSize:         req.CacheBlockSize,
		HitRatioWeight:         req.HitRatioWeight,
		LoadBalanceWeight:      req.LoadBalanceWeight,
		MaxTreeSize:            req.MaxTreeSize,
		EvictionIntervalSec:    req.EvictionIntervalSec,
		SessionEvictEpochs:     req.SessionEvictEpochs,
		SessionAffinityBoost:   req.SessionAffinityBoost,
		MaxRequestLoad:         req.MaxRequestLoad,
		MaxInflight:            req.MaxInflight,
		WaitingQueueEnabled:    req.WaitingQueueEnabled,
		WaitingQueueMaxSize:    req.WaitingQueueMaxSize,
		WaitingQueueTimeoutSec: req.WaitingQueueTimeoutSec,
	}
}

func (h *HTTPHandler) handleStartStep(w http.ResponseWriter, r *http.Request) {
	var req startStepRequest
	if err := jsonutil.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeJSON(w, http.StatusBadRequest, startStepResponse{
			Success: false,
			Message: "invalid request body: " + err.Error(),
		})
		return
	}

	policyCfg := req.toPolicyConfig()
	group, _ := controlplane.ResourceGroupFromRequest(r, req.ResourceGroup, req.Metadata.ResourceGroup)
	state, err := controlplane.StartStep(r.Context(), h.server, controlplane.StartStepCommand{
		ResourceGroup: group,
		StepID:        req.StepID,
		PolicyName:    req.Policy,
		PolicyConfig:  policyCfg,
	})
	if err != nil {
		metrics.ControlPlaneOpsTotal.WithLabelValues("start_step", "error").Inc()
		h.logger.Warn("start step failed",
			zap.String("resource_group", group),
			zap.Int64("step_id", req.StepID),
			zap.String("policy", req.Policy),
			zap.Int64("max_session_load", req.MaxSessionLoad),
			zap.Int64("max_request_load", req.MaxRequestLoad),
			zap.Error(err))
		h.writeJSON(w, http.StatusConflict, startStepResponse{
			Success: false,
			Message: err.Error(),
			StepID:  req.StepID,
		})
		return
	}
	phase, stepID := state.Phase, state.StepID
	h.notifier.BroadcastStepState(state)
	metrics.ControlPlaneOpsTotal.WithLabelValues("start_step", "ok").Inc()
	h.writeJSON(w, http.StatusOK, startStepResponse{
		Success:              true,
		StepID:               stepID,
		Phase:                phase.String(),
		Policy:               req.Policy,
		MaxSessionLoad:       req.MaxSessionLoad,
		LoadDiffThreshold:    req.LoadDiffThreshold,
		CacheThreshold:       req.CacheThreshold,
		BalanceAbsThreshold:  req.BalanceAbsThreshold,
		BalanceRelThreshold:  req.BalanceRelThreshold,
		CacheBlockSize:       req.CacheBlockSize,
		HitRatioWeight:       req.HitRatioWeight,
		LoadBalanceWeight:    req.LoadBalanceWeight,
		MaxTreeSize:          req.MaxTreeSize,
		EvictionIntervalSec:  req.EvictionIntervalSec,
		SessionEvictEpochs:   req.SessionEvictEpochs,
		SessionAffinityBoost: req.SessionAffinityBoost,
		MaxRequestLoad:       req.MaxRequestLoad,
		MaxInflight:          req.MaxInflight,
	})
}

// ---------- /v1/steps/end ----------

type endStepRequest struct {
	ResourceGroup string                             `json:"resource_group,omitempty"`
	Metadata      controlplane.ResourceGroupMetadata `json:"metadata"`
	StepID        int64                              `json:"step_id"`
}

type endStepResponse struct {
	Success         bool   `json:"success"`
	Message         string `json:"message,omitempty"`
	StepID          int64  `json:"step_id"`
	Phase           string `json:"phase"`
	PendingRequests int64  `json:"pending_requests"`
}

func (h *HTTPHandler) handleEndStep(w http.ResponseWriter, r *http.Request) {
	var req endStepRequest
	if err := jsonutil.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeJSON(w, http.StatusBadRequest, endStepResponse{
			Success: false,
			Message: "invalid request body: " + err.Error(),
		})
		return
	}

	group, _ := controlplane.ResourceGroupFromRequest(r, req.ResourceGroup, req.Metadata.ResourceGroup)
	pending, state, err := controlplane.EndStep(r.Context(), h.server, controlplane.EndStepCommand{
		ResourceGroup: group,
		StepID:        req.StepID,
	})
	if err != nil {
		metrics.ControlPlaneOpsTotal.WithLabelValues("end_step", "error").Inc()
		h.logger.Warn("end step failed",
			zap.String("resource_group", group),
			zap.Int64("step_id", req.StepID),
			zap.Error(err))
		h.writeJSON(w, controlErrorStatus(err, http.StatusConflict), endStepResponse{
			Success: false,
			Message: err.Error(),
			StepID:  req.StepID,
		})
		return
	}
	phase, stepID := state.Phase, state.StepID
	h.notifier.BroadcastStepState(state)
	metrics.ControlPlaneOpsTotal.WithLabelValues("end_step", "ok").Inc()
	h.writeJSON(w, http.StatusOK, endStepResponse{
		Success:         true,
		StepID:          stepID,
		Phase:           phase.String(),
		PendingRequests: pending,
	})
}

// ---------- /v1/steps/pause ----------

type pauseResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Paused  bool   `json:"paused"`
	StepID  int64  `json:"step_id"`
}

func (h *HTTPHandler) handlePause(w http.ResponseWriter, r *http.Request) {
	selector, err := decodeOptionalResourceGroupSelector(r)
	if err != nil {
		h.writeJSON(w, http.StatusBadRequest, pauseResponse{
			Success: false,
			Message: "invalid request body: " + err.Error(),
		})
		return
	}
	group, _ := controlplane.ResourceGroupFromRequest(r, selector.ResourceGroup, selector.Metadata.ResourceGroup)
	if err := h.server.PauseResourceGroup(r.Context(), group); err != nil {
		metrics.ControlPlaneOpsTotal.WithLabelValues("pause", "error").Inc()
		h.logger.Warn("pause failed",
			zap.String("resource_group", group),
			zap.Error(err))
		state, _ := h.server.ResourceGroupStepState(r.Context(), group)
		h.writeJSON(w, controlErrorStatus(err, http.StatusConflict), pauseResponse{
			Success: false,
			Message: err.Error(),
			Paused:  state.Paused,
			StepID:  state.StepID,
		})
		return
	}
	h.logger.Info("pause succeed",
		zap.String("resource_group", group))
	metrics.ControlPlaneOpsTotal.WithLabelValues("pause", "ok").Inc()
	state, err := h.server.ResourceGroupStepState(r.Context(), group)
	if err != nil {
		h.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	h.notifier.BroadcastStepState(state)
	h.writeJSON(w, http.StatusOK, pauseResponse{
		Success: true,
		Paused:  true,
		StepID:  state.StepID,
	})
}

// ---------- /v1/steps/continue ----------

func (h *HTTPHandler) handleContinue(w http.ResponseWriter, r *http.Request) {
	selector, err := decodeOptionalResourceGroupSelector(r)
	if err != nil {
		h.writeJSON(w, http.StatusBadRequest, pauseResponse{
			Success: false,
			Message: "invalid request body: " + err.Error(),
		})
		return
	}
	group, _ := controlplane.ResourceGroupFromRequest(r, selector.ResourceGroup, selector.Metadata.ResourceGroup)
	if err := h.server.ContinueResourceGroup(r.Context(), group); err != nil {
		metrics.ControlPlaneOpsTotal.WithLabelValues("continue", "error").Inc()
		h.logger.Warn("continue failed",
			zap.String("resource_group", group),
			zap.Error(err))
		state, _ := h.server.ResourceGroupStepState(r.Context(), group)
		h.writeJSON(w, controlErrorStatus(err, http.StatusConflict), pauseResponse{
			Success: false,
			Message: err.Error(),
			Paused:  state.Paused,
			StepID:  state.StepID,
		})
		return
	}
	h.logger.Info("continue succeed",
		zap.String("resource_group", group))
	metrics.ControlPlaneOpsTotal.WithLabelValues("continue", "ok").Inc()
	state, err := h.server.ResourceGroupStepState(r.Context(), group)
	if err != nil {
		h.writeJSON(w, controlErrorStatus(err, http.StatusServiceUnavailable), map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	h.notifier.BroadcastStepState(state)
	h.writeJSON(w, http.StatusOK, pauseResponse{
		Success: true,
		Paused:  false,
		StepID:  state.StepID,
	})
}

// ---------- /v1/steps/current ----------

type currentStepResponse struct {
	StepID             int64                          `json:"step_id"`
	Phase              string                         `json:"phase"`
	Paused             bool                           `json:"paused"`
	RegisteredGateways map[string]*domain.GatewayInfo `json:"registered_gateways,omitzero"`
}

func (h *HTTPHandler) handleGetStep(w http.ResponseWriter, r *http.Request) {
	group, scoped := controlplane.ResourceGroupFromRequest(r, "", "")
	state := h.server.StepState()
	paused := h.server.IsPaused()
	if scoped {
		var err error
		state, err = h.server.ResourceGroupStepState(r.Context(), group)
		if err != nil {
			h.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"success": false,
				"message": err.Error(),
			})
			return
		}
		paused = state.Paused
	}
	phase, stepID := state.Phase, state.StepID
	gateways := h.registry.GetAll()

	h.writeJSON(w, http.StatusOK, currentStepResponse{
		StepID:             stepID,
		Phase:              phase.String(),
		Paused:             paused,
		RegisteredGateways: gateways,
	})
}

// ---------- /v1/status ----------

type statusResponse struct {
	StepID              int64                          `json:"step_id"`
	Phase               string                         `json:"phase"`
	Paused              bool                           `json:"paused"`
	Policy              string                         `json:"policy"`
	ActiveCount         int64                          `json:"active_count"`
	PDActiveCount       int64                          `json:"pd_active_count"`
	TotalActiveCount    int64                          `json:"total_active_count"`
	AllocCounter        uint64                         `json:"alloc_counter"`
	WaitingQueueDepth   int64                          `json:"waiting_queue_depth"`
	PDWaitingQueueDepth int64                          `json:"pd_waiting_queue_depth"`
	QueueEnabled        bool                           `json:"queue_enabled"`
	Instances           []instanceStatusEntry          `json:"instances"`
	RegisteredGateways  map[string]*domain.GatewayInfo `json:"registered_gateways"`
}

type instanceStatusEntry struct {
	ID             string  `json:"id"`
	Endpoint       string  `json:"endpoint"`
	GPUNum         int     `json:"gpu_num"`
	ActiveRequests int64   `json:"active_requests"`
	Healthy        bool    `json:"healthy"`
	CircuitOpen    bool    `json:"circuit_open"`
	ActualLoad     float64 `json:"actual_load"`
}

func (h *HTTPHandler) handleStatus(w http.ResponseWriter, _ *http.Request) {
	state := h.server.StepState()
	phase, stepID := state.Phase, state.StepID
	snap := h.server.State().GetSnapshot()

	instances := make([]instanceStatusEntry, 0, len(snap))
	for _, ns := range snap {
		instances = append(instances, instanceStatusEntry{
			ID:             ns.Instance.ID,
			Endpoint:       ns.Instance.Endpoint,
			GPUNum:         ns.Instance.GPUNum,
			ActiveRequests: ns.ActiveRequests,
			Healthy:        ns.Healthy == 1,
			CircuitOpen:    ns.CircuitOpen == 1,
			ActualLoad:     ns.ActualLoad,
		})
	}

	h.writeJSON(w, http.StatusOK, statusResponse{
		StepID:              stepID,
		Phase:               phase.String(),
		Paused:              h.server.IsPaused(),
		Policy:              string(h.server.PolicyName()),
		ActiveCount:         h.server.GetActiveCount(),
		PDActiveCount:       h.server.GetPDActiveCount(),
		TotalActiveCount:    h.server.GetTotalActiveCount(),
		AllocCounter:        h.server.GetAllocCounter(),
		WaitingQueueDepth:   h.server.GetWaitingQueueDepth(),
		PDWaitingQueueDepth: h.server.GetPDWaitingQueueDepth(),
		QueueEnabled:        h.server.IsQueueEnabled(),
		Instances:           instances,
		RegisteredGateways:  h.registry.GetAll(),
	})
}

// ---------- /v1/instances ----------

// instanceEntry is the API-layer DTO for instance registration.
// Callers provide Host + Port; the system composes Endpoint internally.
type instanceEntry struct {
	ID            string            `json:"id"`
	Host          string            `json:"host"`
	Port          int               `json:"port"`
	MetricsPort   int               `json:"metrics_port,omitzero"`
	BackendType   string            `json:"backend_type,omitzero"` // "fastdeploy"(default), "sglang", "vllm"
	GPUNum        int               `json:"gpu_num"`
	TotalKVBlocks int               `json:"total_kv_blocks,omitzero"`
	ModelVersion  int64             `json:"model_version,omitzero"`
	ResourceGroup string            `json:"resource_group,omitzero"`
	Labels        map[string]string `json:"labels,omitzero"`
}

func (e *instanceEntry) validate(index int) error {
	if e.ID == "" {
		return fmt.Errorf("instances[%d]: id is required", index)
	}
	if e.Host == "" {
		return fmt.Errorf("instances[%d]: host is required", index)
	}
	if e.Port <= 0 || e.Port > 65535 {
		return fmt.Errorf("instances[%d]: port must be between 1 and 65535", index)
	}
	if e.GPUNum <= 0 {
		return fmt.Errorf("instances[%d]: gpu_num must be greater than 0", index)
	}
	return nil
}

func (e *instanceEntry) toDomain() *domain.Instance {
	metricsPort := e.MetricsPort
	if metricsPort == 0 {
		metricsPort = e.Port
	}
	backendType := e.BackendType
	if backendType == "" {
		backendType = "fastdeploy"
	}
	return &domain.Instance{
		ID:              e.ID,
		Endpoint:        net.JoinHostPort(e.Host, strconv.Itoa(e.Port)),
		MetricsEndpoint: net.JoinHostPort(e.Host, strconv.Itoa(metricsPort)),
		BackendType:     backendType,
		GPUNum:          e.GPUNum,
		TotalKVBlocks:   e.TotalKVBlocks,
		ModelVersion:    e.ModelVersion,
		ResourceGroup:   e.ResourceGroup,
		Labels:          e.Labels,
	}
}

type registerInstancesRequest struct {
	ResourceGroup string                             `json:"resource_group,omitempty"`
	Metadata      controlplane.ResourceGroupMetadata `json:"metadata"`
	Instances     []instanceEntry                    `json:"instances"`
}

type registerInstancesResponse struct {
	Success         bool `json:"success"`
	RegisteredCount int  `json:"registered_count"`
}

type unregisterInstancesRequest struct {
	ResourceGroup string                             `json:"resource_group,omitempty"`
	Metadata      controlplane.ResourceGroupMetadata `json:"metadata"`
	IDs           []string                           `json:"ids"`
}

type unregisterInstancesResponse struct {
	Success           bool `json:"success"`
	UnregisteredCount int  `json:"unregistered_count"`
}

type syncInstancesResponse struct {
	Success bool `json:"success"`
	Added   int  `json:"added"`
	Removed int  `json:"removed"`
	Updated int  `json:"updated"`
}

type listInstancesResponse struct {
	Instances []*domain.Instance `json:"instances"`
}

func (h *HTTPHandler) handleRegisterInstances(w http.ResponseWriter, r *http.Request) {
	instances, selector, ok := h.decodeInstances(w, r)
	if !ok {
		return
	}
	instances = h.enrichSGLangInstances(r.Context(), instances)

	group, scoped := controlplane.ResourceGroupFromRequest(r, selector.ResourceGroup, selector.Metadata.ResourceGroup)
	if err := controlplane.ValidateScopedInstanceResourceGroups(instances, group, scoped); err != nil {
		h.writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	var err error
	if scoped {
		err = h.server.RegisterInstancesForResourceGroup(r.Context(), group, instances)
	} else {
		err = h.server.RegisterInstances(r.Context(), instances)
	}
	if err != nil {
		h.writeJSON(w, controlErrorStatus(err, http.StatusServiceUnavailable), map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	metrics.ControlPlaneOpsTotal.WithLabelValues("register_instances", "ok").Inc()
	h.logger.Info("instances registered via API",
		zap.String("resource_group", group),
		zap.Bool("resource_group_scoped", scoped),
		zap.Int("count", len(instances)))
	h.writeJSON(w, http.StatusOK, registerInstancesResponse{
		Success:         true,
		RegisteredCount: len(instances),
	})
}

func (h *HTTPHandler) handleUnregisterInstances(w http.ResponseWriter, r *http.Request) {
	var req unregisterInstancesRequest
	if err := jsonutil.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeJSON(w, http.StatusBadRequest, unregisterInstancesResponse{Success: false})
		return
	}
	if len(req.IDs) == 0 {
		h.writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "ids array is required and must not be empty",
		})
		return
	}

	group, scoped := controlplane.ResourceGroupFromRequest(r, req.ResourceGroup, req.Metadata.ResourceGroup)
	removed := 0
	var err error
	if scoped {
		removed, err = h.server.UnregisterInstancesForResourceGroup(r.Context(), group, req.IDs)
	} else {
		removed, err = h.server.UnregisterInstances(r.Context(), req.IDs)
	}
	if err != nil {
		h.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	h.logger.Info("instances unregistered via API",
		zap.String("resource_group", group),
		zap.Bool("resource_group_scoped", scoped),
		zap.Int("requested", len(req.IDs)),
		zap.Int("removed", removed))
	// Idempotent: always return success, even if some IDs were already absent.
	h.writeJSON(w, http.StatusOK, unregisterInstancesResponse{
		Success:           true,
		UnregisteredCount: removed,
	})
}

func (h *HTTPHandler) handleListInstances(w http.ResponseWriter, r *http.Request) {
	group, scoped := controlplane.ResourceGroupFromRequest(r, "", "")
	instances := h.server.State().List()
	if scoped {
		instances = h.server.State().ListByResourceGroup(group)
	}
	h.writeJSON(w, http.StatusOK, listInstancesResponse{Instances: instances})
}

func (h *HTTPHandler) handleSyncInstances(w http.ResponseWriter, r *http.Request) {
	instances, selector, ok := h.decodeInstances(w, r)
	if !ok {
		return
	}

	instances = h.enrichSGLangInstances(r.Context(), instances)

	group, scoped := controlplane.ResourceGroupFromRequest(r, selector.ResourceGroup, selector.Metadata.ResourceGroup)
	if err := controlplane.ValidateScopedInstanceResourceGroups(instances, group, scoped); err != nil {
		h.writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	result, err := controlplane.SyncInstances(r.Context(), h.server, controlplane.SyncInstancesCommand{
		ResourceGroup: group,
		Scoped:        scoped,
		Instances:     instances,
	})
	if err != nil {
		h.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	metrics.ControlPlaneOpsTotal.WithLabelValues("sync_instances", "ok").Inc()
	h.logger.Info("instances synced via API",
		zap.String("resource_group", group),
		zap.Bool("resource_group_scoped", scoped),
		zap.Int("added", result.Added),
		zap.Int("removed", result.Removed),
		zap.Int("updated", result.Updated))
	h.writeJSON(w, http.StatusOK, syncInstancesResponse{
		Success: true,
		Added:   result.Added,
		Removed: result.Removed,
		Updated: result.Updated,
	})
}

// ---------- /v1/resource-groups ----------

type listResourceGroupsResponse struct {
	ResourceGroups []string `json:"resource_groups"`
}

func (h *HTTPHandler) handleListResourceGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.server.ResourceGroups(r.Context())
	if err != nil {
		h.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	h.writeJSON(w, http.StatusOK, listResourceGroupsResponse{ResourceGroups: groups})
}

func (h *HTTPHandler) decodeInstances(w http.ResponseWriter, r *http.Request) ([]*domain.Instance, registerInstancesRequest, bool) {
	var req registerInstancesRequest
	if err := jsonutil.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "invalid request body: " + err.Error(),
		})
		return nil, registerInstancesRequest{}, false
	}
	instances := make([]*domain.Instance, 0, len(req.Instances))
	for i := range req.Instances {
		if err := req.Instances[i].validate(i); err != nil {
			h.writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": err.Error(),
			})
			return nil, registerInstancesRequest{}, false
		}
		instances = append(instances, req.Instances[i].toDomain())
	}
	return instances, req, true
}

// ---------- /v1/admin/waiting-queue ----------

type waitingQueueDiagResponse struct {
	QueueEnabled     bool   `json:"queue_enabled"`
	QueueDepth       int64  `json:"queue_depth"`
	QueueMaxSize     int64  `json:"queue_max_size"`
	QueueTimeoutSec  int64  `json:"queue_timeout_sec"`
	StepID           int64  `json:"step_id"`
	Phase            string `json:"phase"`
	ActiveCount      int64  `json:"active_count"`
	PDActiveCount    int64  `json:"pd_active_count"`
	TotalActiveCount int64  `json:"total_active_count"`
	EventChannelLen  int    `json:"event_channel_len"`
}

func (h *HTTPHandler) handleWaitingQueueDiag(w http.ResponseWriter, _ *http.Request) {
	state := h.server.StepState()
	phase, stepID := state.Phase, state.StepID
	h.writeJSON(w, http.StatusOK, waitingQueueDiagResponse{
		QueueEnabled:     h.server.IsQueueEnabled(),
		QueueDepth:       h.server.GetWaitingQueueDepth(),
		QueueMaxSize:     h.server.GetQueueMaxSize(),
		QueueTimeoutSec:  h.server.GetQueueTimeoutSec(),
		StepID:           stepID,
		Phase:            phase.String(),
		ActiveCount:      h.server.GetActiveCount(),
		PDActiveCount:    h.server.GetPDActiveCount(),
		TotalActiveCount: h.server.GetTotalActiveCount(),
		EventChannelLen:  len(h.server.eventCh),
	})
}

// ---------- helpers ----------

// fetchSGLangDPSize fetches /server_info from a sglang instance and returns dp_size.
// Returns 0 if the fetch fails or dp_size is absent (non-fatal, logged as warning).
func (h *HTTPHandler) fetchSGLangDPSize(ctx context.Context, endpoint string) int {
	url := "http://" + endpoint + "/server_info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.logger.Warn("failed to create server_info request",
			zap.String("endpoint", endpoint),
			zap.Error(err))
		return 0
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		h.logger.Warn("failed to fetch server_info from sglang instance",
			zap.String("endpoint", endpoint),
			zap.String("url", url),
			zap.Error(err))
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.logger.Warn("sglang server_info returned non-200",
			zap.String("endpoint", endpoint),
			zap.Int("status_code", resp.StatusCode))
		return 0
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.logger.Warn("failed to read server_info response body",
			zap.String("endpoint", endpoint),
			zap.Error(err))
		return 0
	}

	var info struct {
		DPSize int `json:"dp_size"`
	}
	if err := jsonutil.Unmarshal(body, &info); err != nil {
		h.logger.Warn("failed to parse server_info JSON",
			zap.String("endpoint", endpoint),
			zap.Error(err))
		return 0
	}

	h.logger.Info("fetched dp_size from sglang server_info",
		zap.String("endpoint", endpoint),
		zap.Int("dp_size", info.DPSize))
	return info.DPSize
}

// enrichSGLangInstances fetches dp_size from sglang instances and expands multi-DP
// instances into dp_size separate domain.Instance entries with distinct dp_rank values.
// Non-sglang or single-DP instances get dp_rank = -1.
func (h *HTTPHandler) enrichSGLangInstances(ctx context.Context, instances []*domain.Instance) []*domain.Instance {
	result := make([]*domain.Instance, 0, len(instances))
	for _, inst := range instances {
		if inst.BackendType != "sglang" {
			inst.DPSize = 1
			inst.DPRank = -1
			result = append(result, inst)
			continue
		}
		if dpSize := h.fetchSGLangDPSize(ctx, inst.Endpoint); dpSize > 1 {
			inst.DPSize = dpSize
		} else {
			inst.DPSize = 1
		}
		if inst.DPSize <= 1 {
			inst.DPRank = -1
			result = append(result, inst)
			continue
		}
		// Expand into dp_size instances with dp_rank 0..dp_size-1.
		for rank := range inst.DPSize {
			expanded := *inst // shallow copy
			expanded.DPRank = rank
			expanded.ID = fmt.Sprintf("%s_dp%d", inst.ID, rank)
			result = append(result, &expanded)
		}
		h.logger.Info("expanded sglang multi-DP instance",
			zap.String("original_id", inst.ID),
			zap.Int("dp_size", inst.DPSize))
	}
	return result
}

func (h *HTTPHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := jsonutil.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("failed to encode JSON response",
			zap.Error(err))
	}
}

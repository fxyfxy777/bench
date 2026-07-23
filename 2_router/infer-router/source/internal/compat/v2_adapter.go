package compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/controlplane"
	"github.com/yzx/rl-router/internal/domain"
	stepnotifier "github.com/yzx/rl-router/internal/scheduler/notifier"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/pkg/jsonutil"
	routerlogger "github.com/yzx/rl-router/pkg/logger"
)

// SchedulerAPI is the subset of scheduler.Server that V2Adapter uses.
// Defined here (consumer side) to keep the scheduler package unaware of compat.
type SchedulerAPI interface {
	controlplane.StepScheduler
	controlplane.InstanceSyncer
	RemoveSession(ctx context.Context, sessionID string)
}

type resourceGroupSessionRemover interface {
	RemoveSessionForResourceGroup(ctx context.Context, resourceGroup, sessionID string)
}

type resourceGroupIdleWaiter interface {
	WaitResourceGroupIdle(ctx context.Context, resourceGroup string) (domain.StepState, error)
}

// V2Adapter provides backward-compatible `/api/v2/*` routes that map to
// the existing RL-Router scheduler internals. This allows callers that
// still use the rollout-controller V2 API (InferenceRouterAgent, training
// frameworks) to work without modification.
type V2Adapter struct {
	scheduler  SchedulerAPI
	notifier   *stepnotifier.StepNotifier
	logger     *zap.Logger
	httpClient *http.Client // used for fetching backend server_info (e.g. sglang dp_size)

	// currentStepID caches the step_id from start_infer so that
	// stop_infer (which sends no body) can look it up per resource group.
	// Missing map entries mean no active cached step; 0 is a valid step ID.
	currentStepMu  sync.RWMutex
	currentStepIDs map[string]int64
}

// NewV2Adapter creates a new V2 compatibility adapter.
func NewV2Adapter(s SchedulerAPI, n *stepnotifier.StepNotifier, logger *zap.Logger) *V2Adapter {
	a := &V2Adapter{
		scheduler: s,
		notifier:  n,
		logger:    logger,
		currentStepIDs: map[string]int64{
			domain.DefaultResourceGroup: -1,
		},
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
	return a
}

// RegisterRoutes adds the V2 compat routes to the given mux.
// chatHandler may be nil if the gateway is not configured (scheduler-only mode).
func (a *V2Adapter) RegisterRoutes(mux *http.ServeMux, chatHandler http.HandlerFunc) {
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("POST /api/v2/start_infer", a.logHTTP("POST /api/v2/start_infer", a.handleStartInfer))
	mux.HandleFunc("POST /api/v2/stop_infer", a.logHTTP("POST /api/v2/stop_infer", a.handleStopInfer))
	mux.HandleFunc("PUT /api/v2/instances", a.logHTTP("PUT /api/v2/instances", a.handleSyncInstances))
	mux.HandleFunc("POST /api/v2/session_finish", a.logHTTP("POST /api/v2/session_finish", a.handleSessionFinish))
	if chatHandler != nil {
		mux.HandleFunc("POST /api/v2/chat/completions", chatHandler)
	}
}

func (a *V2Adapter) logHTTP(route string, next http.HandlerFunc) http.HandlerFunc {
	return routerlogger.ControlPlaneHTTPLogHandler(a.logger, route, next)
}

// RegisterV2ChatRoute registers only the /api/v2/chat/completions route.
// This is used in pure gateway mode where no scheduler is available.
func RegisterV2ChatRoute(mux *http.ServeMux, chatHandler http.HandlerFunc) {
	if chatHandler != nil {
		mux.HandleFunc("POST /api/v2/chat/completions", chatHandler)
	}
}

// RegisterV2GatewayRoutes registers /health and /api/v2/chat/completions for
// pure gateway mode. The /health endpoint returns a simple 200 OK so that the
// PaddleRL InferenceRouterAgent health check works consistently across all
// deployment modes (hybrid, scheduler, gateway).
func RegisterV2GatewayRoutes(mux *http.ServeMux, chatHandler http.HandlerFunc) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	})
	if chatHandler != nil {
		mux.HandleFunc("POST /api/v2/chat/completions", chatHandler)
	}
}

// ---------- v2Status response envelope ----------

type v2Status struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type v2Response struct {
	Status v2Status `json:"status"`
}

func (a *V2Adapter) writeV2Response(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // RC always returns HTTP 200
	_ = jsonutil.NewEncoder(w).Encode(v2Response{
		Status: v2Status{Code: code, Message: msg},
	})
}

func (a *V2Adapter) cachedStepID(resourceGroup string) int64 {
	resourceGroup = domain.NormalizeResourceGroup(resourceGroup)
	a.currentStepMu.RLock()
	stepID, ok := a.currentStepIDs[resourceGroup]
	a.currentStepMu.RUnlock()
	if !ok {
		return -1
	}
	return stepID
}

func (a *V2Adapter) setCachedStepID(resourceGroup string, stepID int64) {
	resourceGroup = domain.NormalizeResourceGroup(resourceGroup)
	a.currentStepMu.Lock()
	a.currentStepIDs[resourceGroup] = stepID
	a.currentStepMu.Unlock()
}

func (a *V2Adapter) clearCachedStepID(resourceGroup string) {
	a.setCachedStepID(resourceGroup, -1)
}

// ---------- GET /health ----------

func (a *V2Adapter) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// ---------- POST /api/v2/start_infer ----------

type startInferRequest struct {
	ResourceGroup     string                             `json:"resource_group,omitempty"`
	Metadata          controlplane.ResourceGroupMetadata `json:"metadata"`
	ModelVersion      *int64                             `json:"model_version"`
	LoadBalancePolicy string                             `json:"load_balance_policy,omitempty"`
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
	// Session-aware V5 block-aware thresholds.
	BlockMinThreshold    int64 `json:"block_min_threshold,omitempty"`    // 新 session 门控阈值 (default 256)
	BlockEmergencyThresh int64 `json:"block_emergency_thresh,omitempty"` // 紧急迁移阈值 (default 128)

	// PD separation (prefill/decode disaggregation)
	PrefillPolicy string `json:"prefill_policy,omitempty"`
	DecodePolicy  string `json:"decode_policy,omitempty"`
}

func (a *V2Adapter) handleStartInfer(w http.ResponseWriter, r *http.Request) {
	var req startInferRequest
	if err := jsonutil.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeV2Response(w, 1, "invalid request body: "+err.Error())
		return
	}

	if req.ModelVersion == nil {
		a.writeV2Response(w, 1, "model_version is required")
		return
	}
	stepID := *req.ModelVersion
	if stepID < 0 {
		a.writeV2Response(w, 1, "model_version must be >= 0")
		return
	}

	policyCfg := policy.PolicyConfig{
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
		// Session-aware V5 block-aware thresholds.
		BlockMinThreshold:    req.BlockMinThreshold,
		BlockEmergencyThresh: req.BlockEmergencyThresh,
		// PD separation policies — passed through to scheduler event-loop.
		PDPrefillPolicy: req.PrefillPolicy,
		PDDecodePolicy:  req.DecodePolicy,
	}
	group, _ := controlplane.ResourceGroupFromRequest(r, req.ResourceGroup, req.Metadata.ResourceGroup)
	state, err := controlplane.StartStep(r.Context(), a.scheduler, controlplane.StartStepCommand{
		ResourceGroup: group,
		StepID:        stepID,
		PolicyName:    req.LoadBalancePolicy,
		PolicyConfig:  policyCfg,
	})
	if err != nil {
		a.logger.Warn("v2 start_infer failed",
			zap.String("resource_group", group),
			zap.Int64("model_version", stepID),
			zap.String("load_balance_policy", req.LoadBalancePolicy),
			zap.Int64("max_session_load", req.MaxSessionLoad),
			zap.Int64("load_diff_threshold", req.LoadDiffThreshold),
			zap.Error(err))
		a.writeV2Response(w, 1, err.Error())
		return
	}

	// Cache step_id for stop_infer and broadcast state change.
	a.setCachedStepID(group, stepID)
	a.notifier.BroadcastStepState(state)

	a.writeV2Response(w, 0, "success")
}

// ---------- POST /api/v2/stop_infer ----------

type stopInferRequest struct {
	ResourceGroup string                             `json:"resource_group,omitempty"`
	Metadata      controlplane.ResourceGroupMetadata `json:"metadata"`
	StepID        *int64                             `json:"step_id,omitempty"`
	ModelVersion  *int64                             `json:"model_version,omitempty"`
}

func (a *V2Adapter) handleStopInfer(w http.ResponseWriter, r *http.Request) {
	var req stopInferRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		a.writeV2Response(w, 1, "invalid request body: "+err.Error())
		return
	}

	group, _ := controlplane.ResourceGroupFromRequest(r, req.ResourceGroup, req.Metadata.ResourceGroup)
	stepID := a.stopInferStepID(group, req)
	if stepID < 0 {
		a.writeV2Response(w, 1, "no active step to stop")
		return
	}

	// Validate current phase before attempting EndStep.
	state, err := a.scheduler.ResourceGroupStepState(r.Context(), group)
	if err != nil {
		a.writeV2Response(w, 1, "query step state failed: "+err.Error())
		return
	}
	phase, currentStep := state.Phase, state.StepID
	if currentStep != stepID {
		a.writeV2Response(w, 1,
			fmt.Sprintf("cannot stop: resource_group=%s current_step=%d cached_step=%d",
				group, currentStep, stepID))
		return
	}
	if phase != domain.StepServing && phase != domain.StepDraining && phase != domain.StepIdle {
		a.writeV2Response(w, 1,
			fmt.Sprintf("cannot stop: resource_group=%s phase=%s current_step=%d cached_step=%d",
				group, phase, currentStep, stepID))
		return
	}

	var pending int64
	if phase == domain.StepServing {
		pending, state, err = controlplane.EndStep(r.Context(), a.scheduler, controlplane.EndStepCommand{
			ResourceGroup: group,
			StepID:        stepID,
		})
		if err != nil {
			a.logger.Warn("v2 stop_infer failed",
				zap.String("resource_group", group),
				zap.Int64("step_id", stepID),
				zap.Error(err))
			a.writeV2Response(w, 1, err.Error())
			return
		}
		if pending > 0 {
			a.logger.Info("v2 stop_infer waiting for draining requests",
				zap.String("resource_group", group),
				zap.Int64("step_id", stepID),
				zap.Int64("pending_requests", pending))
		}
	}

	wasDraining := state.Phase == domain.StepDraining
	a.notifier.BroadcastStepState(state)
	state, err = a.waitForDrainComplete(r.Context(), group, state)
	if err != nil {
		a.logger.Warn("v2 stop_infer drain wait failed",
			zap.String("resource_group", group),
			zap.Int64("step_id", stepID),
			zap.Error(err))
		a.writeV2Response(w, 1, err.Error())
		return
	}
	a.clearCachedStepID(group)
	if wasDraining && state.Phase == domain.StepIdle {
		a.notifier.BroadcastStepState(state)
	}

	a.writeV2Response(w, 0, "success")
}

func (a *V2Adapter) waitForDrainComplete(ctx context.Context, group string, state domain.StepState) (domain.StepState, error) {
	if state.Phase != domain.StepDraining {
		return state, nil
	}
	waiter, ok := a.scheduler.(resourceGroupIdleWaiter)
	if !ok {
		return state, nil
	}
	finalState, err := waiter.WaitResourceGroupIdle(ctx, group)
	if err != nil {
		return state, fmt.Errorf("wait for drain complete: %w", err)
	}
	return finalState, nil
}

func (a *V2Adapter) stopInferStepID(group string, req stopInferRequest) int64 {
	if req.StepID != nil {
		return *req.StepID
	}
	if req.ModelVersion != nil {
		return *req.ModelVersion
	}
	return a.cachedStepID(group)
}

func decodeOptionalJSON(r *http.Request, v any) error {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	if err := jsonutil.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// ---------- PUT /api/v2/instances ----------

// v2Instance matches the rollout-controller instance JSON schema.
type v2Instance struct {
	ID             string            `json:"id"`
	Host           string            `json:"host"`
	HostIP         string            `json:"host_ip,omitempty"`
	InferPort      int               `json:"infer_port"`
	Port           int               `json:"port,omitempty"`
	MetricsPort    int               `json:"metrics_port"`
	GPUNum         int               `json:"gpu_num"`
	TokenPerBlocks int               `json:"token_per_blocks"` // ignored
	TotalKVBlocks  int               `json:"total_kv_blocks,omitempty"`
	ModelVersion   int64             `json:"model_version,omitempty"`
	BackendType    string            `json:"backend_type,omitempty"`
	ResourceType   string            `json:"resource_type,omitempty"`
	ResourceGroup  string            `json:"resource_group,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`

	// PD separation (splitwise) fields.
	Role             string   `json:"role,omitempty"`
	ConnectorPort    int      `json:"connector_port"`
	TransferProtocol []string `json:"transfer_protocol,omitempty"`
	RDMAPorts        []string `json:"rdma_ports,omitempty"`
	DeviceIDs        []string `json:"device_ids,omitempty"`
	TpSize           int      `json:"tp_size,omitempty"`
}

type v2SyncRequest struct {
	ResourceGroup string                             `json:"resource_group,omitempty"`
	Metadata      controlplane.ResourceGroupMetadata `json:"metadata"`
	Instances     []v2Instance                       `json:"instances"`
}

func (a *V2Adapter) handleSyncInstances(w http.ResponseWriter, r *http.Request) {
	var req v2SyncRequest
	if err := jsonutil.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeV2Response(w, 1, "invalid request body: "+err.Error())
		return
	}

	instances := make([]*domain.Instance, 0, len(req.Instances))
	for i, inst := range req.Instances {
		if inst.ID == "" {
			a.writeV2Response(w, 1, fmt.Sprintf("instances[%d]: id is required", i))
			return
		}
		host := inst.Host
		if host == "" {
			host = inst.HostIP
		}
		if host == "" {
			a.writeV2Response(w, 1, fmt.Sprintf("instances[%d]: host is required", i))
			return
		}
		inferPort := inst.InferPort
		if inferPort <= 0 {
			inferPort = inst.Port
		}
		if inferPort <= 0 || inferPort > 65535 {
			a.writeV2Response(w, 1, fmt.Sprintf("instances[%d]: infer_port must be between 1 and 65535", i))
			return
		}

		gpuNum := inst.GPUNum
		if gpuNum <= 0 {
			gpuNum = 1
		}

		metricsPort := inst.MetricsPort
		if metricsPort <= 0 {
			metricsPort = inferPort
		}

		connectorPort := ""
		if inst.ConnectorPort > 0 {
			connectorPort = strconv.Itoa(inst.ConnectorPort)
		}

		backendType := inst.BackendType
		if backendType == "" {
			backendType = "fastdeploy"
		}

		labels := normalizeV2InstanceLabels(inst)
		instances = append(instances, &domain.Instance{
			ID:               inst.ID,
			Host:             host,
			Endpoint:         net.JoinHostPort(host, strconv.Itoa(inferPort)),
			MetricsEndpoint:  net.JoinHostPort(host, strconv.Itoa(metricsPort)),
			BackendType:      backendType,
			GPUNum:           gpuNum,
			TotalKVBlocks:    inst.TotalKVBlocks,
			ModelVersion:     inst.ModelVersion,
			ResourceGroup:    inst.ResourceGroup,
			Labels:           labels,
			ConnectorPort:    connectorPort,
			Role:             inst.Role,
			TransferProtocol: inst.TransferProtocol,
			RDMAPorts:        inst.RDMAPorts,
			DeviceIDs:        inst.DeviceIDs,
			TpSize:           inst.TpSize,
		})
	}

	instances = a.enrichSGLangInstances(r.Context(), instances)

	group, scoped := controlplane.ResourceGroupFromRequest(r, req.ResourceGroup, req.Metadata.ResourceGroup)
	if err := controlplane.ValidateScopedInstanceResourceGroups(instances, group, scoped); err != nil {
		a.writeV2Response(w, 1, err.Error())
		return
	}
	if _, err := controlplane.SyncInstances(r.Context(), a.scheduler, controlplane.SyncInstancesCommand{
		ResourceGroup: group,
		Scoped:        scoped,
		Instances:     instances,
	}); err != nil {
		a.writeV2Response(w, 1, "sync instances failed: "+err.Error())
		return
	}
	a.logger.Info("v2 instances synced",
		zap.String("resource_group", group),
		zap.Bool("resource_group_scoped", scoped),
		zap.Int("count", len(instances)))
	for _, ins := range instances {
		a.logger.Info("v2 instance synced",
			zap.String("instance_id", ins.ID),
			zap.String("backend_type", ins.BackendType),
			zap.String("host", ins.Host),
			zap.String("endpoint", ins.Endpoint),
			zap.String("resource_group", domain.NormalizeResourceGroup(ins.ResourceGroup)),
			zap.String("resource_type", ins.Labels[domain.LabelResourceType]),
			zap.String("metrics_port", ins.MetricsEndpoint),
			zap.String("connector_port", ins.ConnectorPort),
			zap.String("role", ins.Role),
			zap.Int("tp_size", ins.TpSize),
		)
	}
	a.writeV2Response(w, 0, "success")
}

func normalizeV2InstanceLabels(inst v2Instance) map[string]string {
	labels := make(map[string]string, len(inst.Labels)+1)
	maps.Copy(labels, inst.Labels)
	if inst.ResourceType != "" {
		labels[domain.LabelResourceType] = inst.ResourceType
	}
	return labels
}

// ---------- POST /api/v2/session_finish ----------

type sessionFinishRequest struct {
	SessionID     string                             `json:"session_id"`
	ResourceGroup string                             `json:"resource_group,omitempty"`
	Metadata      controlplane.ResourceGroupMetadata `json:"metadata"`
}

func (a *V2Adapter) handleSessionFinish(w http.ResponseWriter, r *http.Request) {
	var req sessionFinishRequest
	if err := jsonutil.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeV2Response(w, 1, "invalid request body: "+err.Error())
		return
	}

	if req.SessionID == "" {
		a.writeV2Response(w, 1, "session_id is required")
		return
	}

	group, _ := controlplane.ResourceGroupFromRequest(r, req.ResourceGroup, req.Metadata.ResourceGroup)
	if remover, ok := a.scheduler.(resourceGroupSessionRemover); ok {
		remover.RemoveSessionForResourceGroup(r.Context(), group, req.SessionID)
	} else {
		a.scheduler.RemoveSession(r.Context(), req.SessionID)
	}
	a.logger.Info("v2 session finished",
		zap.String("resource_group", group),
		zap.String("session_id", req.SessionID))
	a.writeV2Response(w, 0, "success")
}

// ---------- sglang multi-DP enrichment ----------

// enrichSGLangInstances expands sglang instances with dp_size > 1 into
// multiple virtual instances (one per DP rank), mirroring the V1 logic.
func (a *V2Adapter) enrichSGLangInstances(ctx context.Context, instances []*domain.Instance) []*domain.Instance {
	result := make([]*domain.Instance, 0, len(instances))
	for _, inst := range instances {
		if inst.BackendType != "sglang" {
			inst.DPSize = 1
			inst.DPRank = -1
			result = append(result, inst)
			continue
		}
		if dpSize := a.fetchSGLangDPSize(ctx, inst.Endpoint); dpSize > 1 {
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
		a.logger.Info("expanded sglang multi-DP instance",
			zap.String("original_id", inst.ID),
			zap.Int("dp_size", inst.DPSize))
	}
	return result
}

// fetchSGLangDPSize fetches /server_info from a sglang instance and returns dp_size.
// Returns 0 if the fetch fails or dp_size is absent (non-fatal, logged as warning).
func (a *V2Adapter) fetchSGLangDPSize(ctx context.Context, endpoint string) int {
	url := "http://" + endpoint + "/server_info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		a.logger.Warn("failed to create server_info request",
			zap.String("endpoint", endpoint),
			zap.Error(err))
		return 0
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		a.logger.Warn("failed to fetch server_info from sglang instance",
			zap.String("endpoint", endpoint),
			zap.String("url", url),
			zap.Error(err))
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		a.logger.Warn("sglang server_info returned non-200",
			zap.String("endpoint", endpoint),
			zap.Int("status_code", resp.StatusCode))
		return 0
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		a.logger.Warn("failed to read server_info response body",
			zap.String("endpoint", endpoint),
			zap.Error(err))
		return 0
	}

	var info struct {
		DPSize int `json:"dp_size"`
	}
	if err := jsonutil.Unmarshal(body, &info); err != nil {
		a.logger.Warn("failed to parse server_info JSON",
			zap.String("endpoint", endpoint),
			zap.Error(err))
		return 0
	}

	a.logger.Info("fetched dp_size from sglang server_info",
		zap.String("endpoint", endpoint),
		zap.Int("dp_size", info.DPSize))
	return info.DPSize
}

package scheduler

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yzx/rl-router/api/proto/routerpb"
	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/internal/scheduler/registry"
)

// GRPCHandler implements routerpb.SchedulerServiceServer.
type GRPCHandler struct {
	routerpb.UnimplementedSchedulerServiceServer
	server   *Server
	registry *registry.GatewayRegistry
	logger   *zap.Logger
}

func NewGRPCHandler(server *Server, registry *registry.GatewayRegistry, logger *zap.Logger) *GRPCHandler {
	return &GRPCHandler{
		server:   server,
		registry: registry,
		logger:   logger,
	}
}

func schedulerGRPCStatus(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, ErrGlobalRateLimitExceeded):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, ErrResourceGroupRateLimitExceeded):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, ErrWaitingQueueFull):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, ErrWaitingQueueTimeout):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, ErrPaused):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, ErrUnknownResourceGroup):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrResourceGroupInstanceConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, policy.ErrNoInstances),
		errors.Is(err, policy.ErrAllOverloaded),
		errors.Is(err, policy.ErrAllExceedSession):
		return status.Error(codes.Unavailable, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func (h *GRPCHandler) Allocate(ctx context.Context, req *routerpb.AllocateRequest) (*routerpb.AllocateResponse, error) {
	routeCtx := domain.AcquireRouteContext()
	routeCtx.TraceID = req.GetTraceId()
	routeCtx.RequestID = req.GetRequestId()
	routeCtx.GatewayID = req.GetGatewayId()
	routeCtx.ResourceGroup = req.GetResourceGroup()
	routeCtx.Labels = req.GetLabels()
	routeCtx.SessionID = req.GetSessionId()
	routeCtx.RequestText = req.GetRequestText()
	routeCtx.RequestTokenIDs = int32sToInts(req.GetRequestTokenIds())

	inst, allocID, err := h.server.Allocate(ctx, routeCtx)
	if err != nil {
		return nil, schedulerGRPCStatus(err)
	}

	return &routerpb.AllocateResponse{
		InstanceId:   inst.ID,
		Endpoint:     inst.Endpoint,
		AllocationId: allocID,
	}, nil
}

func (h *GRPCHandler) Release(ctx context.Context, req *routerpb.ReleaseRequest) (*routerpb.ReleaseResponse, error) {
	costMetrics := domain.AcquireCostMetrics()
	costMetrics.DurationMs = req.GetDurationMs()
	costMetrics.GPUUsage = req.GetGpuUsage()
	costMetrics.ErrorCode = req.GetErrorCode()
	costMetrics.PromptTokens = req.GetPromptTokens()
	costMetrics.CompletionTokens = req.GetCompletionTokens()
	costMetrics.TotalTokens = req.GetTotalTokens()

	if err := h.server.Release(ctx, req.GetInstanceId(), req.GetGatewayId(), req.GetAllocationId(), costMetrics); err != nil {
		return nil, err
	}
	return &routerpb.ReleaseResponse{}, nil
}

func (h *GRPCHandler) Register(ctx context.Context, req *routerpb.RegisterRequest) (*routerpb.RegisterResponse, error) {
	h.registry.Register(req.GetGatewayAddr(), req.GetLabels())

	state := h.server.StepState()
	states, err := h.server.ResourceGroupStates(ctx)
	if err != nil {
		h.logger.Warn("failed to collect resource group states for register",
			zap.String("gateway_addr", req.GetGatewayAddr()),
			zap.Error(err))
		states = []domain.StepState{state}
	}
	phase, stepID := state.Phase, state.StepID
	return &routerpb.RegisterResponse{
		Success:             true,
		Message:             "registered",
		Phase:               routerpb.StepPhase(phase),
		StepId:              stepID,
		ResourceGroupStates: stepStatesToProto(states),
	}, nil
}

func (h *GRPCHandler) Heartbeat(ctx context.Context, req *routerpb.HeartbeatRequest) (*routerpb.HeartbeatResponse, error) {
	ok := h.registry.Heartbeat(req.GetGatewayId(), req.GetActiveConnections())

	// Process piggybacked pending releases (bounded per-heartbeat by the gateway).
	if pending := req.GetPendingReleases(); len(pending) > 0 {
		h.processPendingReleases(req.GetGatewayId(), pending)
	}

	state := h.server.StepState()
	states, err := h.server.ResourceGroupStates(ctx)
	if err != nil {
		h.logger.Warn("failed to collect resource group states for heartbeat",
			zap.String("gateway_id", req.GetGatewayId()),
			zap.Error(err))
		states = []domain.StepState{state}
	}
	phase, stepID := state.Phase, state.StepID
	return &routerpb.HeartbeatResponse{
		Success:             ok,
		Phase:               routerpb.StepPhase(phase),
		StepId:              stepID,
		ResourceGroupStates: stepStatesToProto(states),
	}, nil
}

func stepStatesToProto(states []domain.StepState) []*routerpb.ResourceGroupStepState {
	out := make([]*routerpb.ResourceGroupStepState, 0, len(states))
	for _, state := range states {
		group := state.ResourceGroup
		if group == "" {
			group = domain.DefaultResourceGroup
		}
		out = append(out, &routerpb.ResourceGroupStepState{
			ResourceGroup: group,
			Phase:         routerpb.StepPhase(state.Phase),
			StepId:        state.StepID,
			Paused:        state.Paused,
		})
	}
	return out
}

// processPendingReleases submits piggybacked releases to the event loop.
// Uses a short-lived context to avoid blocking the heartbeat handler under backpressure.
func (h *GRPCHandler) processPendingReleases(gatewayID string, pending []*routerpb.PendingRelease) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	delivered := 0
	for _, pr := range pending {
		var err error
		if pr.GetRole() == "" {
			err = h.server.Release(ctx, pr.GetInstanceId(), pr.GetGatewayId(), pr.GetAllocationId(), nil)
		} else {
			err = h.server.ReleasePD(ctx, pr.GetInstanceId(), pr.GetGatewayId(), pr.GetAllocationId(), pr.GetRole(), 0, "")
		}
		if err != nil {
			h.logger.Warn("pending release submit failed, remaining will retry next heartbeat",
				zap.String("gateway_id", gatewayID),
				zap.String("allocation_id", pr.GetAllocationId()),
				zap.Int("delivered", delivered),
				zap.Int("remaining", len(pending)-delivered),
				zap.Error(err))
			break
		}
		delivered++
	}
	if delivered > 0 {
		h.logger.Info("processed piggybacked pending releases",
			zap.String("gateway_id", gatewayID),
			zap.Int("count", delivered))
	}
}

func (h *GRPCHandler) AllocatePD(ctx context.Context, req *routerpb.AllocatePDRequest) (*routerpb.AllocatePDResponse, error) {
	routeCtx := domain.AcquireRouteContext()
	routeCtx.TraceID = req.GetTraceId()
	routeCtx.RequestID = req.GetRequestId()
	routeCtx.GatewayID = req.GetGatewayId()
	routeCtx.ResourceGroup = req.GetResourceGroup()
	routeCtx.Labels = req.GetLabels()
	routeCtx.SessionID = req.GetSessionId()
	routeCtx.RequestText = req.GetRequestText()
	routeCtx.RequestTokenIDs = int32sToInts(req.GetRequestTokenIds())

	prefill, decode, prefillAllocID, decodeAllocID, err := h.server.AllocatePD(ctx, routeCtx)
	if err != nil {
		return nil, schedulerGRPCStatus(err)
	}

	return &routerpb.AllocatePDResponse{
		Prefill:             instanceToPDInfo(prefill),
		Decode:              instanceToPDInfo(decode),
		PrefillAllocationId: prefillAllocID,
		DecodeAllocationId:  decodeAllocID,
	}, nil
}

func (h *GRPCHandler) ReleasePD(_ context.Context, req *routerpb.ReleasePDRequest) (*routerpb.ReleasePDResponse, error) {
	if err := h.server.ReleasePD(
		context.Background(),
		req.GetInstanceId(),
		req.GetGatewayId(),
		req.GetAllocationId(),
		req.GetRole(),
		req.GetDurationMs(),
		req.GetErrorCode(),
	); err != nil {
		if errors.Is(err, ErrInvalidPDRelease) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, err
	}
	return &routerpb.ReleasePDResponse{}, nil
}

// instanceToPDInfo converts a domain.Instance to a protobuf PDInstanceInfo.
func instanceToPDInfo(inst *domain.Instance) *routerpb.PDInstanceInfo {
	if inst == nil {
		return nil
	}
	return &routerpb.PDInstanceInfo{
		InstanceId:       inst.ID,
		Endpoint:         inst.Endpoint,
		Host:             inst.Host,
		TransferProtocol: inst.TransferProtocol,
		RdmaPorts:        inst.RDMAPorts,
		DeviceIds:        inst.DeviceIDs,
		TpSize:           int32(inst.TpSize),
		ConnectorPort:    inst.ConnectorPort,
	}
}

// int32sToInts converts []int32 from protobuf to []int for policy code.
func int32sToInts(ids []int32) []int {
	if len(ids) == 0 {
		return nil
	}
	out := make([]int, len(ids))
	for i, v := range ids {
		out[i] = int(v)
	}
	return out
}

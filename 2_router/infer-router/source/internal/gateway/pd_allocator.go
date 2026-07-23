package gateway

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/yzx/rl-router/api/proto/routerpb"
	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
	"github.com/yzx/rl-router/pkg/tracing"
)

// PDAllocator abstracts PD instance pair allocation for the gateway.
// - In hybrid mode: LocalPDAllocator calls the scheduler directly in-process.
// - In gateway mode: RemotePDAllocator calls the scheduler via gRPC.
type PDAllocator interface {
	AllocatePD(ctx context.Context, req *domain.RouteContext) (prefill, decode *domain.Instance, prefillAllocID, decodeAllocID string, err error)
	ReleasePD(ctx context.Context, instanceID, gatewayID, allocationID, role string, durationMs int64, errorCode string) error
}

// PDPoolResourceOwner indicates that the PD allocator takes ownership of
// pool-acquired RouteContext values. LocalPDAllocator returns true because the
// scheduler event-loop releases the context after processing the event; remote
// gRPC allocation leaves ownership with the gateway caller.
type PDPoolResourceOwner interface {
	OwnsPDPoolResources() bool
}

// LocalPDAllocator calls the scheduler in-process for PD allocation (hybrid mode).
type LocalPDAllocator struct {
	allocateFn func(ctx context.Context, req *domain.RouteContext) (prefill, decode *domain.Instance, prefillAllocID, decodeAllocID string, err error)
	releaseFn  func(ctx context.Context, instanceID, gatewayID, allocationID, role string, durationMs int64, errorCode string) error
}

func NewLocalPDAllocator(
	allocateFn func(ctx context.Context, req *domain.RouteContext) (prefill, decode *domain.Instance, prefillAllocID, decodeAllocID string, err error),
	releaseFn func(ctx context.Context, instanceID, gatewayID, allocationID, role string, durationMs int64, errorCode string) error,
) *LocalPDAllocator {
	return &LocalPDAllocator{allocateFn: allocateFn, releaseFn: releaseFn}
}

func (a *LocalPDAllocator) AllocatePD(ctx context.Context, req *domain.RouteContext) (prefill, decode *domain.Instance, prefillAllocID, decodeAllocID string, err error) {
	return a.allocateFn(ctx, req)
}

func (a *LocalPDAllocator) ReleasePD(ctx context.Context, instanceID, gatewayID, allocationID, role string, durationMs int64, errorCode string) error {
	return a.releaseFn(ctx, instanceID, gatewayID, allocationID, role, durationMs, errorCode)
}

func (a *LocalPDAllocator) OwnsPDPoolResources() bool {
	return true
}

// RemotePDAllocator calls the scheduler via gRPC for PD allocation (gateway-only mode).
type RemotePDAllocator struct {
	client          routerpb.SchedulerServiceClient
	logger          *zap.Logger
	rpcTimeout      time.Duration
	allocateTimeout time.Duration
}

// NewRemotePDAllocator creates a RemotePDAllocator that reuses an existing gRPC connection.
func NewRemotePDAllocator(conn *grpc.ClientConn, log *zap.Logger) *RemotePDAllocator {
	return &RemotePDAllocator{
		client:          routerpb.NewSchedulerServiceClient(conn),
		logger:          log,
		rpcTimeout:      defaultSchedulerRPCTimeout,
		allocateTimeout: defaultSchedulerAllocateTimeout,
	}
}

// SetRPCTimeout overrides the timeout used for short scheduler RPCs.
func (a *RemotePDAllocator) SetRPCTimeout(d time.Duration) {
	a.rpcTimeout = d
}

// SetAllocateTimeout overrides the timeout used for scheduler AllocatePD RPCs.
func (a *RemotePDAllocator) SetAllocateTimeout(d time.Duration) {
	a.allocateTimeout = d
}

func (a *RemotePDAllocator) AllocatePD(ctx context.Context, req *domain.RouteContext) (prefill, decode *domain.Instance, prefillAllocID, decodeAllocID string, err error) {
	ctx, span := tracing.StartSpan(ctx, "gateway.rpc.allocate_pd")
	defer span.End()

	start := time.Now()
	ctx, cancel := schedulerRPCContext(ctx, a.allocateTimeout)
	defer cancel()
	resp, rpcErr := a.client.AllocatePD(ctx, &routerpb.AllocatePDRequest{
		TraceId:         req.TraceID,
		GatewayId:       req.GatewayID,
		RequestId:       req.RequestID,
		ResourceGroup:   req.ResourceGroup,
		SessionId:       req.SessionID,
		RequestText:     req.RequestText,
		RequestTokenIds: intsToInt32s(req.RequestTokenIDs),
		Labels:          req.Labels,
	})
	elapsed := time.Since(start)
	metrics.RemoteAllocLatencyMs.WithLabelValues("allocate_pd").Observe(float64(elapsed.Microseconds()) / 1000.0)

	if rpcErr != nil {
		a.logger.Error("remote allocate_pd failed",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			zap.String("trace_id", req.TraceID),
			zap.Duration("rpc_latency", elapsed),
			zap.Error(rpcErr))
		return nil, nil, "", "", fmt.Errorf("remote allocate_pd: %w", rpcErr)
	}

	prefill = pdInfoToInstance(resp.GetPrefill())
	decode = pdInfoToInstance(resp.GetDecode())

	if prefill == nil || decode == nil {
		a.logger.Error("remote allocate_pd returned incomplete response",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			zap.String("trace_id", req.TraceID),
			zap.Bool("prefill_nil", prefill == nil),
			zap.Bool("decode_nil", decode == nil),
			zap.Duration("rpc_latency", elapsed))
		return nil, nil, "", "", fmt.Errorf("remote allocate_pd: scheduler returned nil instance (prefill_nil=%v, decode_nil=%v)", prefill == nil, decode == nil)
	}

	a.logger.Debug("remote allocate_pd succeeded",
		logger.Event(logger.EventAllocate),
		logger.Status(logger.StatusOK),
		zap.String("trace_id", req.TraceID),
		zap.String("prefill_id", prefill.ID),
		zap.String("decode_id", decode.ID),
		zap.Duration("rpc_latency", elapsed))

	return prefill, decode, resp.GetPrefillAllocationId(), resp.GetDecodeAllocationId(), nil
}

func (a *RemotePDAllocator) ReleasePD(ctx context.Context, instanceID, gatewayID, allocationID, role string, durationMs int64, errorCode string) error {
	ctx, span := tracing.StartSpan(ctx, "gateway.rpc.release_pd")
	defer span.End()

	start := time.Now()
	ctx, cancel := schedulerRPCContext(ctx, a.rpcTimeout)
	defer cancel()
	_, rpcErr := a.client.ReleasePD(ctx, &routerpb.ReleasePDRequest{
		InstanceId:   instanceID,
		GatewayId:    gatewayID,
		AllocationId: allocationID,
		Role:         role,
		DurationMs:   durationMs,
		ErrorCode:    errorCode,
	})
	elapsed := time.Since(start)
	metrics.RemoteAllocLatencyMs.WithLabelValues("release_pd").Observe(float64(elapsed.Microseconds()) / 1000.0)

	if rpcErr != nil {
		a.logger.Error("remote release_pd failed",
			logger.Event(logger.EventRelease),
			logger.Status(logger.StatusFail),
			zap.String("allocation_id", allocationID),
			zap.String("role", role),
			zap.Duration("rpc_latency", elapsed),
			zap.Error(rpcErr))
		return fmt.Errorf("remote release_pd: %w", rpcErr)
	}
	return nil
}

// pdInfoToInstance converts a protobuf PDInstanceInfo to a domain.Instance.
func pdInfoToInstance(info *routerpb.PDInstanceInfo) *domain.Instance {
	if info == nil {
		return nil
	}
	return &domain.Instance{
		ID:               info.GetInstanceId(),
		Endpoint:         info.GetEndpoint(),
		Host:             info.GetHost(),
		TransferProtocol: info.GetTransferProtocol(),
		RDMAPorts:        info.GetRdmaPorts(),
		DeviceIDs:        info.GetDeviceIds(),
		TpSize:           int(info.GetTpSize()),
		ConnectorPort:    info.GetConnectorPort(),
	}
}

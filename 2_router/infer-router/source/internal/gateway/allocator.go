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

// InstanceAllocator abstracts instance allocation for the gateway.
// - In hybrid mode: LocalAllocator calls the scheduler directly in-process.
// - In gateway mode: RemoteAllocator calls the scheduler via gRPC.
type InstanceAllocator interface {
	Allocate(ctx context.Context, req *domain.RouteContext) (*domain.Instance, string, error)
	Release(ctx context.Context, instanceID string, gatewayID string, allocationID string, m *domain.CostMetrics) error
}

// PoolResourceOwner indicates that the allocator takes ownership of pool-acquired
// objects (RouteContext, CostMetrics) and is responsible for releasing them.
// LocalAllocator implements this because the scheduler event-loop handles release.
// RemoteAllocator does NOT implement this — the caller must release after use.
type PoolResourceOwner interface {
	OwnsPoolResources() bool
}

// LocalAllocator calls the scheduler in-process (used in hybrid mode).
type LocalAllocator struct {
	allocateFn func(ctx context.Context, req *domain.RouteContext) (*domain.Instance, string, error)
	releaseFn  func(ctx context.Context, id string, gatewayID string, allocationID string, m *domain.CostMetrics) error
}

// OwnsPoolResources returns true because the scheduler event-loop releases
// RouteContext and CostMetrics after processing.
func (c *LocalAllocator) OwnsPoolResources() bool { return true }

func NewLocalAllocator(
	allocateFn func(ctx context.Context, req *domain.RouteContext) (*domain.Instance, string, error),
	releaseFn func(ctx context.Context, id string, gatewayID string, allocationID string, m *domain.CostMetrics) error,
) *LocalAllocator {
	return &LocalAllocator{allocateFn: allocateFn, releaseFn: releaseFn}
}

func (c *LocalAllocator) Allocate(ctx context.Context, req *domain.RouteContext) (*domain.Instance, string, error) {
	return c.allocateFn(ctx, req)
}

func (c *LocalAllocator) Release(ctx context.Context, instanceID string, gatewayID string, allocationID string, m *domain.CostMetrics) error {
	return c.releaseFn(ctx, instanceID, gatewayID, allocationID, m)
}

// RemoteAllocator calls the scheduler via gRPC (used in gateway-only mode).
type RemoteAllocator struct {
	client          routerpb.SchedulerServiceClient
	logger          *zap.Logger
	rpcTimeout      time.Duration
	allocateTimeout time.Duration
}

// NewRemoteAllocator creates a RemoteAllocator that reuses an existing gRPC connection.
func NewRemoteAllocator(conn *grpc.ClientConn, log *zap.Logger) *RemoteAllocator {
	return newRemoteAllocator(routerpb.NewSchedulerServiceClient(conn), log)
}

// newRemoteAllocator is the internal constructor that accepts a pre-built gRPC client.
// Tests call this directly with a mock client.
func newRemoteAllocator(client routerpb.SchedulerServiceClient, log *zap.Logger) *RemoteAllocator {
	return &RemoteAllocator{
		client:          client,
		logger:          log,
		rpcTimeout:      defaultSchedulerRPCTimeout,
		allocateTimeout: defaultSchedulerAllocateTimeout,
	}
}

// SetRPCTimeout overrides the timeout used for short scheduler RPCs.
func (c *RemoteAllocator) SetRPCTimeout(d time.Duration) {
	c.rpcTimeout = d
}

// SetAllocateTimeout overrides the timeout used for scheduler Allocate RPCs.
func (c *RemoteAllocator) SetAllocateTimeout(d time.Duration) {
	c.allocateTimeout = d
}

func (c *RemoteAllocator) Allocate(ctx context.Context, req *domain.RouteContext) (*domain.Instance, string, error) {
	ctx, span := tracing.StartSpan(ctx, "gateway.rpc.allocate")
	defer span.End()

	start := time.Now()
	ctx, cancel := schedulerRPCContext(ctx, c.allocateTimeout)
	defer cancel()
	resp, err := c.client.Allocate(ctx, &routerpb.AllocateRequest{
		TraceId:         req.TraceID,
		RequestId:       req.RequestID,
		ResourceGroup:   req.ResourceGroup,
		Labels:          req.Labels,
		GatewayId:       req.GatewayID,
		SessionId:       req.SessionID,
		RequestText:     req.RequestText,
		RequestTokenIds: intsToInt32s(req.RequestTokenIDs),
	})
	elapsed := time.Since(start)
	metrics.RemoteAllocLatencyMs.WithLabelValues("allocate").Observe(float64(elapsed.Microseconds()) / 1000.0)

	if err != nil {
		c.logger.Error("remote allocate failed",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			zap.String("trace_id", req.TraceID),
			zap.Duration("rpc_latency", elapsed),
			zap.Error(err))
		return nil, "", fmt.Errorf("remote allocate: %w", err)
	}

	c.logger.Debug("remote allocate succeeded",
		logger.Event(logger.EventAllocate),
		logger.Status(logger.StatusOK),
		zap.String("trace_id", req.TraceID),
		zap.String("instance_id", resp.GetInstanceId()),
		zap.Duration("rpc_latency", elapsed))

	return &domain.Instance{
		ID:       resp.GetInstanceId(),
		Endpoint: resp.GetEndpoint(),
	}, resp.GetAllocationId(), nil
}

func (c *RemoteAllocator) Release(ctx context.Context, instanceID string, gatewayID string, allocationID string, m *domain.CostMetrics) error {
	ctx, span := tracing.StartSpan(ctx, "gateway.rpc.release")
	defer span.End()

	start := time.Now()
	ctx, cancel := schedulerRPCContext(ctx, c.rpcTimeout)
	defer cancel()
	_, err := c.client.Release(ctx, &routerpb.ReleaseRequest{
		InstanceId:       instanceID,
		GatewayId:        gatewayID,
		AllocationId:     allocationID,
		DurationMs:       m.DurationMs,
		GpuUsage:         m.GPUUsage,
		ErrorCode:        m.ErrorCode,
		PromptTokens:     m.PromptTokens,
		CompletionTokens: m.CompletionTokens,
		TotalTokens:      m.TotalTokens,
	})
	elapsed := time.Since(start)
	metrics.RemoteAllocLatencyMs.WithLabelValues("release").Observe(float64(elapsed.Microseconds()) / 1000.0)

	if err != nil {
		c.logger.Error("remote release failed",
			logger.Event(logger.EventRelease),
			logger.Status(logger.StatusFail),
			zap.String("gateway_addr", gatewayID),
			zap.String("instance", instanceID),
			zap.String("allocation_id", allocationID),
			zap.Duration("rpc_latency", elapsed),
			zap.Error(err))
		return fmt.Errorf("remote release: %w", err)
	}
	return nil
}

func schedulerRPCContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

// intsToInt32s converts []int to []int32 for protobuf encoding.
func intsToInt32s(ids []int) []int32 {
	if len(ids) == 0 {
		return nil
	}
	out := make([]int32, len(ids))
	for i, v := range ids {
		out[i] = int32(v)
	}
	return out
}

package gateway

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/yzx/rl-router/api/proto/routerpb"
	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/metrics"
)

// SchedulerClient manages the gateway's communication with the scheduler,
// including registration, periodic heartbeat, and step-state synchronization.
// The gatewayAddr serves as both the unique identity and the push-notification target.
type SchedulerClient struct {
	gatewayAddr  string
	interval     time.Duration
	conn         *grpc.ClientConn
	client       routerpb.SchedulerServiceClient
	stepPhase    atomic.Int32
	stepID       atomic.Int64
	paused       atomic.Bool
	stepStates   atomic.Value // map[string]domain.StepState, replaced copy-on-write
	stepStatesMu sync.Mutex
	registered   atomic.Bool
	logger       *zap.Logger
	rpcTimeout   time.Duration

	// pendingReleases holds releases that exhausted retries and await heartbeat piggyback.
	pendingMu       sync.Mutex
	pendingReleases []PendingRelease
}

// maxPendingReleases bounds the queue. If a gateway accumulates this many failed
// releases, heartbeat expiry will catch the rest.
const maxPendingReleases = 256

// maxPendingPerHeartbeat limits how many pending releases are sent per heartbeat
// to avoid blocking the scheduler's heartbeat handler under backpressure.
const maxPendingPerHeartbeat = 32

func NewSchedulerClient(gatewayAddr, schedulerAddr string, interval time.Duration, logger *zap.Logger, dialOpts ...grpc.DialOption) (*SchedulerClient, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	opts = append(opts, dialOpts...)

	conn, err := grpc.NewClient(schedulerAddr, opts...)
	if err != nil {
		return nil, fmt.Errorf("scheduler_client: failed to connect to scheduler at %s: %w", schedulerAddr, err)
	}

	return newSchedulerClient(gatewayAddr, interval, conn, routerpb.NewSchedulerServiceClient(conn), logger), nil
}

// newSchedulerClient is the internal constructor that accepts a pre-built gRPC client.
// Exported constructor wraps this; tests can call it directly with a mock client.
func newSchedulerClient(gatewayAddr string, interval time.Duration, conn *grpc.ClientConn, client routerpb.SchedulerServiceClient, logger *zap.Logger) *SchedulerClient {
	sc := &SchedulerClient{
		gatewayAddr: gatewayAddr,
		interval:    interval,
		conn:        conn,
		client:      client,
		logger:      logger,
		rpcTimeout:  defaultSchedulerRPCTimeout,
	}
	// Default to IDLE until we hear from the scheduler.
	sc.stepPhase.Store(int32(domain.StepIdle))
	sc.storeStepStates(map[string]domain.StepState{
		domain.DefaultResourceGroup: {ResourceGroup: domain.DefaultResourceGroup, Phase: domain.StepIdle},
	})
	return sc
}

// SetRPCTimeout overrides the timeout applied to scheduler RPCs.
func (sc *SchedulerClient) SetRPCTimeout(d time.Duration) {
	sc.rpcTimeout = d
}

// Start registers with the scheduler (with retries) and starts the heartbeat loop.
// Blocks until registration succeeds or ctx is cancelled.
func (sc *SchedulerClient) Start(ctx context.Context) error {
	if err := sc.registerWithRetry(ctx); err != nil {
		return err
	}
	go sc.heartbeatLoop(ctx)
	return nil
}

func (sc *SchedulerClient) registerWithRetry(ctx context.Context) error {
	backoff := 500 * time.Millisecond
	maxBackoff := 10 * time.Second

	for {
		rpcCtx, cancel := schedulerRPCContext(ctx, sc.rpcTimeout)
		resp, err := sc.client.Register(rpcCtx, &routerpb.RegisterRequest{
			GatewayId:   sc.gatewayAddr,
			GatewayAddr: sc.gatewayAddr,
		})
		cancel()
		if err == nil && resp.GetSuccess() {
			sc.updateStepStatesFromRegister(resp)
			sc.registered.Store(true)
			sc.logger.Info("registered with scheduler",
				zap.String("gateway_addr", sc.gatewayAddr),
				zap.String("phase", domain.StepPhase(resp.GetPhase()).String()),
				zap.Int64("step_id", resp.GetStepId()))
			return nil
		}

		if err != nil {
			sc.logger.Warn("register failed, retrying",
				zap.String("gateway_addr", sc.gatewayAddr),
				zap.Error(err),
				zap.Duration("backoff", backoff))
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("scheduler_client: registration cancelled for gateway %s: %w", sc.gatewayAddr, ctx.Err())
		case <-timer.C:
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// jitteredInterval returns interval ± 20% random jitter.
func jitteredInterval(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	jitter := time.Duration(rand.Int64N(int64(base) * 2 / 5)) // [0, 40% of base)
	return base - base/5 + jitter                             // base ± 20%
}

func (sc *SchedulerClient) heartbeatLoop(ctx context.Context) {
	// Initial random delay in [0, interval) to stagger heartbeats across gateways.
	if sc.interval > 0 {
		initTimer := time.NewTimer(time.Duration(rand.Int64N(int64(sc.interval))))
		select {
		case <-ctx.Done():
			initTimer.Stop()
			return
		case <-initTimer.C:
		}
	}

	consecutiveFailures := 0
	const reRegisterThreshold = 3

	timer := time.NewTimer(jitteredInterval(sc.interval))
	defer timer.Stop()

	for {
		sc.sendHeartbeat(ctx, &consecutiveFailures, reRegisterThreshold)

		timer.Reset(jitteredInterval(sc.interval))
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (sc *SchedulerClient) sendHeartbeat(ctx context.Context, consecutiveFailures *int, reRegisterThreshold int) {
	drained := sc.drainPendingReleases()

	rpcCtx, cancel := schedulerRPCContext(ctx, sc.rpcTimeout)
	req := &routerpb.HeartbeatRequest{
		GatewayId:   sc.gatewayAddr,
		TimestampMs: time.Now().UnixMilli(),
	}
	if len(drained) > 0 {
		req.PendingReleases = make([]*routerpb.PendingRelease, len(drained))
		for i, r := range drained {
			req.PendingReleases[i] = &routerpb.PendingRelease{
				InstanceId:   r.InstanceID,
				GatewayId:    r.GatewayAddr,
				AllocationId: r.AllocationID,
				Role:         r.Role,
			}
		}
	}
	resp, err := sc.client.Heartbeat(rpcCtx, req)
	cancel()

	if err != nil {
		*consecutiveFailures++
		if len(drained) > 0 {
			sc.requeuePendingReleases(drained)
		}
		sc.logger.Warn("heartbeat failed",
			zap.String("gateway_addr", sc.gatewayAddr),
			zap.Int("consecutive_failures", *consecutiveFailures),
			zap.Error(err))

		if *consecutiveFailures >= reRegisterThreshold {
			sc.registered.Store(false)
			sc.logger.Info("too many heartbeat failures, attempting re-register",
				zap.String("gateway_addr", sc.gatewayAddr))
			if regErr := sc.registerWithRetry(ctx); regErr != nil {
				sc.logger.Error("re-register failed",
					zap.String("gateway_addr", sc.gatewayAddr),
					zap.Error(regErr))
				return
			}
			*consecutiveFailures = 0
		}
		return
	}

	*consecutiveFailures = 0
	sc.updateStepStatesFromHeartbeat(resp)

	if len(drained) > 0 {
		metrics.PendingReleaseDelivered.Add(float64(len(drained)))
		sc.logger.Info("piggybacked pending releases on heartbeat",
			zap.String("gateway_addr", sc.gatewayAddr),
			zap.Int("count", len(drained)))
	}

	if !resp.GetSuccess() {
		sc.logger.Warn("heartbeat rejected by scheduler, attempting re-register",
			zap.String("gateway_addr", sc.gatewayAddr))
		if regErr := sc.registerWithRetry(ctx); regErr != nil {
			sc.logger.Error("re-register failed after heartbeat rejection",
				zap.String("gateway_addr", sc.gatewayAddr),
				zap.Error(regErr))
			return
		}
	}
}

// IsServing returns true if the cached scheduler state is SERVING.
func (sc *SchedulerClient) IsServing() bool {
	return domain.StepPhase(sc.stepPhase.Load()) == domain.StepServing
}

// IsPaused returns true if the cached scheduler state indicates allocation is paused.
func (sc *SchedulerClient) IsPaused() bool {
	return sc.paused.Load()
}

// IsConnected returns true after the client has completed scheduler
// registration. It is intentionally independent from the scheduler step phase.
func (sc *SchedulerClient) IsConnected() bool {
	return sc.registered.Load()
}

// UpdateStepState updates the cached step state from an external push notification.
// Called by the HTTP notification endpoint to achieve near-instant state sync.
func (sc *SchedulerClient) UpdateStepState(state domain.StepState) {
	state = normalizeStepStateResourceGroup(state)
	sc.mergeStepState(state)
	sc.logger.Info("step state updated via push notification",
		zap.String("gateway_addr", sc.gatewayAddr),
		zap.String("resource_group", state.ResourceGroup),
		zap.String("phase", state.Phase.String()),
		zap.Int64("step_id", state.StepID),
		zap.Bool("paused", state.Paused))
}

func (sc *SchedulerClient) ResourceGroupStepState(resourceGroup string) (domain.StepState, bool) {
	resourceGroup = normalizeResourceGroup(resourceGroup)
	states := sc.loadStepStates()
	if state, ok := states[resourceGroup]; ok {
		return normalizeStepStateResourceGroup(state), true
	}
	if resourceGroup == domain.DefaultResourceGroup {
		return domain.StepState{
			ResourceGroup: domain.DefaultResourceGroup,
			Phase:         domain.StepPhase(sc.stepPhase.Load()),
			StepID:        sc.stepID.Load(),
			Paused:        sc.paused.Load(),
		}, true
	}
	return domain.StepState{ResourceGroup: resourceGroup, Phase: domain.StepIdle}, false
}

// GatewayAddr returns the gateway's advertise address, which also serves as its identity.
func (sc *SchedulerClient) GatewayAddr() string {
	return sc.gatewayAddr
}

// GetStepInfo returns the cached step phase and step ID.
func (sc *SchedulerClient) GetStepInfo() (domain.StepPhase, int64) {
	return domain.StepPhase(sc.stepPhase.Load()), sc.stepID.Load()
}

func (sc *SchedulerClient) updateStepStatesFromRegister(resp *routerpb.RegisterResponse) {
	sc.updateDefaultState(domain.StepState{
		ResourceGroup: domain.DefaultResourceGroup,
		Phase:         domain.StepPhase(resp.GetPhase()),
		StepID:        resp.GetStepId(),
		Paused:        sc.paused.Load(),
	})
	sc.replaceStepStates(protoStepStatesToDomain(resp.GetResourceGroupStates()))
}

func (sc *SchedulerClient) updateStepStatesFromHeartbeat(resp *routerpb.HeartbeatResponse) {
	sc.updateDefaultState(domain.StepState{
		ResourceGroup: domain.DefaultResourceGroup,
		Phase:         domain.StepPhase(resp.GetPhase()),
		StepID:        resp.GetStepId(),
		Paused:        sc.paused.Load(),
	})
	sc.replaceStepStates(protoStepStatesToDomain(resp.GetResourceGroupStates()))
}

func (sc *SchedulerClient) updateDefaultState(state domain.StepState) {
	state = normalizeStepStateResourceGroup(state)
	sc.stepPhase.Store(int32(state.Phase))
	sc.stepID.Store(state.StepID)
	sc.paused.Store(state.Paused)
	sc.mergeStepState(state)
}

func (sc *SchedulerClient) replaceStepStates(states []domain.StepState) {
	sc.stepStatesMu.Lock()
	defer sc.stepStatesMu.Unlock()

	next := make(map[string]domain.StepState, len(states)+1)
	for _, state := range states {
		state = normalizeStepStateResourceGroup(state)
		next[state.ResourceGroup] = state
	}
	if _, ok := next[domain.DefaultResourceGroup]; !ok {
		next[domain.DefaultResourceGroup] = domain.StepState{
			ResourceGroup: domain.DefaultResourceGroup,
			Phase:         domain.StepPhase(sc.stepPhase.Load()),
			StepID:        sc.stepID.Load(),
			Paused:        sc.paused.Load(),
		}
	}
	sc.storeStepStates(next)
	if state, ok := next[domain.DefaultResourceGroup]; ok {
		sc.stepPhase.Store(int32(state.Phase))
		sc.stepID.Store(state.StepID)
		sc.paused.Store(state.Paused)
	}
}

func (sc *SchedulerClient) mergeStepState(state domain.StepState) {
	state = normalizeStepStateResourceGroup(state)
	sc.stepStatesMu.Lock()
	defer sc.stepStatesMu.Unlock()

	old := sc.loadStepStates()
	next := make(map[string]domain.StepState, len(old)+1)
	maps.Copy(next, old)
	next[state.ResourceGroup] = state
	sc.storeStepStates(next)
	if state.ResourceGroup == domain.DefaultResourceGroup {
		sc.stepPhase.Store(int32(state.Phase))
		sc.stepID.Store(state.StepID)
		sc.paused.Store(state.Paused)
	}
}

func (sc *SchedulerClient) loadStepStates() map[string]domain.StepState {
	if v := sc.stepStates.Load(); v != nil {
		if states, ok := v.(map[string]domain.StepState); ok {
			return states
		}
	}
	return nil
}

func (sc *SchedulerClient) storeStepStates(states map[string]domain.StepState) {
	sc.stepStates.Store(states)
}

func protoStepStatesToDomain(states []*routerpb.ResourceGroupStepState) []domain.StepState {
	out := make([]domain.StepState, 0, len(states))
	for _, state := range states {
		if state == nil {
			continue
		}
		out = append(out, domain.StepState{
			ResourceGroup: normalizeResourceGroup(state.GetResourceGroup()),
			Phase:         domain.StepPhase(state.GetPhase()),
			StepID:        state.GetStepId(),
			Paused:        state.GetPaused(),
		})
	}
	return out
}

func normalizeStepStateResourceGroup(state domain.StepState) domain.StepState {
	state.ResourceGroup = normalizeResourceGroup(state.ResourceGroup)
	return state
}

func normalizeResourceGroup(resourceGroup string) string {
	if resourceGroup == "" {
		return domain.DefaultResourceGroup
	}
	return resourceGroup
}

// Conn returns the underlying gRPC connection for reuse by RemoteAllocator.
func (sc *SchedulerClient) Conn() *grpc.ClientConn {
	return sc.conn
}

// Stop closes the gRPC connection.
func (sc *SchedulerClient) Stop() {
	sc.registered.Store(false)
	if sc.conn != nil {
		_ = sc.conn.Close()
	}
}

// ---------- Pending Release Queue ----------

// PendingRelease represents a release that failed all retries and awaits
// heartbeat piggyback for guaranteed delivery. Exported for cross-package use.
type PendingRelease struct {
	InstanceID   string
	GatewayAddr  string
	AllocationID string
	Role         string // empty for normal; "prefill"/"decode" for PD
}

// EnqueuePendingRelease parks a release that failed all retries for piggyback
// on the next heartbeat. Thread-safe: called from request goroutines.
func (sc *SchedulerClient) EnqueuePendingRelease(rel PendingRelease) {
	sc.pendingMu.Lock()
	defer sc.pendingMu.Unlock()
	if len(sc.pendingReleases) >= maxPendingReleases {
		dropped := sc.pendingReleases[0]
		sc.pendingReleases = sc.pendingReleases[1:]
		sc.logger.Warn("pending release queue full, dropping oldest entry",
			zap.String("gateway_addr", sc.gatewayAddr),
			zap.String("dropped_allocation_id", dropped.AllocationID))
	}
	sc.pendingReleases = append(sc.pendingReleases, rel)
	metrics.PendingReleaseEnqueued.Inc()
	sc.logger.Warn("pending release enqueued for heartbeat retry",
		zap.String("gateway_addr", sc.gatewayAddr),
		zap.String("instance", rel.InstanceID),
		zap.String("release_gateway_addr", rel.GatewayAddr),
		zap.String("allocation_id", rel.AllocationID),
		zap.String("role", rel.Role),
		zap.Int("pending_release_depth", len(sc.pendingReleases)))
}

// drainPendingReleases atomically takes up to maxPendingPerHeartbeat releases
// for the current heartbeat. Remaining entries stay for subsequent heartbeats.
func (sc *SchedulerClient) drainPendingReleases() []PendingRelease {
	sc.pendingMu.Lock()
	defer sc.pendingMu.Unlock()
	if len(sc.pendingReleases) == 0 {
		return nil
	}
	n := min(len(sc.pendingReleases), maxPendingPerHeartbeat)
	drained := make([]PendingRelease, n)
	copy(drained, sc.pendingReleases[:n])
	sc.pendingReleases = sc.pendingReleases[n:]
	return drained
}

// requeuePendingReleases re-inserts releases when heartbeat itself failed.
func (sc *SchedulerClient) requeuePendingReleases(releases []PendingRelease) {
	sc.pendingMu.Lock()
	defer sc.pendingMu.Unlock()
	sc.pendingReleases = append(releases, sc.pendingReleases...)
	if len(sc.pendingReleases) > maxPendingReleases {
		sc.pendingReleases = sc.pendingReleases[:maxPendingReleases]
	}
}

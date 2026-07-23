package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/config"
	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/circuitbreaker"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/internal/scheduler/store"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
	"github.com/yzx/rl-router/pkg/tracing"
)

// ErrGlobalRateLimitExceeded is returned when the scheduler's global in-flight
// cap (globalMaxInflight) has been reached. Gateway should map this to HTTP 429.
var ErrGlobalRateLimitExceeded = errors.New("global rate limit exceeded: max inflight allocations reached")

// ErrResourceGroupRateLimitExceeded is returned when a resource group's
// max_inflight cap has been reached.
var ErrResourceGroupRateLimitExceeded = errors.New("resource group rate limit exceeded: max inflight allocations reached")

// ErrSchedulerStopped is returned when callers submit work after the event-loop
// has stopped. Gateways should treat this as a transient scheduler outage.
var ErrSchedulerStopped = errors.New("scheduler stopped")

// ErrUnknownResourceGroup is returned when a scoped control-plane operation
// targets a resource group that has no runtime state.
var ErrUnknownResourceGroup = errors.New("unknown resource group")

// ErrResourceGroupInstanceConflict is returned when a scoped instance mutation
// attempts to move an existing instance ID from one resource group to another.
var ErrResourceGroupInstanceConflict = errors.New("instance id already belongs to another resource group")

// ErrWaitingQueueFull is returned when the waiting queue has reached its max_size.
var ErrWaitingQueueFull = errors.New("scheduler waiting queue is full")

// ErrWaitingQueueTimeout is returned when a queued request exceeds its queue timeout.
var ErrWaitingQueueTimeout = errors.New("waiting queue timeout")

// ErrPaused is returned when allocation is paused via /v1/steps/pause.
// Gateway should return finish_reason=abort to the client instead of an HTTP error.
var ErrPaused = errors.New("scheduler allocation paused")

// ErrInvalidPDRelease is returned when a ReleasePD request is malformed and
// must not mutate scheduler state.
var ErrInvalidPDRelease = errors.New("invalid pd release")

// Server is the scheduler control-plane service.
// All state mutations (Select + Acquire, Release + Feedback, Step transitions)
// are serialized through a single event-loop goroutine, ensuring atomic
// allocation decisions without explicit locking.
type Server struct {
	eventCh     chan *event
	stopCh      chan struct{}
	submitMu    sync.RWMutex // prevents new submissions from racing behind Stop's sentinel event
	stopping    atomic.Bool
	state       *store.NodeStateStore
	policy      policy.Policy
	policyName  atomic.Value
	groups      map[string]*groupRuntime
	activeGroup *groupRuntime
	logger      *zap.Logger // control-plane: step lifecycle, gateway events, errors
	accessLog   *zap.Logger // request-path: per-request allocate/release/batch stats

	// Step state — written only by the event-loop, read externally via atomics.
	stepPhase         atomic.Int32 // domain.StepPhase
	stepID            atomic.Int64
	activeCount       int64         // in-flight allocations in current step (event-loop only)
	activeCountAtomic atomic.Int64  // mirrors activeCount for external readers (monitoring)
	stepDoneCh        chan struct{} // closed when DRAINING → IDLE transition completes
	lastEndedStepID   int64         // last successfully ended step ID, for EndStep idempotency

	// gatewayAllocs tracks per-gateway active allocations for ghost load cleanup.
	// Key: gatewayAddr, Value: map[allocationID]allocation metadata. Written only by the event-loop.
	gatewayAllocs map[string]map[string]gatewayAllocation

	// Idempotency dedup state — written only by the event-loop, reset each step.
	allocCounter       uint64                     // step-scoped monotonic counter for allocationID generation
	allocCounterAtomic atomic.Uint64              // mirrors allocCounter for external readers (status API)
	allocIDBuf         [32]byte                   // scratch buffer for allocationID string building
	allocDedup         map[string]allocCacheEntry // request_id → cached result (value type, no pointer alloc)
	releaseDedup       map[string]struct{}        // allocation_id → already released

	// Reusable buffers for event-loop (single-threaded, no concurrency concerns).
	drainBuf   []*event               // reusable: drainAll
	allocsBuf  []*event               // reusable: processBatch allocs grouping
	pendingBuf []int                  // reusable: batchAllocate pending indices
	reqsBuf    []*domain.RouteContext // reusable: batchAllocate request batch

	// Cached type assertion for BatchSelector interface.
	batchSelector policy.BatchSelector

	// Cached type assertion for GenerationAware interface.
	generationAware policy.GenerationAware

	// Cached type assertion for DriftAware interface.
	driftTracker policy.DriftAware

	// Cached type assertion for policies that prebuild random-access node IDs.
	cachedIDEnsurer policy.CachedIDEnsurer

	// Circuit breaker manager — nil if CB is disabled. Written only by the event-loop.
	cbManager *circuitbreaker.Manager

	// Global rate limit — 0 means unlimited. Written only by the event-loop.
	globalMaxInflight int64

	// Waiting queue — event-loop only. Requests that cannot be allocated
	// immediately are held here (FIFO) until capacity frees up.
	waitingQueue       []*event
	queueEnabled       bool
	queueMaxSize       int64
	queueTimeout       time.Duration
	queueDepthAtomic   atomic.Int64              // mirrors aggregate normal waiting queue depth for external readers
	queueEnabledAtomic atomic.Bool               // mirrors queueEnabled for external readers
	queueMaxSizeAtomic atomic.Int64              // mirrors queueMaxSize for external readers
	queueTimeoutAtomic atomic.Int64              // mirrors queueTimeout (seconds) for external readers
	defaultQueueCfg    config.WaitingQueueConfig // startup defaults from config.yaml

	// Drain audit state — event-loop only, no concurrency.
	drainCyclesWithoutRelease int
	lastDrainHadCapacityFree  bool // set by handleRelease / handleRemoveSession / handleCleanupGateway; read in auditDrainHealth
	// PD separation state — written only by the event-loop.
	pdPrefillPolicy policy.Policy // nil if PD mode not active
	pdDecodePolicy  policy.Policy // nil if PD mode not active
	pdActiveCount   int64         // PD mode total in-flight count (prefill + decode)
	pdActiveAtomic  atomic.Int64  // mirrors pdActiveCount for external readers (monitoring)

	// PD dedup maps (reset on StartStep, same lifecycle as allocDedup/releaseDedup).
	pdAllocDedup   map[string]pdAllocCacheEntry
	pdReleaseDedup map[string]struct{}

	// PD waiting queue — event-loop only. PD requests that cannot be allocated
	// immediately are held here (FIFO) until capacity frees up.
	// Shares queueEnabled/queueMaxSize/queueTimeout config with the normal waiting queue.
	pdWaitingQueue     []*event
	pdQueueDepthAtomic atomic.Int64 // mirrors aggregate PD waiting queue depth for external readers

	// Pause state — event-loop only. When paused, all allocations (centralized
	// and PD) are rejected with ErrPaused and queued requests are drained.
	paused       bool        // written only by event-loop
	pausedAtomic atomic.Bool // mirrors paused for external readers (status API)

	// Fair scheduling: alternates drain order between normal and PD queues.
	lastDrainedNormal bool

	// currentPolicyConfig is the last accepted StartStep policy config, reused
	// when PD-role instances register after the step already entered SERVING.
	currentPolicyConfig policy.PolicyConfig

	// defaultPDPolicyConfig holds startup defaults for PD policies. Per-step
	// StartStep fields override these values in resolvePDPolicies.
	defaultPDPolicyConfig policy.PolicyConfig
}

type allocationKind uint8

const (
	allocationKindNormal allocationKind = iota
	allocationKindPrefill
	allocationKindDecode
)

type gatewayAllocation struct {
	instanceID    string
	allocationID  string
	resourceGroup string
	kind          allocationKind
}

// ServerOption configures optional Server features.
// Applied during NewServer construction.
type ServerOption func(*Server)

// WithCircuitBreaker enables circuit breaker tracking for backend instances.
// The Manager is accessed exclusively from the event-loop (no mutex needed).
func WithCircuitBreaker(cfg circuitbreaker.Config) ServerOption {
	return func(s *Server) {
		s.cbManager = circuitbreaker.NewManager(cfg)
	}
}

// WithGlobalMaxInflight sets the scheduler-side global cap on total in-flight
// allocations. When the activeCount reaches this limit, new Allocate requests
// are rejected with ErrGlobalRateLimitExceeded. Set to 0 (default) to disable.
func WithGlobalMaxInflight(max int64) ServerOption {
	return func(s *Server) {
		s.globalMaxInflight = max
	}
}

// WithWaitingQueueConfig sets the startup-default waiting queue configuration.
// Per-step overrides via StartStep API take precedence.
func WithWaitingQueueConfig(cfg config.WaitingQueueConfig) ServerOption {
	return func(s *Server) {
		s.defaultQueueCfg = cfg
		// Apply defaults immediately (will be overridden by StartStep if needed).
		s.queueEnabled = cfg.Enabled
		s.queueMaxSize = cfg.MaxSize
		if s.queueMaxSize <= 0 {
			s.queueMaxSize = 100000
		}
		s.queueTimeout = cfg.Timeout.Duration
		if s.queueTimeout <= 0 {
			s.queueTimeout = 300 * time.Second
		}
		// Sync atomic mirrors for external readers.
		s.queueEnabledAtomic.Store(s.queueEnabled)
		s.queueMaxSizeAtomic.Store(s.queueMaxSize)
		s.queueTimeoutAtomic.Store(int64(s.queueTimeout.Seconds()))
	}
}

// WithDefaultPDPolicyConfig sets startup defaults for PD prefill/decode policies.
// Per-step StartStep PolicyConfig values override non-zero/non-empty fields.
func WithDefaultPDPolicyConfig(cfg policy.PolicyConfig) ServerOption {
	return func(s *Server) {
		s.defaultPDPolicyConfig = cfg
	}
}

// preWarmPools fills the event and allocResult pools with pre-allocated objects
// to reduce GC pressure during the initial burst of requests.
var preWarmPoolsOnce sync.Once

func preWarmPools() {
	preWarmPoolsOnce.Do(func() {
		const preWarmSize = 8192

		events := make([]*event, preWarmSize)
		for i := range events {
			events[i] = new(event)
		}
		for _, ev := range events {
			eventPool.Put(ev)
		}

		channels := make([]chan allocResult, preWarmSize)
		for i := range channels {
			channels[i] = make(chan allocResult, 1)
		}
		for _, ch := range channels {
			allocResultPool.Put(ch)
		}

		pdChannels := make([]chan pdAllocResult, preWarmSize/2)
		for i := range pdChannels {
			pdChannels[i] = make(chan pdAllocResult, 1)
		}
		for _, ch := range pdChannels {
			pdAllocResultPool.Put(ch)
		}
	})
}

func NewServer(state *store.NodeStateStore, p policy.Policy, logger *zap.Logger, accessLog *zap.Logger, opts ...ServerOption) *Server {
	// Pre-warm object pools to reduce GC pressure during initial burst.
	preWarmPools()

	s := &Server{
		eventCh:        make(chan *event, defaultEventBufSize),
		stopCh:         make(chan struct{}),
		state:          state,
		policy:         p,
		logger:         logger,
		accessLog:      accessLog,
		stepDoneCh:     make(chan struct{}),
		gatewayAllocs:  make(map[string]map[string]gatewayAllocation),
		allocDedup:     make(map[string]allocCacheEntry, defaultDedupCapacity),
		releaseDedup:   make(map[string]struct{}, defaultDedupCapacity),
		pdAllocDedup:   make(map[string]pdAllocCacheEntry, defaultDedupCapacity),
		pdReleaseDedup: make(map[string]struct{}, defaultDedupCapacity),
		drainBuf:       make([]*event, 0, maxDrainBatch),
		allocsBuf:      make([]*event, 0, maxDrainBatch),
		pendingBuf:     make([]int, 0, maxDrainBatch),
		reqsBuf:        make([]*domain.RouteContext, 0, maxDrainBatch),
		groups:         make(map[string]*groupRuntime),
	}
	if bs, ok := p.(policy.BatchSelector); ok {
		s.batchSelector = bs
	}
	if ga, ok := p.(policy.GenerationAware); ok {
		s.generationAware = ga
	}
	if dt, ok := p.(policy.DriftAware); ok {
		s.driftTracker = dt
	}
	if ce, ok := p.(policy.CachedIDEnsurer); ok {
		s.cachedIDEnsurer = ce
	}
	for _, opt := range opts {
		opt(s)
	}
	// Start in IDLE phase.
	s.stepPhase.Store(int32(domain.StepIdle))
	s.policyName.Store(string(p.Name()))
	s.groups[domain.DefaultResourceGroup] = newGroupRuntime(domain.DefaultResourceGroup, p, s.defaultQueueCfg)
	s.syncDefaultGroupMirrors()
	go s.loop()
	return s
}

func (s *Server) submitEvent(ctx context.Context, ev *event) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		s.submitMu.RLock()
		if s.stopping.Load() {
			s.submitMu.RUnlock()
			return ErrSchedulerStopped
		}
		select {
		case <-s.stopCh:
			s.submitMu.RUnlock()
			return ErrSchedulerStopped
		case <-ctx.Done():
			s.submitMu.RUnlock()
			return ctx.Err()
		case s.eventCh <- ev:
			s.submitMu.RUnlock()
			return nil
		default:
			s.submitMu.RUnlock()
		}

		timer := time.NewTimer(submitRetryDelay)
		select {
		case <-s.stopCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ErrSchedulerStopped
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Server) preparePolicyForNodes(nodes map[string]*domain.NodeState) {
	if s.generationAware != nil {
		s.generationAware.SetGeneration(s.state.Generation())
	}
	if s.cachedIDEnsurer != nil {
		s.cachedIDEnsurer.EnsureCachedIDs(nodes)
	}
}

func (s *Server) loop() {
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case first, ok := <-s.eventCh:
			if !ok {
				return
			}
			batch := s.drainAll(first)
			batchStart := time.Now()
			if s.processBatch(batch) {
				return
			}
			metrics.EventLoopBatchDuration.Observe(float64(time.Since(batchStart).Microseconds()) / 1000.0)
		case <-heartbeat.C:
			s.logHeartbeat()
		}
	}
}

// logHeartbeat emits a periodic status summary to the control-plane logger (router.log).
// This "proof of life" runs every heartbeatInterval and is written synchronously,
// so its absence reliably indicates event-loop deadlock.
func (s *Server) logHeartbeat() {
	depth := len(s.eventCh)
	phase := domain.StepPhase(s.stepPhase.Load())
	stepID := s.stepID.Load()
	active := s.activeCount
	pdActive := s.pdActiveCount
	queueDepth := s.queueDepthAtomic.Load()
	pdQueueDepth := s.pdQueueDepthAtomic.Load()

	metrics.EventChannelDepth.Set(float64(depth))
	metrics.AllocDedupSize.Set(float64(len(s.allocDedup)))
	metrics.ReleaseDedupSize.Set(float64(len(s.releaseDedup)))
	metrics.WaitingQueueDepth.Set(float64(queueDepth))
	metrics.PDWaitingQueueDepth.Set(float64(pdQueueDepth))

	// Update global rate limit headroom gauge.
	if s.globalMaxInflight > 0 {
		metrics.GlobalRateLimitHeadroom.Set(float64(s.globalMaxInflight - s.totalActiveCount()))
	} else {
		metrics.GlobalRateLimitHeadroom.Set(-1)
	}

	// Sync circuit breaker state: handles time-based Open→HalfOpen transitions
	// even when no new requests are flowing.
	if s.cbManager != nil {
		s.cbManager.SyncToNodeState(s.state.GetNodes(), time.Now())
	}

	fields := []zap.Field{
		logger.Event(logger.EventSchedulerAlive),
		zap.String("resource_group", domain.DefaultResourceGroup),
		zap.String("phase", phase.String()),
		zap.Int64("step_id", stepID),
		zap.Int64("active_requests", active),
		zap.Int64("pd_active_requests", pdActive),
		zap.Int64("total_active_requests", s.totalActiveCount()),
		zap.Int("channel_depth", depth),
		zap.Int("instances", len(s.state.GetNodes())),
		zap.Int("alloc_dedup_size", len(s.allocDedup)),
		zap.Int("release_dedup_size", len(s.releaseDedup)),
		zap.Int64("waiting_queue_depth", queueDepth),
		zap.Int64("pd_waiting_queue_depth", pdQueueDepth),
		zap.Int("default_waiting_queue_depth", len(s.waitingQueue)),
		zap.Int("default_pd_waiting_queue_depth", len(s.pdWaitingQueue)),
		zap.Bool("queue_enabled", s.queueEnabled),
	}
	fields = append(fields, s.aggregateTrackingFields()...)
	s.logger.Info("event-loop alive", fields...)
	metrics.EventLoopLastActiveTS.Set(float64(time.Now().Unix()))
}

// drainAll collects the first event plus as many pending events as possible
// from the channel (non-blocking), up to maxDrainBatch.
func (s *Server) drainAll(first *event) []*event {
	s.drainBuf = s.drainBuf[:0]
	s.drainBuf = append(s.drainBuf, first)
	for len(s.drainBuf) < maxDrainBatch {
		select {
		case ev := <-s.eventCh:
			s.drainBuf = append(s.drainBuf, ev)
		default:
			return s.drainBuf
		}
	}
	return s.drainBuf
}

// flushAllocs processes and returns accumulated allocate events.
// Called only from the event-loop.
func (s *Server) flushAllocs() {
	if len(s.allocsBuf) > 0 {
		s.batchAllocate(s.allocsBuf)
		for _, ev := range s.allocsBuf {
			finishRouteEventIfNotQueued(ev)
		}
		s.allocsBuf = s.allocsBuf[:0]
	}
}

// processBatch processes a drained batch while preserving global FIFO ordering.
// Consecutive Allocate events are grouped and handled as a batch; any non-Allocate
// event flushes the pending Allocate group first, ensuring correct sequencing.
// Returns true if evStop was encountered and the loop should exit.
func (s *Server) processBatch(batch []*event) (stopped bool) {
	s.allocsBuf = s.allocsBuf[:0]

	for _, ev := range batch {
		switch ev.typ {
		case evAllocate:
			s.allocsBuf = append(s.allocsBuf, ev)
			continue
		case evRelease:
			s.flushAllocs()
			s.handleRelease(ev)
			domain.ReleaseCostMetrics(ev.costMetrics)
			putEvent(ev)
		case evAllocatePD:
			s.flushAllocs()
			s.handleAllocatePD(ev)
			finishRouteEventIfNotQueued(ev)
		case evReleasePD:
			s.flushAllocs()
			s.handleReleasePD(ev)
			putEvent(ev)
		case evCompensatePD:
			s.flushAllocs()
			s.handleCompensatePD(ev)
			putEvent(ev)
		case evStartStep:
			s.flushAllocs()
			s.handleStartStep(ev)
			putEvent(ev)
		case evEndStep:
			s.flushAllocs()
			s.handleEndStep(ev)
			putEvent(ev)
		case evCleanupGateway:
			s.flushAllocs()
			s.handleCleanupGateway(ev)
			putEvent(ev)
		case evRemoveSession:
			s.flushAllocs()
			s.handleRemoveSession(ev)
			putEvent(ev)
		case evPause:
			s.flushAllocs()
			s.handlePause(ev)
			putEvent(ev)
		case evContinue:
			s.flushAllocs()
			s.handleContinue(ev)
			putEvent(ev)
		case evGetResourceGroupState:
			s.flushAllocs()
			s.handleGetResourceGroupState(ev)
			putEvent(ev)
		case evGetResourceGroupDrainWait:
			s.flushAllocs()
			s.handleGetResourceGroupDrainWait(ev)
			putEvent(ev)
		case evListResourceGroupStates:
			s.flushAllocs()
			s.handleListResourceGroupStates(ev)
			putEvent(ev)
		case evListResourceGroups:
			s.flushAllocs()
			s.handleListResourceGroups(ev)
			putEvent(ev)
		case evRegisterInstances:
			s.flushAllocs()
			s.handleRegisterInstances(ev)
			putEvent(ev)
		case evUnregisterInstances:
			s.flushAllocs()
			s.handleUnregisterInstances(ev)
			putEvent(ev)
		case evSyncInstances:
			s.flushAllocs()
			s.handleSyncInstances(ev)
			putEvent(ev)
		case evStop:
			s.flushAllocs()
			s.stopping.Store(true)
			close(s.stopCh)
			putEvent(ev)
			return true
		}
	}

	s.flushAllocs()

	s.drainWaitingQueuesForGroups()

	// Update event-loop metrics for every batch.
	batchLen := len(batch)
	metrics.EventLoopBatchSize.Observe(float64(batchLen))
	metrics.EventChannelDepth.Set(float64(len(s.eventCh)))
	metrics.EventLoopLastActiveTS.Set(float64(time.Now().Unix()))

	// Log batch metrics when batch_size > 1 for observability.
	if batchLen > 1 {
		s.accessLog.Debug("batch processed",
			logger.Event(logger.EventBatchProcessed),
			zap.Int("batch_size", batchLen),
			zap.Int("channel_depth", len(s.eventCh)))
	}
	return false
}

// handleStartStep, handleEndStep, handleCleanupGateway are in server_step.go.

// Allocate selects the best instance and increments its load counter.
// Returns the instance, an allocation ID (for idempotent Release), and any error.
func (s *Server) Allocate(ctx context.Context, req *domain.RouteContext) (*domain.Instance, string, error) {
	ctx, span := tracing.StartSpan(ctx, "scheduler.allocate")
	defer span.End()

	// Capture TraceID before sending to event-loop, because the event-loop
	// will ReleaseRouteContext(req) after processing — reading req.TraceID
	// after that point is a use-after-release race.
	traceID := req.TraceID
	requestID := req.RequestID
	gatewayAddr := req.GatewayID

	ch := allocResultPool.Get().(chan allocResult)
	ev := getEvent()
	ev.typ = evAllocate
	ev.ctx = ctx
	ev.route = req
	ev.resultCh = ch
	ev.enqueueTime = time.Now()
	if err := s.submitEvent(ctx, ev); err != nil {
		domain.ReleaseRouteContext(req)
		putEvent(ev)
		allocResultPool.Put(ch)
		return nil, "", err
	}

	select {
	case res := <-ch:
		allocResultPool.Put(ch)
		if res.err != nil {
			return nil, "", res.err
		}
		s.accessLog.Debug("allocated",
			zap.String("trace_id", traceID),
			zap.String("instance", res.inst.ID),
			zap.String("allocation_id", res.allocationID))
		return res.inst, res.allocationID, nil
	case <-ctx.Done():
		// Compensate: if the event-loop committed a fresh allocation while we
		// were cancelling, submit a Release to prevent ghost allocation.
		s.handleAbandonedAllocResultAsync(ch, gatewayAddr, requestID, traceID, ctx.Err())
		return nil, "", ctx.Err()
	}
}

// Release decrements the load counter and feeds metrics back to the policy.
// Fire-and-forget: the caller does not need to wait for completion.
// Duplicate releases with the same allocationID are silently skipped.
func (s *Server) Release(ctx context.Context, instanceID string, gatewayAddr string, allocationID string, m *domain.CostMetrics) error {
	ev := getEvent()
	ev.typ = evRelease
	ev.instanceID = instanceID
	ev.gatewayAddr = gatewayAddr
	ev.allocationID = allocationID
	ev.costMetrics = m
	if err := s.submitEvent(ctx, ev); err != nil {
		domain.ReleaseCostMetrics(m)
		putEvent(ev)
		return err
	}
	return nil
}

// AllocatePD selects a prefill + decode instance pair for PD-split inference.
// Returns both instances, their allocation IDs, and any error.
func (s *Server) AllocatePD(ctx context.Context, req *domain.RouteContext) (prefill, decode *domain.Instance, prefillAllocID, decodeAllocID string, err error) {
	ctx, span := tracing.StartSpan(ctx, "scheduler.allocate_pd")
	defer span.End()

	traceID := req.TraceID
	requestID := req.RequestID
	gatewayAddr := req.GatewayID

	ch := pdAllocResultPool.Get().(chan pdAllocResult)
	ev := getEvent()
	ev.typ = evAllocatePD
	ev.ctx = ctx
	ev.route = req
	ev.pdResultCh = ch
	ev.enqueueTime = time.Now()
	if err := s.submitEvent(ctx, ev); err != nil {
		domain.ReleaseRouteContext(req)
		putEvent(ev)
		pdAllocResultPool.Put(ch)
		return nil, nil, "", "", err
	}

	select {
	case res := <-ch:
		pdAllocResultPool.Put(ch)
		if res.err != nil {
			return nil, nil, "", "", res.err
		}
		s.accessLog.Debug("pd allocated",
			zap.String("trace_id", traceID),
			zap.String("prefill", res.prefill.ID),
			zap.String("decode", res.decode.ID),
			zap.String("prefill_alloc_id", res.prefillAllocID),
			zap.String("decode_alloc_id", res.decodeAllocID))
		return res.prefill, res.decode, res.prefillAllocID, res.decodeAllocID, nil
	case <-ctx.Done():
		s.handleAbandonedPDAllocationResultAsync(ch, requestID, gatewayAddr, traceID, ctx.Err())
		return nil, nil, "", "", ctx.Err()
	}
}

func (s *Server) handleAbandonedPDAllocationResultAsync(
	ch chan pdAllocResult,
	requestID, gatewayAddr, traceID string,
	cause error,
) {
	select {
	case res := <-ch:
		pdAllocResultPool.Put(ch)
		s.handleAbandonedPDAllocationResult(res, requestID, gatewayAddr, traceID, cause)
	default:
		go s.waitAbandonedPDAllocationResult(ch, requestID, gatewayAddr, traceID, cause)
	}
}

func (s *Server) waitAbandonedPDAllocationResult(
	ch chan pdAllocResult,
	requestID, gatewayAddr, traceID string,
	cause error,
) {
	timer := time.NewTimer(pdAllocationCompensationWait)
	defer timer.Stop()

	select {
	case res := <-ch:
		pdAllocResultPool.Put(ch)
		s.handleAbandonedPDAllocationResult(res, requestID, gatewayAddr, traceID, cause)
	case <-timer.C:
		s.logger.Warn("pd allocation cancellation compensation timed out",
			zap.String("trace_id", traceID),
			zap.String("request_id", requestID),
			zap.Error(cause),
			zap.Duration("wait", pdAllocationCompensationWait))
	case <-s.stopCh:
	}
}

func (s *Server) handleAbandonedPDAllocationResult(
	res pdAllocResult,
	requestID, gatewayAddr, traceID string,
	cause error,
) {
	if res.err != nil {
		return
	}
	if !res.newAllocation {
		s.accessLog.Debug("pd allocation cancellation observed cached result",
			zap.String("trace_id", traceID),
			zap.String("request_id", requestID),
			zap.String("prefill_alloc_id", res.prefillAllocID),
			zap.String("decode_alloc_id", res.decodeAllocID),
			zap.Error(cause))
		return
	}
	if res.prefill == nil || res.decode == nil {
		s.logger.Warn("pd allocation cancellation result missing instance",
			zap.String("trace_id", traceID),
			zap.String("request_id", requestID),
			zap.Bool("prefill_nil", res.prefill == nil),
			zap.Bool("decode_nil", res.decode == nil),
			zap.Error(cause))
		return
	}

	compensateCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.submitCompensatePD(
		compensateCtx,
		requestID,
		gatewayAddr,
		res.prefill.ID,
		res.decode.ID,
		res.prefillAllocID,
		res.decodeAllocID,
		traceID,
	); err != nil {
		s.logger.Warn("pd allocation cancellation compensation submit failed",
			zap.String("trace_id", traceID),
			zap.String("request_id", requestID),
			zap.String("prefill_alloc_id", res.prefillAllocID),
			zap.String("decode_alloc_id", res.decodeAllocID),
			zap.Error(err))
		return
	}
	s.accessLog.Warn("pd allocation abandoned by caller, compensation submitted",
		zap.String("trace_id", traceID),
		zap.String("request_id", requestID),
		zap.String("prefill", res.prefill.ID),
		zap.String("decode", res.decode.ID),
		zap.String("prefill_alloc_id", res.prefillAllocID),
		zap.String("decode_alloc_id", res.decodeAllocID),
		zap.Error(cause))
}

func (s *Server) submitCompensatePD(
	ctx context.Context,
	requestID, gatewayAddr, prefillID, decodeID, prefillAllocID, decodeAllocID, traceID string,
) error {
	ev := getEvent()
	ev.typ = evCompensatePD
	ev.gatewayAddr = gatewayAddr
	ev.compensateRequestID = requestID
	ev.compensatePrefillID = prefillID
	ev.compensateDecodeID = decodeID
	ev.compensatePrefillAllocID = prefillAllocID
	ev.compensateDecodeAllocID = decodeAllocID
	ev.compensateAllocationTrace = traceID
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return err
	}
	return nil
}

// ---------- Normal Allocate abandonment compensation ----------

// handleAbandonedAllocResultAsync compensates for a normal Allocate whose
// caller timed out after submitting to the event-loop. If the event-loop
// committed a fresh allocation, we submit a Release to free it.
func (s *Server) handleAbandonedAllocResultAsync(
	ch chan allocResult,
	gatewayAddr, requestID, traceID string,
	cause error,
) {
	select {
	case res := <-ch:
		allocResultPool.Put(ch)
		s.compensateAbandonedAlloc(res, gatewayAddr, requestID, traceID, cause)
	default:
		go s.waitAbandonedAllocResult(ch, gatewayAddr, requestID, traceID, cause)
	}
}

func (s *Server) waitAbandonedAllocResult(
	ch chan allocResult,
	gatewayAddr, requestID, traceID string,
	cause error,
) {
	timer := time.NewTimer(pdAllocationCompensationWait)
	defer timer.Stop()

	select {
	case res := <-ch:
		allocResultPool.Put(ch)
		s.compensateAbandonedAlloc(res, gatewayAddr, requestID, traceID, cause)
	case <-timer.C:
		s.logger.Warn("allocation cancellation compensation timed out",
			zap.String("trace_id", traceID),
			zap.String("request_id", requestID),
			zap.Error(cause),
			zap.Duration("wait", pdAllocationCompensationWait))
	case <-s.stopCh:
	}
}

func (s *Server) compensateAbandonedAlloc(
	res allocResult,
	gatewayAddr, requestID, traceID string,
	cause error,
) {
	if res.err != nil || !res.newAllocation {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Release(ctx, res.inst.ID, gatewayAddr, res.allocationID, nil); err != nil {
		s.logger.Warn("allocation cancellation compensation release failed",
			zap.String("trace_id", traceID),
			zap.String("request_id", requestID),
			zap.String("allocation_id", res.allocationID),
			zap.Error(err))
		return
	}
	metrics.AllocCompensationsTotal.Inc()
	s.accessLog.Warn("compensated abandoned allocation",
		zap.String("trace_id", traceID),
		zap.String("request_id", requestID),
		zap.String("instance", res.inst.ID),
		zap.String("allocation_id", res.allocationID),
		zap.Error(cause))
}

// ReleasePD releases a prefill or decode allocation slot.
// Fire-and-forget: the caller does not need to wait for completion.
func (s *Server) ReleasePD(ctx context.Context, instanceID, gatewayAddr, allocationID, role string, durationMs int64, errorCode string) error {
	if instanceID == "" {
		return fmt.Errorf("%w: instance_id is empty", ErrInvalidPDRelease)
	}
	if allocationID == "" {
		return fmt.Errorf("%w: allocation_id is empty", ErrInvalidPDRelease)
	}
	if role != "prefill" && role != "decode" {
		return fmt.Errorf("%w: unknown role %q", ErrInvalidPDRelease, role)
	}

	ev := getEvent()
	ev.typ = evReleasePD
	ev.instanceID = instanceID
	ev.gatewayAddr = gatewayAddr
	ev.allocationID = allocationID
	ev.releaseRole = role
	if durationMs > 0 || errorCode != "" {
		m := domain.AcquireCostMetrics()
		m.DurationMs = durationMs
		m.ErrorCode = errorCode
		ev.costMetrics = m
	}
	if err := s.submitEvent(ctx, ev); err != nil {
		domain.ReleaseCostMetrics(ev.costMetrics)
		putEvent(ev)
		return err
	}
	return nil
}

// StartStep transitions the scheduler from IDLE to SERVING for a new step.
// Resets all node states and policy internal state.
// If policyName is non-empty, the policy is rebuilt for this step; otherwise
// the default policy (from config) is used.
// policyCfg carries optional per-policy parameters (e.g. MaxSessionLoad for session_aware).
func (s *Server) StartStep(ctx context.Context, stepID int64, policyName string, policyCfg policy.PolicyConfig) error {
	return s.StartResourceGroupStep(ctx, domain.DefaultResourceGroup, stepID, policyName, policyCfg)
}

func (s *Server) StartResourceGroupStep(ctx context.Context, resourceGroup string, stepID int64, policyName string, policyCfg policy.PolicyConfig) error {
	_, span := tracing.StartSpan(ctx, "scheduler.start_step")
	defer span.End()

	ch := make(chan stepResult, 1)
	ev := getEvent()
	ev.typ = evStartStep
	ev.resourceGroup = resourceGroup
	ev.stepID = stepID
	ev.policyName = policyName
	ev.policyConfig = policyCfg
	ev.stepResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return err
	}

	select {
	case res := <-ch:
		return res.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EndStep transitions the scheduler from SERVING to DRAINING (or directly to IDLE).
// Returns the number of pending (in-flight) requests.
func (s *Server) EndStep(ctx context.Context, stepID int64) (int64, error) {
	return s.EndResourceGroupStep(ctx, domain.DefaultResourceGroup, stepID)
}

func (s *Server) EndResourceGroupStep(ctx context.Context, resourceGroup string, stepID int64) (int64, error) {
	_, span := tracing.StartSpan(ctx, "scheduler.end_step")
	defer span.End()

	ch := make(chan stepResult, 1)
	ev := getEvent()
	ev.typ = evEndStep
	ev.resourceGroup = resourceGroup
	ev.stepID = stepID
	ev.stepResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return 0, err
	}

	select {
	case res := <-ch:
		return res.pendingRequests, res.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// StepState returns a snapshot of the current step phase, step ID, and pause state.
// Safe to call from any goroutine (uses atomic reads).
func (s *Server) StepState() domain.StepState {
	return domain.StepState{
		ResourceGroup: domain.DefaultResourceGroup,
		Phase:         domain.StepPhase(s.stepPhase.Load()),
		StepID:        s.stepID.Load(),
		Paused:        s.pausedAtomic.Load(),
	}
}

func (s *Server) ResourceGroupStepState(ctx context.Context, resourceGroup string) (domain.StepState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ch := make(chan domain.StepState, 1)
	ev := getEvent()
	ev.typ = evGetResourceGroupState
	ev.resourceGroup = resourceGroup
	ev.stepStateResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return domain.StepState{}, err
	}
	select {
	case state := <-ch:
		return state, nil
	case <-ctx.Done():
		return domain.StepState{}, ctx.Err()
	}
}

func (s *Server) WaitResourceGroupIdle(ctx context.Context, resourceGroup string) (domain.StepState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resourceGroup = domain.NormalizeResourceGroup(resourceGroup)
	for {
		res, err := s.resourceGroupDrainWaitSnapshot(ctx, resourceGroup)
		if err != nil {
			return domain.StepState{}, err
		}
		if res.state.Phase != domain.StepDraining {
			return res.state, nil
		}
		if res.doneCh == nil {
			return res.state, fmt.Errorf("scheduler: cannot wait for resource_group %q drain, done channel is missing", resourceGroup)
		}
		select {
		case <-res.doneCh:
		case <-ctx.Done():
			return res.state, ctx.Err()
		}
	}
}

func (s *Server) resourceGroupDrainWaitSnapshot(ctx context.Context, resourceGroup string) (stepWaitResult, error) {
	ch := make(chan stepWaitResult, 1)
	ev := getEvent()
	ev.typ = evGetResourceGroupDrainWait
	ev.resourceGroup = resourceGroup
	ev.stepWaitResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return stepWaitResult{}, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return stepWaitResult{}, ctx.Err()
	}
}

func (s *Server) ResourceGroups(ctx context.Context) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ch := make(chan []string, 1)
	ev := getEvent()
	ev.typ = evListResourceGroups
	ev.resourceGroupsResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return nil, err
	}
	select {
	case groups := <-ch:
		return groups, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) ResourceGroupStates(ctx context.Context) ([]domain.StepState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ch := make(chan []domain.StepState, 1)
	ev := getEvent()
	ev.typ = evListResourceGroupStates
	ev.resourceGroupStatesResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return nil, err
	}
	select {
	case states := <-ch:
		return states, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// IsPaused returns whether scheduler allocation is currently paused.
// Safe to call from any goroutine (reads atomic mirror).
func (s *Server) IsPaused() bool {
	return s.pausedAtomic.Load()
}

// Pause pauses all allocation (centralized + PD). All queued requests are
// rejected with ErrPaused, and new allocations are rejected until Continue is
// called. Only valid during SERVING phase. Idempotent: calling while already
// paused is a no-op.
func (s *Server) Pause(ctx context.Context) error {
	return s.PauseResourceGroup(ctx, domain.DefaultResourceGroup)
}

func (s *Server) PauseResourceGroup(ctx context.Context, resourceGroup string) error {
	ch := make(chan error, 1)
	ev := getEvent()
	ev.typ = evPause
	ev.resourceGroup = resourceGroup
	ev.pauseResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return err
	}

	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Continue resumes allocation after a Pause call.
// Only valid during SERVING phase. Idempotent: calling while not paused is a no-op.
func (s *Server) Continue(ctx context.Context) error {
	return s.ContinueResourceGroup(ctx, domain.DefaultResourceGroup)
}

func (s *Server) ContinueResourceGroup(ctx context.Context, resourceGroup string) error {
	ch := make(chan error, 1)
	ev := getEvent()
	ev.typ = evContinue
	ev.resourceGroup = resourceGroup
	ev.pauseResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return err
	}

	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetActiveCount returns the aggregate normal in-flight request count.
// Safe to call from any goroutine (reads from an atomic mirror of the
// event-loop's activeCount, updated on every allocation and release).
func (s *Server) GetActiveCount() int64 {
	return s.activeCountAtomic.Load()
}

// GetPDActiveCount returns the current PD in-flight allocation count.
// Safe to call from any goroutine.
func (s *Server) GetPDActiveCount() int64 {
	return s.pdActiveAtomic.Load()
}

// GetTotalActiveCount returns normal + PD in-flight allocation count.
// Safe to call from any goroutine.
func (s *Server) GetTotalActiveCount() int64 {
	return s.activeCountAtomic.Load() + s.pdActiveAtomic.Load()
}

// GetAllocCounter returns the step-scoped allocation counter.
// Safe to call from any goroutine (reads from an atomic mirror of the
// event-loop's allocCounter, updated on every allocation and step start).
func (s *Server) GetAllocCounter() uint64 {
	return s.allocCounterAtomic.Load()
}

// nextAllocationID generates a step-scoped unique allocation ID.
// Called only from the event-loop, so no synchronization needed.
// Uses a scratch buffer to reduce string concatenation allocations.
func (s *Server) nextAllocationID(stepID int64) string {
	s.allocCounter++
	s.allocCounterAtomic.Store(s.allocCounter)
	buf := s.allocIDBuf[:0]
	buf = append(buf, 's')
	buf = strconv.AppendInt(buf, stepID, 10)
	buf = append(buf, '-', 'g')
	buf = strconv.AppendUint(buf, uint64(resourceGroupHash32(s.currentResourceGroup())), 36)
	buf = append(buf, '-', 'a')
	buf = strconv.AppendUint(buf, s.allocCounter, 36)
	return string(buf)
}

func resourceGroupHash32(group string) uint32 {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	const (
		offset32 = uint32(2166136261)
		prime32  = uint32(16777619)
	)
	hash := offset32
	for i := 0; i < len(group); i++ {
		hash ^= uint32(group[i])
		hash *= prime32
	}
	return hash
}

// handleRemoveSession delegates session removal to the policy if it supports it.
// Called only from the event-loop (single-threaded).
func (s *Server) handleRemoveSession(ev *event) {
	g := s.getGroup(ev.resourceGroup)
	if g == nil {
		if ev.sessionDoneCh != nil {
			close(ev.sessionDoneCh)
		}
		return
	}
	s.withGroup(g, func() {
		s.handleRemoveSessionCurrent(ev)
	})
}

func (s *Server) handleRemoveSessionCurrent(ev *event) {
	if sr, ok := s.policy.(policy.SessionRemover); ok {
		sr.RemoveSession(ev.sessionID)
		// Session removal frees session capacity — signal auditDrainHealth.
		s.lastDrainHadCapacityFree = true
		s.logger.Debug("session removed",
			zap.String("session_id", ev.sessionID),
			zap.Int("waiting_queue_depth", len(s.waitingQueue)))
	}
	if ev.sessionDoneCh != nil {
		close(ev.sessionDoneCh)
	}
}

// RemoveSession unbinds a session from its assigned instance via the event-loop.
// This is called when paddlerl sends /api/v2/session_finish.
// Blocks until the event-loop processes the removal or ctx is cancelled.
func (s *Server) RemoveSession(ctx context.Context, sessionID string) {
	s.RemoveSessionForResourceGroup(ctx, domain.DefaultResourceGroup, sessionID)
}

func (s *Server) RemoveSessionForResourceGroup(ctx context.Context, resourceGroup, sessionID string) {
	done := make(chan struct{}, 1)
	ev := getEvent()
	ev.typ = evRemoveSession
	ev.resourceGroup = resourceGroup
	ev.sessionID = sessionID
	ev.sessionDoneCh = done
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Stop gracefully shuts down the event loop.
// It blocks until the loop goroutine exits.
func (s *Server) Stop() {
	if !s.stopping.CompareAndSwap(false, true) {
		<-s.stopCh
		return
	}
	ev := getEvent()
	ev.typ = evStop
	s.submitMu.Lock()
	select {
	case <-s.stopCh:
		s.submitMu.Unlock()
		putEvent(ev)
		return
	case s.eventCh <- ev:
	}
	s.submitMu.Unlock()
	<-s.stopCh
}

// State returns the underlying NodeStateStore (used by the app container and gRPC handler).
func (s *Server) State() *store.NodeStateStore {
	return s.state
}

// PolicyName returns the current scheduling policy name.
// Safe to call from any goroutine; the event-loop mirrors policy swaps into
// an atomic string value during StartStep.
func (s *Server) PolicyName() policy.Name {
	if v := s.policyName.Load(); v != nil {
		return policy.Name(v.(string))
	}
	return ""
}

// CleanupGateway releases all allocations held by the given gateway.
// Called when a gateway expires from the registry (e.g. heartbeat timeout).
// Blocks until cleanup is complete.
func (s *Server) CleanupGateway(ctx context.Context, gatewayAddr string) {
	done := make(chan struct{}, 1)
	ev := getEvent()
	ev.typ = evCleanupGateway
	ev.gatewayAddr = gatewayAddr
	ev.cleanupDoneCh = done
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// logCircuitTransition logs and emits metrics for a circuit breaker state change.
// Called only from the event-loop (single-threaded).
func (s *Server) logCircuitTransition(instanceID, errorCode string, oldState, newState circuitbreaker.State) {
	switch newState {
	case circuitbreaker.Open:
		metrics.CircuitBreakerTrips.WithLabelValues(instanceID, "open").Inc()
		s.logger.Warn("circuit breaker opened",
			logger.Event(logger.EventCircuitOpen),
			zap.String("instance_id", instanceID),
			zap.Stringer("old_state", oldState),
			zap.String("error_code", errorCode))
	case circuitbreaker.HalfOpen:
		metrics.CircuitBreakerTrips.WithLabelValues(instanceID, "half_open").Inc()
		s.logger.Info("circuit breaker half-open",
			logger.Event(logger.EventCircuitHalfOpen),
			zap.String("instance_id", instanceID),
			zap.Stringer("old_state", oldState))
	case circuitbreaker.Closed:
		metrics.CircuitBreakerTrips.WithLabelValues(instanceID, "closed").Inc()
		s.logger.Info("circuit breaker closed",
			logger.Event(logger.EventCircuitClose),
			zap.String("instance_id", instanceID),
			zap.Stringer("old_state", oldState))
	}
}

package gateway

import (
	"context"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/forensics"
	"github.com/yzx/rl-router/pkg/jsonutil"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

// StepChecker provides step-state awareness for the gateway.
// In gateway mode this is the SchedulerClient; in hybrid mode it wraps
// the in-process scheduler.
type StepChecker interface {
	IsServing() bool
}

// ResourceGroupStepChecker provides per-resource-group step state for
// multi-tenant gateways. The bool return indicates whether the state is known
// locally; unknown non-default groups should be checked by the scheduler.
type ResourceGroupStepChecker interface {
	ResourceGroupStepState(resourceGroup string) (domain.StepState, bool)
}

// PauseChecker is an optional interface implemented by step checkers that can
// report scheduler pause state. Most endpoints use it for fast-path rejection;
// chat-completions endpoints skip that fast path so they can return a
// protocol-compatible abort response after reading request metadata.
type PauseChecker interface {
	IsPaused() bool
}

// StepStateUpdater receives step-state push notifications from the scheduler.
type StepStateUpdater interface {
	UpdateStepState(state domain.StepState)
}

// PendingReleaseEnqueuer accepts releases that exhausted retries for later
// delivery via heartbeat piggyback. Implemented by SchedulerClient.
type PendingReleaseEnqueuer interface {
	EnqueuePendingRelease(rel PendingRelease)
}

// Server is the gateway data-plane service.
// It receives HTTP requests, asks the scheduler for an instance, forwards the
// request, and then releases the allocation.
type Server struct {
	allocator              InstanceAllocator
	proxy                  Proxy
	httpClient             *http.Client // used by V2 ACK stream forwarding
	stepChecker            StepChecker
	stepUpdater            StepStateUpdater
	limiter                FlowController // nil = no local rate limiting
	gatewayAddr            string
	logger                 *zap.Logger
	auditLogger            *zap.Logger                // non-sampled audit logger (nil = audit disabled)
	inflight               *forensics.InflightTracker // nil = inflight tracking disabled
	ownsPool               bool                       // true when allocator owns pool objects (LocalAllocator)
	maxRequestBodyBytes    int64                      // 0 means unlimited
	schedulerRPCTimeout    time.Duration
	maxPDRescheduleRetries int
	maxAllocPDRetries      int
	pdAllocRetryBackoff    func() time.Duration

	// pendingReleaseEnqueuer parks failed releases for heartbeat piggyback.
	// nil in hybrid mode (in-process release cannot fail due to network).
	pendingReleaseEnqueuer PendingReleaseEnqueuer

	// PD separation (splitwise) fields.
	splitwiseEnabled bool
	pdAllocator      PDAllocator  // centralized PD allocator
	ownsPDPool       bool         // true when pdAllocator owns RouteContext pool objects
	pdHTTPClient     *http.Client // shared connection pool for PD forward requests

	// routeModeFunc, when non-nil, overrides the default RouteMode dispatch
	// decision (which is derived from splitwiseEnabled). See WithRouteModeFunc.
	routeModeFunc RouteModeFunc
}

const defaultMaxRequestBodyBytes int64 = 64 << 20
const defaultSchedulerRPCTimeout = 3 * time.Second
const defaultSchedulerAllocateTimeout time.Duration = 0
const defaultMaxPDRescheduleRetries = 1000
const defaultMaxAllocPDRetries = 100
const defaultPDBackendResponseHeaderTimeout = 5 * time.Minute

func defaultPDAllocRetryBackoff() time.Duration {
	return time.Duration(1+rand.IntN(10)) * time.Second
}

func (s *Server) nextPDAllocRetryBackoff() time.Duration {
	if s.pdAllocRetryBackoff != nil {
		return s.pdAllocRetryBackoff()
	}
	return defaultPDAllocRetryBackoff()
}

func NewServer(allocator InstanceAllocator, proxy Proxy, logger *zap.Logger, opts ...ServerOption) *Server {
	_, ownsPool := allocator.(PoolResourceOwner)
	s := &Server{
		allocator: allocator,
		proxy:     proxy,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second, // TCP connect timeout
					KeepAlive: 30 * time.Second, // OS-level TCP keepalive probe interval
				}).DialContext,
				MaxIdleConnsPerHost: 100,
				MaxIdleConns:        1000,
				IdleConnTimeout:     90 * time.Second,
			},
			// No global Timeout — each request uses its own context deadline.
		},
		pdHTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConnsPerHost:   20,
				MaxIdleConns:          200,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: defaultPDBackendResponseHeaderTimeout,
			},
		},
		logger:                 logger,
		ownsPool:               ownsPool,
		maxRequestBodyBytes:    defaultMaxRequestBodyBytes,
		schedulerRPCTimeout:    defaultSchedulerRPCTimeout,
		maxPDRescheduleRetries: defaultMaxPDRescheduleRetries,
		maxAllocPDRetries:      defaultMaxAllocPDRetries,
		pdAllocRetryBackoff:    defaultPDAllocRetryBackoff,
	}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

// ServerOption configures optional Server dependencies.
type ServerOption func(*Server)

// WithStepChecker sets the step-state checker for fast-path rejection.
func WithStepChecker(sc StepChecker) ServerOption {
	return func(s *Server) {
		s.stepChecker = sc
	}
}

// WithGatewayAddr sets the gateway address used as identity for allocation tracking.
func WithGatewayAddr(addr string) ServerOption {
	return func(s *Server) {
		s.gatewayAddr = addr
	}
}

// WithStepUpdater sets the step-state updater for push notifications.
func WithStepUpdater(u StepStateUpdater) ServerOption {
	return func(s *Server) {
		s.stepUpdater = u
	}
}

// WithFlowController sets the gateway-local rate limiter / concurrency controller.
// When nil (default), no local rate limiting is applied.
func WithFlowController(fc FlowController) ServerOption {
	return func(s *Server) {
		s.limiter = fc
	}
}

// WithAuditLogger sets the non-sampled audit logger for request lifecycle auditing.
// When nil, audit records are not emitted.
func WithAuditLogger(l *zap.Logger) ServerOption {
	return func(s *Server) {
		s.auditLogger = l
	}
}

// WithInflightTracker sets the inflight request tracker for age-based monitoring.
// When nil, inflight tracking is disabled.
func WithInflightTracker(t *forensics.InflightTracker) ServerOption {
	return func(s *Server) {
		s.inflight = t
	}
}

// WithBackendIdleTimeout overrides the idle connection timeout on the
// Server's HTTP client transport. Should be less than the backend's
// keepalive timeout to avoid fetching stale connections from the pool.
func WithBackendIdleTimeout(d time.Duration) ServerOption {
	return func(s *Server) {
		s.httpClient.Transport.(*http.Transport).IdleConnTimeout = d
	}
}

// WithMaxRequestBodyBytes limits how much request body the gateway buffers for
// replayable forwarding. Set to 0 to disable the limit.
func WithMaxRequestBodyBytes(n int64) ServerOption {
	return func(s *Server) {
		s.maxRequestBodyBytes = n
	}
}

// WithSchedulerRPCTimeout sets the timeout applied to scheduler RPCs initiated
// by the gateway allocators and the scheduler client.
func WithSchedulerRPCTimeout(d time.Duration) ServerOption {
	return func(s *Server) {
		s.schedulerRPCTimeout = d
	}
}

// WithPDBackendResponseHeaderTimeout sets the maximum wait for Prefill/Decode
// response headers in PD forwarding. It does not cap SSE stream duration after
// headers are received. Set d <= 0 to disable the header timeout.
func WithPDBackendResponseHeaderTimeout(d time.Duration) ServerOption {
	return func(s *Server) {
		if tr, ok := s.pdHTTPClient.Transport.(*http.Transport); ok {
			if d < 0 {
				d = 0
			}
			tr.ResponseHeaderTimeout = d
		}
	}
}

// WithPDRetryConfig sets PD reschedule and allocation retry caps.
func WithPDRetryConfig(maxRescheduleRetries, maxAllocRetries int) ServerOption {
	return func(s *Server) {
		s.maxPDRescheduleRetries = maxRescheduleRetries
		s.maxAllocPDRetries = maxAllocRetries
	}
}

// WithSplitwiseConfig configures PD separation (splitwise) mode on the gateway server.
func WithSplitwiseConfig() ServerOption {
	return func(s *Server) {
		s.splitwiseEnabled = true
	}
}

// WithPDAllocator sets the centralized PDAllocator for PD separation mode.
// When set, HandleV2SplitwiseChatCompletion uses this instead of local counterManager.
func WithPDAllocator(alloc PDAllocator) ServerOption {
	return func(s *Server) {
		s.pdAllocator = alloc
		_, s.ownsPDPool = alloc.(PDPoolResourceOwner)
	}
}

// WithPendingReleaseEnqueuer sets the enqueuer for failed releases that should
// be piggybacked on the next heartbeat. Only needed in gateway mode (remote
// allocator); hybrid mode releases in-process and cannot fail due to network.
func WithPendingReleaseEnqueuer(e PendingReleaseEnqueuer) ServerOption {
	return func(s *Server) {
		s.pendingReleaseEnqueuer = e
	}
}

// RouteMode selects the V2 dispatch path for a single request.
type RouteMode int

const (
	// RouteModeNormal sends the request through the unified non-PD lifecycle.
	RouteModeNormal RouteMode = iota
	// RouteModePD sends the request through HandleV2SplitwiseChatCompletion.
	RouteModePD
)

// RouteModeFunc decides per-request whether to dispatch to PD or normal mode.
// Implementations must be safe for concurrent use and must only use metadata
// available before body parsing (headers, host, path, query, or static config).
// The request body has not been consumed when this is invoked, so do not read
// r.Body here. Gateways that need model/extra_body based dispatch should first
// route via an explicit header or deploy separate PD/normal gateway listeners.
type RouteModeFunc func(r *http.Request) RouteMode

// WithRouteModeFunc overrides the default per-request route dispatch decision.
// The default (when this option is not set) follows s.splitwiseEnabled: PD when
// enabled, normal otherwise. Custom dispatchers are useful for multi-tenant
// gateways that route based on headers, host, or URL path.
func WithRouteModeFunc(fn RouteModeFunc) ServerOption {
	return func(s *Server) {
		s.routeModeFunc = fn
	}
}

// routeMode returns the dispatch mode for the given request, consulting the
// custom RouteModeFunc when configured and falling back to splitwiseEnabled.
func (s *Server) routeMode(r *http.Request) RouteMode {
	if s.routeModeFunc != nil {
		return s.routeModeFunc(r)
	}
	if s.splitwiseEnabled {
		return RouteModePD
	}
	return RouteModeNormal
}

func (s *Server) schedulerServing(resourceGroup string) bool {
	if s.stepChecker == nil {
		return true
	}
	resourceGroup = normalizeResourceGroup(resourceGroup)
	if rg, ok := s.stepChecker.(ResourceGroupStepChecker); ok {
		state, known := rg.ResourceGroupStepState(resourceGroup)
		if !known && resourceGroup != domain.DefaultResourceGroup {
			return true
		}
		return state.Phase == domain.StepServing
	}
	if resourceGroup != domain.DefaultResourceGroup {
		return true
	}
	return s.stepChecker.IsServing()
}

func (s *Server) schedulerPaused(resourceGroup string) bool {
	if s.stepChecker == nil {
		return false
	}
	resourceGroup = normalizeResourceGroup(resourceGroup)
	if rg, ok := s.stepChecker.(ResourceGroupStepChecker); ok {
		state, known := rg.ResourceGroupStepState(resourceGroup)
		if !known && resourceGroup != domain.DefaultResourceGroup {
			return false
		}
		return state.Paused
	}
	if resourceGroup != domain.DefaultResourceGroup {
		return false
	}
	pc, ok := s.stepChecker.(PauseChecker)
	return ok && pc.IsPaused()
}

// SplitwiseEnabled returns whether PD separation mode is active.
func (s *Server) SplitwiseEnabled() bool {
	return s.splitwiseEnabled
}

// HandleStepNotify receives step-state push notifications from the scheduler.
// Endpoint: POST /v1/internal/step-state
func (s *Server) HandleStepNotify(w http.ResponseWriter, r *http.Request) {
	if s.stepUpdater == nil {
		http.Error(w, "step updater not configured", http.StatusInternalServerError)
		return
	}

	var state domain.StepState
	if err := jsonutil.NewDecoder(r.Body).Decode(&state); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	s.stepUpdater.UpdateStepState(state)
	w.WriteHeader(http.StatusOK)
}

// HandleChatCompletion handles OpenAI-compatible /v1/chat/completions requests.
func (s *Server) HandleChatCompletion(w http.ResponseWriter, r *http.Request) {
	s.handleInferenceRequest(w, r, "/v1/chat/completions")
}

// HandleCompletion handles OpenAI-compatible /v1/completions requests.
func (s *Server) HandleCompletion(w http.ResponseWriter, r *http.Request) {
	s.handleInferenceRequest(w, r, "/v1/completions")
}

// HandleGenerate handles SGLang-native /generate requests.
func (s *Server) HandleGenerate(w http.ResponseWriter, r *http.Request) {
	s.handleInferenceRequest(w, r, "/generate")
}

// HandleReward routes FastDeploy reward-model requests through the same
// allocate-forward-release lifecycle as non-streaming inference requests.
func (s *Server) HandleReward(w http.ResponseWriter, r *http.Request) {
	s.handleRewardRequest(w, r, "/v1/reward")
}

func (s *Server) handleRewardRequest(w http.ResponseWriter, r *http.Request, backendPath string) {
	pc, r := s.runPreChecks(w, r)
	if !pc.passed {
		return
	}
	if pc.limiterOK {
		defer s.limiter.Release()
	}
	s.handleBufferedForwardInner(w, r, pc.bodyBytes, pc.reqLog, pc.start, bufferedForwardOptions{
		BackendPath: backendPath,
		Mode:        "reward",
	})
}

// handleInferenceRequest is the unified handler for all inference endpoints.
// Reads the request body to detect stream mode, then forks:
//   - stream=true:  transparent SSE proxy via httputil.ReverseProxy + streamTracker
//   - stream=false: buffered forward with usage extraction + backend retry
//
// backendPath is forwarded to the backend as-is (e.g. "/v1/chat/completions",
// "/v1/completions", "/generate"). For streaming, the reverse proxy preserves
// r.URL.Path automatically; for non-streaming, backendPath is used to construct
// the target URL.
func (s *Server) handleInferenceRequest(w http.ResponseWriter, r *http.Request, backendPath string) {
	abortOnPause := backendPath == "/v1/chat/completions"
	pc, r := s.runPreChecksWithOptions(w, r, preCheckOptions{skipPauseCheck: abortOnPause})
	if !pc.passed {
		return
	}
	if pc.limiterOK {
		defer s.limiter.Release()
	}

	bodyBytes := pc.bodyBytes
	reqLog := pc.reqLog
	stream := parseStreamField(bodyBytes)

	if abortOnPause && s.schedulerPaused(pc.resourceGroup) {
		reqLog.Info("chat completion rejected: scheduler paused, returning abort response",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			zap.String("resource_group", pc.resourceGroup))
		s.writeAbortResponse(w, extractModel(bodyBytes), stream, false)
		return
	}

	if stream {
		s.handleStreamChat(w, r, bodyBytes, reqLog, pc.start, abortOnPause)
	} else {
		sessionID := extractSessionID(bodyBytes)
		s.handleNonStreamChatInner(w, r, bodyBytes, reqLog, pc.start, backendPath, sessionID, abortOnPause, "")
	}
}

// handleStreamChat handles streaming chat completion requests.
// Uses the existing httputil.ReverseProxy with FlushInterval:-1, wrapped in
// a streamTracker to record TTFT, detect [DONE], and count chunks/bytes.
func (s *Server) handleStreamChat(
	w http.ResponseWriter,
	r *http.Request,
	bodyBytes []byte,
	reqLog *zap.Logger,
	start time.Time,
	abortOnPause bool,
) {
	lc := s.newRequestLifecycle(r.Context(), r, bodyBytes, start, reqLog, "stream")
	// finalize handles: releaseAllocation + EmitAudit + ReleaseAuditRecord.
	defer lc.finalize()

	replayableBody(r, bodyBytes)

	// Extract optional session_id from request body for session-aware affinity.
	sessionID := extractSessionID(bodyBytes)

	// Allocate.
	routeCtx := s.acquireRouteContext(r, lc.audit.RequestID, sessionID, extractRequestText(bodyBytes), bodyBytes)
	routeCtx.RequestTokenIDs = extractRequestTokenIDs(bodyBytes)
	if err := lc.tryAllocate(routeCtx); err != nil {
		if abortOnPause && isPausedError(err) {
			reqLog.Info("stream: allocation paused, returning abort response",
				logger.Event(logger.EventAllocate),
				logger.Status(logger.StatusFail),
				zap.String("reason", "paused"))
			lc.audit.DisconnectSource = "paused"
			s.writeAbortResponse(w, extractModel(bodyBytes), true, false)
			return
		}
		src := forensics.ClassifyAllocError(err, lc.ctx)
		code, msg, errType := allocErrorResponse(src, err)
		writeErrorJSON(w, code, msg, errType)
		return
	}
	lc.audit.Attempt = 1

	// Inject data_parallel_rank for sglang multi-DP instances.
	if lc.inst != nil {
		if dpRank := parseDPRankFromID(lc.inst.ID); dpRank >= 0 {
			bodyBytes = injectDPRank(bodyBytes, dpRank)
			replayableBody(r, bodyBytes)
		}
	}

	reqLog.Debug("forwarding to backend",
		logger.Event(logger.EventProxyForward),
		zap.String("target", lc.endpoint),
		zap.String("allocation_id", lc.allocationID))

	// Forward with stale-connection retry.
	lc.audit.ForwardedAt = time.Now()
	tracker, proxyStart, proxyErr := s.streamWithStaleRetry(w, r, lc.instanceID, lc.endpoint, reqLog)
	lc.proxyErr = proxyErr
	proxyLatency := time.Since(proxyStart)
	backendStatus := tracker.status

	// Populate audit stream fields.
	lc.audit.RespondedAt = time.Now()
	lc.audit.BackendStatus = backendStatus

	streamResult := tracker.Result(proxyErr)
	lc.audit.StreamResult = streamResult
	lc.populateStreamAudit(&tracker.capture)
	lc.classifyAndRecordDisconnect(proxyErr, tracker.streamDone)

	ttft := tracker.TTFT(proxyStart)
	lc.recordStreamMetrics(ttft, streamResult, backendStatus)

	// Log success/failure.
	if proxyErr != nil {
		reqLog.Error("proxy failed",
			logger.Event(logger.EventProxyComplete),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonProxyError),
			logger.DisconnectSourceField(lc.audit.DisconnectSource),
			zap.String("instance", lc.instanceID),
			zap.Int("backend_status", backendStatus),
			zap.Duration("proxy_latency", proxyLatency),
			zap.Error(proxyErr))
	} else {
		reqLog.Debug("proxy completed",
			logger.Event(logger.EventProxyComplete),
			logger.Status(logger.StatusOK),
			zap.String("instance", lc.instanceID),
			zap.Int("backend_status", backendStatus),
			zap.Duration("proxy_latency", proxyLatency),
			zap.String("stream_result", streamResult),
			zap.Int64("bytes_written", tracker.bytesWritten),
			zap.Int64("chunks_written", tracker.chunksWritten))
	}
	// Release + audit emission happen in deferred lc.finalize().
}

// releaseTarget carries the identifying fields used in retry logging.
// It does NOT carry the actual release payload (CostMetrics / Role / DurationMs
// / ErrorCode); those are captured by the closure passed to retryRelease.
type releaseTarget struct {
	InstanceID   string
	GatewayAddr  string
	AllocationID string
	Role         string // empty for normal-mode releases
}

// retryRelease is the shared release-retry orchestration used by both the
// normal allocator (Release) and the PD allocator (ReleasePD).
//
// Behavior contract (must remain stable for both call sites):
//   - Detach from client context via WithoutCancel: client disconnect must
//     never abandon a release; preserve trace/span values.
//   - 10s overall timeout to bound scheduler-side hangs.
//   - 1 + 3 retries with exponential backoff (100ms → 200ms → 400ms → 800ms).
//   - metrics.ReleaseRetries is incremented exactly once per retry attempt
//     (never on the final-failure log line).
//   - Logging fields: instance, allocation_id, attempt; if target.Role is
//     non-empty (PD path), it is added as "role".
func (s *Server) retryRelease(
	parent context.Context,
	reqLog *zap.Logger,
	target releaseTarget,
	op func(ctx context.Context) error,
) {
	const maxRetries = 3
	backoff := 100 * time.Millisecond

	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()

	for attempt := 0; attempt <= maxRetries; attempt++ {
		err := op(releaseCtx)
		if err == nil {
			if ce := reqLog.Check(zap.DebugLevel, "release delivered"); ce != nil {
				fields := []zap.Field{
					logger.Event(logger.EventRelease),
					logger.Status(logger.StatusOK),
				}
				fields = append(fields, releaseLogFields(target, attempt+1)...)
				ce.Write(fields...)
			}
			return
		}

		if attempt < maxRetries {
			metrics.ReleaseRetries.Inc()
			fields := releaseLogFields(target, attempt+1)
			reqLog.Warn("release failed, retrying",
				append([]zap.Field{
					logger.Event(logger.EventRelease),
					logger.Status(logger.StatusRetry),
					logger.Reason(logger.ReasonReleaseFailed),
					zap.Error(err),
				}, fields...)...,
			)

			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
				backoff *= 2
			case <-releaseCtx.Done():
				timer.Stop()
				reqLog.Error("release retry abandoned: release timeout exceeded",
					append([]zap.Field{
						logger.Event(logger.EventRelease),
						logger.Status(logger.StatusFail),
						logger.Reason(logger.ReasonReleaseFailed),
					}, releaseLogFields(target, attempt+1)...)...,
				)
				s.enqueuePendingRelease(target)
				return
			}
			continue
		}
		reqLog.Error("release failed after all retries",
			append([]zap.Field{
				logger.Event(logger.EventRelease),
				logger.Status(logger.StatusFail),
				logger.Reason(logger.ReasonReleaseFailed),
				zap.Error(err),
			}, releaseLogFields(target, attempt+1)...)...,
		)
		s.enqueuePendingRelease(target)
	}
}

// enqueuePendingRelease parks a failed release for heartbeat piggyback delivery.
func (s *Server) enqueuePendingRelease(target releaseTarget) {
	if s.pendingReleaseEnqueuer == nil {
		return
	}
	s.pendingReleaseEnqueuer.EnqueuePendingRelease(PendingRelease{
		InstanceID:   target.InstanceID,
		GatewayAddr:  target.GatewayAddr,
		AllocationID: target.AllocationID,
		Role:         target.Role,
	})
}

// releaseLogFields builds the structured-log fields shared by normal and PD
// release retry paths. Role is omitted when empty to keep normal-mode logs
// byte-identical to the pre-refactor format.
func releaseLogFields(target releaseTarget, attempt int) []zap.Field {
	fields := []zap.Field{
		zap.String("instance", target.InstanceID),
		zap.String("gateway_addr", target.GatewayAddr),
		zap.String("allocation_id", target.AllocationID),
		zap.Int("attempt", attempt),
	}
	if target.Role != "" {
		fields = append(fields, zap.String("role", target.Role))
	}
	return fields
}

// releaseWithRetry is the normal-mode wrapper around retryRelease.
// Release is idempotent via allocationID, so retries are safe.
func (s *Server) releaseWithRetry(ctx context.Context, reqLog *zap.Logger, instanceID, gatewayAddr, allocationID string, m *domain.CostMetrics) {
	target := releaseTarget{InstanceID: instanceID, GatewayAddr: gatewayAddr, AllocationID: allocationID}
	s.retryRelease(ctx, reqLog, target, func(c context.Context) error {
		return s.allocator.Release(c, instanceID, gatewayAddr, allocationID, m)
	})
}

// requestIDCounter is a process-wide monotonic counter for generating unique request IDs.
// Combined with a random prefix (set at init), it produces globally unique IDs without
// per-call crypto/rand overhead (was 2.1μs/13allocs → ~30ns/1alloc).
var (
	requestIDCounter atomic.Uint64
	requestIDPrefix  string // random hex prefix, set once at init
)

func init() {
	// Generate a random prefix to distinguish different gateway processes.
	requestIDPrefix = strconv.FormatUint(uint64(rand.Uint32()), 36)
}

// statusCapturer is a thin wrapper that records the first WriteHeader status code.
// Used to capture backend response status without breaking http.Flusher (SSE).
type statusCapturer struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sc *statusCapturer) WriteHeader(code int) {
	if !sc.wroteHeader {
		sc.status = code
		sc.wroteHeader = true
	}
	sc.ResponseWriter.WriteHeader(code)
}

func (sc *statusCapturer) Flush() {
	if f, ok := sc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sc *statusCapturer) Unwrap() http.ResponseWriter {
	return sc.ResponseWriter
}

// generateRequestID produces an idempotency key for Allocate.
// Uses X-Trace-ID if present, otherwise generates a fast unique ID using
// an atomic counter with a random process-scoped prefix.
func generateRequestID(r *http.Request) string {
	if traceID := r.Header.Get("X-Trace-ID"); traceID != "" {
		return traceID
	}
	seq := requestIDCounter.Add(1)
	// Format: "{prefix}-{counter}" — unique per process, monotonic within process.
	var buf [32]byte
	b := buf[:0]
	b = append(b, requestIDPrefix...)
	b = append(b, '-')
	b = strconv.AppendUint(b, seq, 36)
	return string(b)
}

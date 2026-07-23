package app

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yzx/rl-router/api/proto/routerpb"
	"github.com/yzx/rl-router/internal/compat"
	"github.com/yzx/rl-router/internal/config"
	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/gateway"
	"github.com/yzx/rl-router/internal/scheduler"
	"github.com/yzx/rl-router/internal/scheduler/circuitbreaker"
	"github.com/yzx/rl-router/internal/scheduler/collector"
	"github.com/yzx/rl-router/internal/scheduler/healthcheck"
	"github.com/yzx/rl-router/internal/scheduler/notifier"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/internal/scheduler/registry"
	"github.com/yzx/rl-router/internal/scheduler/store"
	"github.com/yzx/rl-router/pkg/forensics"
	"github.com/yzx/rl-router/pkg/jsonutil"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
	"github.com/yzx/rl-router/pkg/tracing"
	"github.com/yzx/rl-router/pkg/version"
)

// Option configures the App at construction time.
type Option func(*App)

// WithBundle attaches a logger.Bundle for access-log middleware,
// gRPC interceptor, and runtime log-level control.
func WithBundle(b *logger.Bundle) Option {
	return func(a *App) { a.logBundle = b }
}

// schedulerConn abstracts the lifecycle of a remote scheduler connection.
// gateway.SchedulerClient implements this interface.
type schedulerConn interface {
	Start(ctx context.Context) error
	Stop()
	IsServing() bool
}

type schedulerConnHealth interface {
	IsConnected() bool
}

// App is the top-level application container.
// Depending on Mode it assembles a scheduler, a gateway, or both.
type App struct {
	cfg              *config.Config
	logger           *zap.Logger
	logBundle        *logger.Bundle // optional; enables middleware, interceptor, runtime level control
	scheduler        *scheduler.Server
	gateway          *gateway.Server
	gatewayRegistry  *registry.GatewayRegistry
	schedulerGRPC    *scheduler.GRPCHandler
	schedulerHTTP    *scheduler.HTTPHandler
	stepNotifier     *notifier.StepNotifier
	remoteScheduler  schedulerConn
	metricsCollector *collector.MetricsCollector
	healthChecker    *healthcheck.Checker
	inflightTracker  *forensics.InflightTracker  // nil when gateway is not active
	tracingShutdown  func(context.Context) error // nil when tracing is disabled
}

func New(cfg *config.Config, log *zap.Logger, opts ...Option) (*App, error) {
	app := &App{cfg: cfg, logger: log}
	for _, o := range opts {
		o(app)
	}

	// Init OTel tracing (noop when disabled, ~10ns per span).
	tracingShutdown, err := tracing.Init(context.Background(), tracing.Config{
		Enabled:     cfg.Tracing.Enabled,
		Endpoint:    cfg.Tracing.Endpoint,
		ServiceName: cfg.Tracing.ServiceName,
		SampleRate:  cfg.Tracing.SampleRate,
		Insecure:    cfg.Tracing.Insecure,
	})
	if err != nil {
		return nil, fmt.Errorf("init tracing: %w", err)
	}
	app.tracingShutdown = tracingShutdown

	if err := app.initScheduler(); err != nil {
		return nil, err
	}
	if err := app.initGateway(); err != nil {
		return nil, err
	}

	return app, nil
}

func (a *App) initScheduler() error {
	if a.cfg.Mode != config.ModeScheduler && a.cfg.Mode != config.ModeHybrid {
		return nil
	}

	p, err := policy.Build(policy.Name(a.cfg.Policy), policy.PolicyConfig{
		MaxRequestLoad: a.cfg.MaxRequestLoad,
	})
	if err != nil {
		return fmt.Errorf("build policy: %w", err)
	}
	sm := store.NewNodeStateStore()
	accessLog := a.logger // fallback: use control logger if no bundle
	if a.logBundle != nil {
		accessLog = a.logBundle.SubAccess("scheduler")
	}

	// Build scheduler ServerOptions for optional features.
	var serverOpts []scheduler.ServerOption
	if a.cfg.CircuitBreaker.Enabled {
		cbCfg := circuitbreaker.Config{
			FailThreshold:    a.cfg.CircuitBreaker.FailThreshold,
			SuccessThreshold: a.cfg.CircuitBreaker.SuccessThreshold,
			OpenDuration:     a.cfg.CircuitBreaker.OpenDuration.Duration,
		}
		serverOpts = append(serverOpts, scheduler.WithCircuitBreaker(cbCfg))
		a.logger.Info("circuit breaker enabled",
			zap.Int("fail_threshold", cbCfg.FailThreshold),
			zap.Int("success_threshold", cbCfg.SuccessThreshold),
			zap.Duration("open_duration", cbCfg.OpenDuration))
	}

	if a.cfg.RateLimit.GlobalMaxInflight > 0 {
		serverOpts = append(serverOpts, scheduler.WithGlobalMaxInflight(a.cfg.RateLimit.GlobalMaxInflight))
		a.logger.Info("global rate limit enabled",
			zap.Int64("global_max_inflight", a.cfg.RateLimit.GlobalMaxInflight))
	}

	// Waiting queue: pass startup defaults (per-step overrides via StartStep API).
	serverOpts = append(serverOpts, scheduler.WithWaitingQueueConfig(a.cfg.WaitingQueue))
	serverOpts = append(serverOpts, scheduler.WithDefaultPDPolicyConfig(a.defaultPDPolicyConfig()))
	if a.cfg.WaitingQueue.Enabled {
		a.logger.Info("waiting queue enabled (startup default)",
			zap.Int64("max_size", a.cfg.WaitingQueue.MaxSize),
			zap.Duration("timeout", a.cfg.WaitingQueue.Timeout.Duration))
	}

	a.scheduler = scheduler.NewServer(sm, p, a.logger, accessLog, serverOpts...)

	a.gatewayRegistry = registry.NewGatewayRegistry(a.logger)
	a.gatewayRegistry.SetOnExpired(a.scheduler.CleanupGateway)

	a.stepNotifier = notifier.NewStepNotifier(a.gatewayRegistry, a.logger)
	a.schedulerGRPC = scheduler.NewGRPCHandler(a.scheduler, a.gatewayRegistry, a.logger)
	a.schedulerHTTP = scheduler.NewHTTPHandler(a.scheduler, a.gatewayRegistry, a.stepNotifier, a.logger)

	if err := a.initMetricsCollector(p, sm); err != nil {
		return fmt.Errorf("init metrics collector: %w", err)
	}
	a.initHealthChecker(sm)
	return nil
}

func (a *App) defaultPDPolicyConfig() policy.PolicyConfig {
	cfg := a.cfg.Splitwise
	return policy.PolicyConfig{
		MaxRequestLoad:      a.cfg.MaxRequestLoad,
		CacheBlockSize:      cfg.CacheBlockSize,
		HitRatioWeight:      cfg.HitRatioWeight,
		LoadBalanceWeight:   cfg.LoadBalanceWeight,
		BalanceAbsThreshold: int64(cfg.BalanceAbsThreshold),
		BalanceRelThreshold: cfg.BalanceRelThreshold,
		PDPrefillPolicy:     cfg.PrefillPolicy,
		PDDecodePolicy:      cfg.DecodePolicy,
	}
}

// initHealthChecker sets up the independent /health probe if enabled.
func (a *App) initHealthChecker(sm *store.NodeStateStore) {
	hc := a.cfg.HealthChecker
	if !hc.Enabled {
		return
	}

	httpClient := &http.Client{
		Timeout: hc.Timeout.Duration,
		Transport: &http.Transport{
			MaxIdleConns:        hc.MaxConcurrency * 2,
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	cfg := healthcheck.Config{
		Interval:         hc.Interval.Duration,
		Timeout:          hc.Timeout.Duration,
		FailThreshold:    hc.FailThreshold,
		SuccessThreshold: hc.SuccessThreshold,
		HealthPath:       hc.HealthPath,
		MaxConcurrency:   hc.MaxConcurrency,
	}

	a.healthChecker = healthcheck.New(sm, httpClient, cfg, a.logger)
	a.logger.Info("health checker initialized",
		zap.Duration("interval", cfg.Interval),
		zap.Duration("timeout", cfg.Timeout),
		zap.Int("fail_threshold", cfg.FailThreshold),
		zap.Int("success_threshold", cfg.SuccessThreshold),
		zap.String("health_path", cfg.HealthPath))
}

// initMetricsCollector sets up the backend metrics collector if enabled.
func (a *App) initMetricsCollector(p policy.Policy, sm *store.NodeStateStore) error {
	mc := a.cfg.MetricsCollector
	if !mc.Enabled {
		return nil
	}

	// Build HTTP client with per-host connection pooling.
	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          mc.MaxConcurrency * 2,
			MaxIdleConnsPerHost:   1, // each backend is a different host:port
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: mc.ScrapeTimeout.Duration,
		},
	}

	// Build fetchers for known backend types.
	fetchers := make(map[string]collector.MetricsFetcher)
	for _, backendType := range collector.RegisteredBackendTypes() {
		f, err := collector.BuildFetcher(backendType, httpClient)
		if err != nil {
			return fmt.Errorf("build fetcher for %s: %w", backendType, err)
		}
		fetchers[backendType] = f
	}

	// Default fetcher for instances without BackendType.
	var defaultFetcher collector.MetricsFetcher
	if mc.DefaultBackend != "" {
		var err error
		defaultFetcher, err = collector.BuildFetcher(mc.DefaultBackend, httpClient)
		if err != nil {
			return fmt.Errorf("build default fetcher %s: %w", mc.DefaultBackend, err)
		}
	}

	// Collector options.
	opts := []collector.Option{
		collector.WithMaxConcurrency(mc.MaxConcurrency),
	}

	// Optional MetaUpdater — only if policy supports it.
	if updater, ok := p.(collector.MetaUpdater); ok {
		opts = append(opts, collector.WithUpdater(updater))
		a.logger.Info("metrics collector: policy supports MetaUpdater",
			zap.String("policy", string(p.Name())))
	} else {
		a.logger.Info("metrics collector: health-check only (policy does not implement MetaUpdater)",
			zap.String("policy", string(p.Name())))
	}

	a.metricsCollector = collector.New(
		fetchers, defaultFetcher, sm,
		mc.ScrapeInterval.Duration, mc.ScrapeTimeout.Duration,
		a.logger.Named("collector"),
		opts...,
	)
	a.logger.Info("metrics collector initialized",
		zap.Duration("scrape_interval", mc.ScrapeInterval.Duration),
		zap.Duration("scrape_timeout", mc.ScrapeTimeout.Duration),
		zap.Int("max_concurrency", mc.MaxConcurrency),
	)
	return nil
}

func (a *App) initGateway() error {
	if a.cfg.Mode != config.ModeGateway && a.cfg.Mode != config.ModeHybrid {
		return nil
	}

	allocator, opts, err := a.buildGatewayDeps()
	if err != nil {
		return err
	}

	// Audit logger: non-sampled, writes to access.log (or stderr in dev mode).
	if a.logBundle != nil {
		auditLog := a.logBundle.SubAudit("audit")
		opts = append(opts, gateway.WithAuditLogger(auditLog))
	}

	// Inflight request tracker: periodic sweep → rl_router_inflight_request_age_seconds histogram.
	a.inflightTracker = forensics.NewInflightTracker()
	opts = append(opts, gateway.WithInflightTracker(a.inflightTracker))

	// Backend connection pool idle timeout.
	idleTimeout := a.cfg.BackendIdleTimeout.Duration
	if idleTimeout > 0 {
		opts = append(opts, gateway.WithBackendIdleTimeout(idleTimeout))
	}
	opts = append(opts,
		gateway.WithMaxRequestBodyBytes(a.cfg.MaxRequestBodyBytes),
		gateway.WithSchedulerRPCTimeout(a.cfg.SchedulerRPCTimeout.Duration),
	)
	// PD separation (splitwise) mode.
	if a.cfg.Splitwise.Enabled {
		opts = append(opts, a.buildSplitwiseOption())
		opts = append(opts, gateway.WithPDRetryConfig(
			a.cfg.Splitwise.MaxRescheduleRetries,
			a.cfg.Splitwise.MaxAllocRetries,
		))
		opts = append(opts, gateway.WithPDBackendResponseHeaderTimeout(
			a.cfg.Splitwise.BackendResponseHeaderTimeout.Duration,
		))
		// Wire centralized PDAllocator for global PD load balancing.
		opts = append(opts, a.buildPDAllocatorOption())
	}

	var proxy gateway.Proxy
	if a.cfg.BackendH2C {
		proxy = gateway.NewH2CReverseProxy()
	} else if idleTimeout > 0 {
		proxy = gateway.NewReverseProxyWithIdleTimeout(idleTimeout)
	} else {
		proxy = gateway.NewReverseProxy()
	}
	a.gateway = gateway.NewServer(allocator, proxy, a.logger, opts...)
	return nil
}

func (a *App) buildGatewayDeps() (gateway.InstanceAllocator, []gateway.ServerOption, error) {
	if a.cfg.Mode == config.ModeHybrid {
		return a.buildLocalGatewayDeps()
	}
	return a.buildRemoteGatewayDeps()
}

func (a *App) buildLocalGatewayDeps() (gateway.InstanceAllocator, []gateway.ServerOption, error) {
	allocator := gateway.NewLocalAllocator(a.scheduler.Allocate, a.scheduler.Release)
	opts := []gateway.ServerOption{
		gateway.WithStepChecker(&localStepChecker{scheduler: a.scheduler}),
		gateway.WithGatewayAddr("hybrid"),
	}
	if fc := a.buildFlowController(); fc != nil {
		opts = append(opts, gateway.WithFlowController(fc))
	}
	return allocator, opts, nil
}

func (a *App) buildRemoteGatewayDeps() (gateway.InstanceAllocator, []gateway.ServerOption, error) {
	interval := a.cfg.HeartbeatInterval.Duration
	advertiseAddr := resolveAdvertiseAddr(a.cfg)

	sc, err := newSchedulerClientFn(advertiseAddr, a.cfg.SchedulerAddr, interval, a.logger,
		a.grpcClientDialOpts()...)
	if err != nil {
		return nil, nil, fmt.Errorf("create scheduler client: %w", err)
	}
	a.remoteScheduler = sc
	sc.SetRPCTimeout(a.cfg.SchedulerRPCTimeout.Duration)

	allocator := gateway.NewRemoteAllocator(sc.Conn(), a.logger.Named("allocator"))
	allocator.SetRPCTimeout(a.cfg.SchedulerRPCTimeout.Duration)
	allocator.SetAllocateTimeout(a.cfg.SchedulerAllocateTimeout.Duration)
	opts := []gateway.ServerOption{
		gateway.WithStepChecker(sc),
		gateway.WithStepUpdater(sc),
		gateway.WithGatewayAddr(advertiseAddr),
		gateway.WithPendingReleaseEnqueuer(sc),
	}
	if fc := a.buildFlowController(); fc != nil {
		opts = append(opts, gateway.WithFlowController(fc))
	}
	return allocator, opts, nil
}

// buildFlowController creates a FlowController from config. Returns nil when disabled.
func (a *App) buildFlowController() gateway.FlowController {
	if !a.cfg.RateLimit.Enabled {
		return nil
	}
	switch a.cfg.RateLimit.Mode {
	case "rate":
		a.logger.Info("gateway rate limiter enabled",
			zap.Float64("rate_per_second", a.cfg.RateLimit.RatePerSecond),
			zap.Int("burst", a.cfg.RateLimit.Burst),
			zap.Int("queue_size", a.cfg.RateLimit.QueueSize),
			zap.Duration("queue_timeout", a.cfg.RateLimit.QueueTimeout.Duration))
		return gateway.NewRateLimiter(
			a.cfg.RateLimit.RatePerSecond,
			a.cfg.RateLimit.Burst,
			a.cfg.RateLimit.QueueSize,
			a.cfg.RateLimit.QueueTimeout.Duration,
			a.logger.Named("rate-limiter"),
		)
	default: // "concurrency"
		a.logger.Info("gateway concurrency limiter enabled",
			zap.Int("max_concurrent", a.cfg.RateLimit.MaxConcurrentRequests),
			zap.Int("queue_size", a.cfg.RateLimit.QueueSize),
			zap.Duration("queue_timeout", a.cfg.RateLimit.QueueTimeout.Duration))
		return gateway.NewConcurrencyLimiter(
			a.cfg.RateLimit.MaxConcurrentRequests,
			a.cfg.RateLimit.QueueSize,
			a.cfg.RateLimit.QueueTimeout.Duration,
			a.logger.Named("concurrency-limiter"),
		)
	}
}

// buildSplitwiseOption creates the gateway.ServerOption for PD separation mode.
// This is only called when Splitwise.Enabled is true.
func (a *App) buildSplitwiseOption() gateway.ServerOption {
	cfg := a.cfg.Splitwise

	a.logger.Info("splitwise mode enabled",
		zap.String("prefill_policy", cfg.PrefillPolicy),
		zap.String("decode_policy", cfg.DecodePolicy))

	return gateway.WithSplitwiseConfig()
}

// buildPDAllocatorOption creates the gateway.ServerOption for centralized PD allocation.
// In hybrid mode: LocalPDAllocator calls scheduler.Server.AllocatePD/ReleasePD in-process.
// In gateway mode: RemotePDAllocator calls the scheduler via gRPC.
func (a *App) buildPDAllocatorOption() gateway.ServerOption {
	if a.cfg.Mode == config.ModeHybrid && a.scheduler != nil {
		alloc := gateway.NewLocalPDAllocator(a.scheduler.AllocatePD, a.scheduler.ReleasePD)
		a.logger.Info("PD allocator: local (hybrid mode)")
		return gateway.WithPDAllocator(alloc)
	}
	if a.cfg.Mode == config.ModeGateway && a.remoteScheduler != nil {
		// For gateway mode, we need the gRPC connection from the scheduler client.
		// The remoteScheduler is a SchedulerClient which has a Conn() method.
		type connProvider interface {
			Conn() *grpc.ClientConn
		}
		if cp, ok := a.remoteScheduler.(connProvider); ok {
			alloc := gateway.NewRemotePDAllocator(cp.Conn(), a.logger.Named("pd-allocator"))
			alloc.SetRPCTimeout(a.cfg.SchedulerRPCTimeout.Duration)
			alloc.SetAllocateTimeout(a.cfg.SchedulerAllocateTimeout.Duration)
			a.logger.Info("PD allocator: remote (gateway mode)")
			return gateway.WithPDAllocator(alloc)
		}
		a.logger.Warn("PD allocator: remote scheduler does not provide gRPC connection, PD centralized allocation disabled")
	}
	return func(_ *gateway.Server) {}
}

// grpcClientDialOpts returns gRPC dial options for outbound connections,
// including the client metrics interceptor and logging interceptor when a log bundle is available.
func (a *App) grpcClientDialOpts() []grpc.DialOption {
	var interceptors []grpc.UnaryClientInterceptor
	interceptors = append(interceptors, metrics.UnaryClientMetricsInterceptor())
	if a.logBundle != nil {
		interceptors = append(interceptors, logger.UnaryClientInterceptor(a.logBundle.Sub("grpc-client")))
	}
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(interceptors...),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	}
}

// newSchedulerClientFn is the factory for creating a SchedulerClient.
// Overridden in tests to inject errors.
var newSchedulerClientFn = gateway.NewSchedulerClient

// dialOutboundFn resolves the local outbound IP. Overridden in tests.
var dialOutboundFn = func() (net.Addr, error) {
	conn, err := net.DialTimeout("udp", "8.8.8.8:80", time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr(), nil
}

// resolveAdvertiseAddr returns the externally reachable address for this gateway.
// Priority: AdvertiseAddr > auto-detected outbound IP + listen port > ListenAddr.
func resolveAdvertiseAddr(cfg *config.Config) string {
	if cfg.AdvertiseAddr != "" {
		return cfg.AdvertiseAddr
	}

	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return cfg.ListenAddr
	}

	// If ListenAddr binds to a specific IP (not wildcard), use it directly.
	if host != "" && host != "0.0.0.0" && host != "::" {
		return cfg.ListenAddr
	}

	// Detect outbound IP via dialOutboundFn (no actual traffic is sent).
	addr, err := dialOutboundFn()
	if err != nil {
		return cfg.ListenAddr
	}

	localAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return cfg.ListenAddr
	}
	return net.JoinHostPort(localAddr.IP.String(), port)
}

// localStepChecker wraps the in-process scheduler for step-state checks.
type localStepChecker struct {
	scheduler *scheduler.Server
}

func (c *localStepChecker) IsServing() bool {
	phase := c.scheduler.StepState().Phase
	return phase == domain.StepServing
}

func (c *localStepChecker) IsPaused() bool {
	return c.scheduler.IsPaused()
}

func (c *localStepChecker) ResourceGroupStepState(resourceGroup string) (domain.StepState, bool) {
	state, err := c.scheduler.ResourceGroupStepState(context.Background(), resourceGroup)
	if err != nil {
		return domain.StepState{}, false
	}
	return state, true
}

// Start launches all listeners. Blocks until ctx is cancelled or any server fails.
// All goroutines are tracked via errgroup; when one fails the rest are cancelled
// and Start waits for all of them to finish before returning.
func (a *App) Start(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	defer a.shutdown()

	// Start heartbeat expiry loop for gateway registry.
	// Uses errgroup ctx so it stops when any server fails.
	if a.gatewayRegistry != nil {
		timeout := a.cfg.HeartbeatTimeout.Duration
		a.gatewayRegistry.StartExpireLoop(ctx, timeout)
	}

	// Gateway mode: register with scheduler before accepting traffic.
	if a.remoteScheduler != nil {
		a.logger.Info("registering with scheduler",
			zap.String("scheduler_addr", a.cfg.SchedulerAddr))
		if err := a.remoteScheduler.Start(ctx); err != nil {
			return fmt.Errorf("gateway registration failed: %w", err)
		}
	}

	if a.scheduler != nil && a.cfg.GRPCAddr != "" {
		g.Go(func() error { return a.serveGRPC(ctx) })
	}
	if a.cfg.ListenAddr != "" {
		g.Go(func() error { return a.serveHTTP(ctx) })
	}
	if a.cfg.AdminAddr != "" {
		g.Go(func() error { return a.serveAdmin(ctx) })
	}

	// Start backend metrics collector after servers are up.
	if a.metricsCollector != nil {
		a.metricsCollector.Start()
	}

	// Start independent health checker.
	if a.healthChecker != nil {
		a.healthChecker.Start()
	}

	// Start inflight request age tracker (periodic sweep into histogram).
	if a.inflightTracker != nil {
		a.inflightTracker.Start(5 * time.Second)
	}

	// Block until all goroutines finish — either from ctx cancellation or a fatal error.
	return g.Wait()
}

func (a *App) shutdown() {
	a.logger.Info("app shutdown initiated")

	// Stop collector first — it writes to NodeState and policy, so stop before scheduler.
	if a.metricsCollector != nil {
		a.metricsCollector.Stop()
		a.logger.Info("metrics collector stopped")
	}

	// Stop health checker — it writes NodeState.Healthy, so stop before scheduler.
	if a.healthChecker != nil {
		a.healthChecker.Stop()
		a.logger.Info("health checker stopped")
	}

	// Stop inflight tracker — no more sweeps needed.
	if a.inflightTracker != nil {
		a.inflightTracker.Stop()
		a.logger.Info("inflight tracker stopped")
	}

	if a.remoteScheduler != nil {
		a.remoteScheduler.Stop()
		a.logger.Info("scheduler client stopped")
	}
	if a.scheduler != nil {
		a.scheduler.Stop()
		a.logger.Info("scheduler event-loop stopped")
	}
	if a.stepNotifier != nil {
		a.stepNotifier.Stop()
		a.logger.Info("step notifier stopped")
	}

	// Flush tracing spans last — after all components have stopped producing spans.
	if a.tracingShutdown != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.tracingShutdown(ctx); err != nil {
			a.logger.Warn("tracing shutdown error", zap.Error(err))
		} else {
			a.logger.Info("tracing provider stopped")
		}
	}
}

func (a *App) serveHTTP(ctx context.Context) error {
	mux := http.NewServeMux()
	a.registerRoutes(mux)
	handler := a.wrapMiddleware(mux)
	return a.listenAndServe(ctx, a.cfg.ListenAddr, handler, "HTTP", a.cfg.EnableH2C)
}

// serveAdmin starts a dedicated admin HTTP server for infra/management endpoints.
// Reuses listenAndServe for graceful shutdown and errgroup integration.
func (a *App) serveAdmin(ctx context.Context) error {
	mux := http.NewServeMux()
	a.registerAdminRoutes(mux)
	handler := a.wrapAdminMiddleware(mux)
	return a.listenAndServe(ctx, a.cfg.AdminAddr, handler, "Admin", false)
}

// registerRoutes mounts business HTTP endpoints onto mux.
// When AdminAddr is empty, admin routes are also registered here (backward compatible).
//
// Route groups:
//  1. Gateway  — /v1/chat/completions, /v1/completions, /generate, /v1/reward, /v1/internal/step-state
//  2. Scheduler — /v1/steps/*, /v1/instances
//  3. V2 compat — /api/v2/* backward-compatible routes
//  4. Admin (fallback only when AdminAddr is empty) — /healthz, /readyz, /metrics, /v1/admin/*, /debug/pprof/*
func (a *App) registerRoutes(mux *http.ServeMux) {
	a.registerBusinessRoutes(mux)
	if a.cfg.AdminAddr == "" {
		// Fallback: admin routes share the main port.
		a.registerAdminRoutes(mux)
	}
}

// registerAdminRoutes mounts infra and admin endpoints onto mux.
// These are served on the dedicated admin port when AdminAddr is set,
// or fall back to the main port when AdminAddr is empty.
func (a *App) registerAdminRoutes(mux *http.ServeMux) {
	// Infra routes.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", a.handleReadyz)
	if a.cfg.Metrics.Enabled {
		mux.Handle("/metrics", promhttp.Handler())
	}

	// Admin routes.
	if a.logBundle != nil {
		mux.HandleFunc("GET /v1/admin/log-level", a.handleGetLogLevel)
		mux.HandleFunc("PUT /v1/admin/log-level", a.handlePutLogLevel)
	}
	mux.HandleFunc("GET /version", a.handleVersion)

	// Scheduler admin routes (waiting queue diagnostics, etc.).
	if a.schedulerHTTP != nil {
		a.schedulerHTTP.RegisterAdminRoutes(mux)
	}

	// Pprof routes — explicit registration avoids leaking into DefaultServeMux.
	a.registerPprofRoutes(mux)
}

// registerPprofRoutes registers Go pprof handlers on mux.
func (a *App) registerPprofRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
}

// registerBusinessRoutes mounts gateway data-plane, scheduler control-plane,
// and V2 compat routes onto mux.
func (a *App) registerBusinessRoutes(mux *http.ServeMux) {
	// Gateway data-plane routes.
	if a.gateway != nil {
		mux.HandleFunc("POST /v1/chat/completions", a.gateway.HandleChatCompletion)
		mux.HandleFunc("POST /v1/completions", a.gateway.HandleCompletion)
		mux.HandleFunc("POST /generate", a.gateway.HandleGenerate)
		mux.HandleFunc("POST /v1/reward", a.gateway.HandleReward)
		mux.HandleFunc("POST /v1/internal/step-state", a.gateway.HandleStepNotify)
	}

	// 3. Scheduler control-plane routes.
	if a.schedulerHTTP != nil {
		a.schedulerHTTP.RegisterRoutes(mux)
	}

	// 4. V2 compat routes for rollout-controller callers.
	a.registerV2CompatRoutes(mux)
}

// registerV2CompatRoutes adds backward-compatible /api/v2/* routes.
//
// Three cases by mode:
//   - scheduler/hybrid with gateway: all V2 routes including chat
//   - scheduler-only (no gateway):   all V2 routes except chat
//   - gateway-only (no scheduler):   /health + /api/v2/chat/completions
func (a *App) registerV2CompatRoutes(mux *http.ServeMux) {
	if a.scheduler != nil {
		var chatHandler http.HandlerFunc
		if a.gateway != nil {
			chatHandler = a.gateway.HandleV2ChatCompletion
		}
		v2 := compat.NewV2Adapter(a.scheduler, a.stepNotifier, a.logger)
		v2.RegisterRoutes(mux, chatHandler)
		return
	}
	if a.gateway != nil {
		compat.RegisterV2GatewayRoutes(mux, a.gateway.HandleV2ChatCompletion)
	}
}

// wrapMiddleware wraps the mux with middleware in outside-in order:
//
//	AccessLog (outermost, gateway/hybrid only)
//	  → HTTPMetrics
//	      → Tracing
//	          → Recovery
//	              → mux (innermost)
//
// HTTPMetrics and Tracing are always applied (even without logBundle) for observability.
func (a *App) wrapMiddleware(mux *http.ServeMux) http.Handler {
	var wrapped http.Handler = mux

	if a.logBundle != nil {
		wrapped = logger.RecoveryMiddleware(a.logBundle.Root)(wrapped)
	}

	wrapped = tracing.HTTPMiddleware(wrapped)
	wrapped = metrics.HTTPMetricsMiddleware(wrapped)

	if a.logBundle != nil && a.gateway != nil {
		gwAddr := a.resolveGatewayLabel()
		wrapped = logger.AccessLogMiddleware(
			a.logBundle.SubAccess("access"), gwAddr, a.logBundle.SlowRequest,
		)(wrapped)
	}
	return wrapped
}

// wrapAdminMiddleware wraps the admin mux with a lighter middleware stack:
// Recovery + HTTPMetrics only. No AccessLog (avoids K8s probe log spam)
// and no Tracing (avoids noisy infrastructure spans).
func (a *App) wrapAdminMiddleware(mux *http.ServeMux) http.Handler {
	var wrapped http.Handler = mux

	if a.logBundle != nil {
		wrapped = logger.RecoveryMiddleware(a.logBundle.Root)(wrapped)
	}

	wrapped = metrics.HTTPMetricsMiddleware(wrapped)
	return wrapped
}

// resolveGatewayLabel returns a human-readable label for the gateway in access logs.
func (a *App) resolveGatewayLabel() string {
	return cmp.Or(a.cfg.AdvertiseAddr, "hybrid")
}

type readyzResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (a *App) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := make(map[string]string)
	ready := true

	if a.scheduler != nil {
		states, err := a.scheduler.ResourceGroupStates(r.Context())
		if err != nil {
			checks["step"] = "error: " + err.Error()
			ready = false
		} else {
			serving := 0
			for _, state := range states {
				if state.Phase == domain.StepServing {
					serving++
				}
			}
			checks["step"] = fmt.Sprintf("serving_groups=%d total_groups=%d", serving, len(states))
			if serving == 0 {
				ready = false
			}
		}
	}

	if a.remoteScheduler != nil {
		if health, ok := a.remoteScheduler.(schedulerConnHealth); ok {
			if health.IsConnected() {
				checks["scheduler_conn"] = "connected"
			} else {
				checks["scheduler_conn"] = "not_connected"
				ready = false
			}
		} else if a.remoteScheduler.IsServing() {
			checks["scheduler_conn"] = "serving"
		} else {
			checks["scheduler_conn"] = "not_serving"
			ready = false
		}
	}

	status := "ready"
	code := http.StatusOK
	if !ready {
		status = "not_ready"
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = jsonutil.NewEncoder(w).Encode(readyzResponse{Status: status, Checks: checks})
}

func (a *App) serveGRPC(ctx context.Context) error {
	lis, err := net.Listen("tcp", a.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("grpc listen at %s failed: %w", a.cfg.GRPCAddr, err)
	}

	var grpcOpts []grpc.ServerOption
	{
		// Limit concurrent HTTP/2 streams to prevent gRPC stream explosion
		// when scheduler-side waiting queue holds many requests.
		grpcOpts = append(grpcOpts, grpc.MaxConcurrentStreams(50000))

		// Chain: metrics (outermost) → recovery → logging (innermost).
		// Metrics captures all requests including panics (recorded as Internal).
		// OTel trace propagation is handled via StatsHandler (below).
		interceptors := []grpc.UnaryServerInterceptor{
			metrics.UnaryServerMetricsInterceptor(),
		}
		if a.logBundle != nil {
			interceptors = append(interceptors,
				logger.RecoveryUnaryServerInterceptor(a.logBundle.Root),
				logger.UnaryServerInterceptor(a.logBundle.Sub("grpc")),
			)
		}
		chained := logger.ChainUnaryServer(interceptors...)
		grpcOpts = append(grpcOpts,
			grpc.UnaryInterceptor(chained),
			grpc.StatsHandler(otelgrpc.NewServerHandler()),
		)
	}
	s := grpc.NewServer(grpcOpts...)
	routerpb.RegisterSchedulerServiceServer(s, a.schedulerGRPC)

	go func() {
		<-ctx.Done()
		// GracefulStop waits for in-flight RPCs; use a timer to prevent hanging forever.
		stopped := make(chan struct{})
		go func() {
			s.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(a.cfg.ShutdownGrace.Duration):
			a.logger.Warn("grpc graceful stop timed out, forcing stop",
				zap.String("addr", a.cfg.GRPCAddr))
			s.Stop()
		}
	}()

	a.logger.Info("scheduler gRPC listening", zap.String("addr", a.cfg.GRPCAddr))
	if err := s.Serve(lis); err != nil {
		return fmt.Errorf("grpc server crashed at %s: %w", a.cfg.GRPCAddr, err)
	}
	return nil
}

// listenAndServe starts an HTTP server with unified graceful-shutdown logic.
// When enableH2C is true, the handler is wrapped with h2c.NewHandler for HTTP/2 cleartext support.
func (a *App) listenAndServe(ctx context.Context, addr string, handler http.Handler, name string, enableH2C bool) error {
	if enableH2C {
		handler = h2c.NewHandler(handler, &http2.Server{})
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 100 * time.Second,
		// ReadTimeout / WriteTimeout intentionally unset:
		// SSE streaming proxied responses may last minutes; a global
		// write-timeout would kill them prematurely.
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownGrace.Duration)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			a.logger.Error(name+" graceful shutdown failed, forcing close",
				zap.String("addr", addr), zap.Error(err))
			_ = srv.Close()
		}
	}()

	a.logger.Info(name+" listening", zap.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("%s crashed at %s: %w", name, addr, err)
	}
	return nil
}

// logLevelRequest is the JSON payload for PUT /v1/admin/log-level.
type logLevelRequest struct {
	Level       string `json:"level"`
	AccessLevel string `json:"access_level"`
	Module      string `json:"module"`
}

// logLevelResponse is the JSON response for GET /v1/admin/log-level.
type logLevelResponse struct {
	Level       string            `json:"level"`
	AccessLevel string            `json:"access_level"`
	Modules     map[string]string `json:"modules,omitempty"`
}

// handleGetLogLevel returns current log levels.
func (a *App) handleGetLogLevel(w http.ResponseWriter, _ *http.Request) {
	resp := logLevelResponse{
		Level:       a.logBundle.Level.Level().String(),
		AccessLevel: a.logBundle.AccessLevel.Level().String(),
		Modules:     a.logBundle.Modules(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = jsonutil.NewEncoder(w).Encode(resp)
}

// handlePutLogLevel adjusts log level (global or per-module).
func (a *App) handlePutLogLevel(w http.ResponseWriter, r *http.Request) {
	var req logLevelRequest
	if err := jsonutil.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Module != "" {
		// Per-module level adjustment.
		var lvl zapcore.Level
		if err := lvl.UnmarshalText([]byte(req.Level)); err != nil {
			http.Error(w, fmt.Sprintf("invalid level %q", req.Level), http.StatusBadRequest)
			return
		}
		if !a.logBundle.SetModuleLevel(req.Module, lvl) {
			http.Error(w, fmt.Sprintf("unknown module %q", req.Module), http.StatusNotFound)
			return
		}
		a.logger.Info("module log level changed",
			zap.String("module", req.Module), zap.String("level", lvl.String()))
	} else {
		// Global level adjustment.
		if req.Level != "" {
			var lvl zapcore.Level
			if err := lvl.UnmarshalText([]byte(req.Level)); err != nil {
				http.Error(w, fmt.Sprintf("invalid level %q", req.Level), http.StatusBadRequest)
				return
			}
			a.logBundle.Level.SetLevel(lvl)
			a.logger.Info("control log level changed", zap.String("level", lvl.String()))
		}
		if req.AccessLevel != "" {
			var lvl zapcore.Level
			if err := lvl.UnmarshalText([]byte(req.AccessLevel)); err != nil {
				http.Error(w, fmt.Sprintf("invalid access_level %q", req.AccessLevel), http.StatusBadRequest)
				return
			}
			a.logBundle.AccessLevel.SetLevel(lvl)
			a.logger.Info("access log level changed", zap.String("level", lvl.String()))
		}
	}
	w.WriteHeader(http.StatusOK)
}

// handleVersion returns build information.
func (a *App) handleVersion(w http.ResponseWriter, _ *http.Request) {
	buildInfo := version.Get()
	w.Header().Set("Content-Type", "application/json")
	_ = jsonutil.NewEncoder(w).Encode(buildInfo)
}

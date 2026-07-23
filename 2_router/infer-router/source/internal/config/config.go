package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yzx/rl-router/internal/scheduler/policy"
)

// Mode defines how the router process runs.
type Mode string

const (
	ModeGateway   Mode = "gateway"
	ModeScheduler Mode = "scheduler"
	ModeHybrid    Mode = "hybrid"
)

// Config is the top-level configuration for the router process.
type Config struct {
	Mode          Mode          `json:"mode" yaml:"mode"`
	ListenAddr    string        `json:"listen_addr" yaml:"listen_addr"`
	AdminAddr     string        `json:"admin_addr" yaml:"admin_addr"` // admin HTTP listen address (metrics, healthz, pprof); empty = share main port
	GRPCAddr      string        `json:"grpc_addr" yaml:"grpc_addr"`
	SchedulerAddr string        `json:"scheduler_addr" yaml:"scheduler_addr"` // used by gateway mode
	Policy        string        `json:"policy" yaml:"policy"`
	Log           LogConfig     `json:"log" yaml:"log"`
	Metrics       MetricsConfig `json:"metrics" yaml:"metrics"`

	// Per-instance active-request / load-score cap for the default policy.
	// Overridable per-step via the StartStep API. 0 = policy default (128 for rr/min_load/min_request, 100 for cache_aware).
	MaxRequestLoad int64 `json:"max_request_load" yaml:"max_request_load"`

	// Gateway registration settings.
	AdvertiseAddr     string   `json:"advertise_addr" yaml:"advertise_addr"` // externally reachable address, also used as gateway identity
	HeartbeatInterval Duration `json:"heartbeat_interval" yaml:"heartbeat_interval"`

	// Scheduler heartbeat timeout.
	HeartbeatTimeout Duration `json:"heartbeat_timeout" yaml:"heartbeat_timeout"`

	// Graceful shutdown deadline.
	ShutdownGrace Duration `json:"shutdown_grace" yaml:"shutdown_grace"`

	// Backend HTTP connection pool idle timeout.
	// Should be less than the backend's keepalive timeout to avoid stale connections.
	// Default: 3s (suitable for uvicorn default keepalive=5s).
	BackendIdleTimeout Duration `json:"backend_idle_timeout" yaml:"backend_idle_timeout"`

	// Gateway request body buffering cap. The gateway reads request bodies once
	// so it can replay them to selected backends; this bound protects memory.
	// 0 disables the limit.
	MaxRequestBodyBytes int64 `json:"max_request_body_bytes" yaml:"max_request_body_bytes"`

	// Timeout for short gateway-to-scheduler unary RPCs (Release/Register/Heartbeat).
	// 0 disables the per-RPC timeout and relies on caller contexts only.
	SchedulerRPCTimeout Duration `json:"scheduler_rpc_timeout" yaml:"scheduler_rpc_timeout"`

	// Timeout for gateway-to-scheduler Allocate/AllocatePD RPCs. 0 disables the
	// per-RPC timeout and lets scheduler-side waiting queues use the caller context.
	SchedulerAllocateTimeout Duration `json:"scheduler_allocate_timeout" yaml:"scheduler_allocate_timeout"`

	// H2C (HTTP/2 cleartext) settings.
	EnableH2C  bool `json:"enable_h2c" yaml:"enable_h2c"`   // serve frontend via h2c
	BackendH2C bool `json:"backend_h2c" yaml:"backend_h2c"` // connect to backends via h2c

	// Backend metrics collector.
	MetricsCollector MetricsCollectorConfig `json:"metrics_collector" yaml:"metrics_collector"`

	// Independent health checker (probes /health endpoint).
	HealthChecker HealthCheckerConfig `json:"health_checker" yaml:"health_checker"`

	// Per-request circuit breaker.
	CircuitBreaker CircuitBreakerConfig `json:"circuit_breaker" yaml:"circuit_breaker"`

	// Rate limiting / concurrency control.
	RateLimit RateLimitConfig `json:"rate_limit" yaml:"rate_limit"`

	// Distributed tracing.
	Tracing TracingConfig `json:"tracing" yaml:"tracing"`

	// Scheduler-side waiting queue for RL training scenarios.
	// When enabled, requests that cannot be allocated immediately are queued
	// instead of being rejected. Per-step overrides via StartStep API.
	WaitingQueue WaitingQueueConfig `json:"waiting_queue" yaml:"waiting_queue"`
	// PD separation (splitwise) scheduling.
	Splitwise SplitwiseConfig `json:"splitwise" yaml:"splitwise"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level       string   `json:"level" yaml:"level"`
	AccessLevel string   `json:"access_level" yaml:"access_level"` // request-path log level (default: same as Level)
	Format      string   `json:"format" yaml:"format"`             // "json" or "console"
	SlowRequest Duration `json:"slow_request" yaml:"slow_request"` // slow request warning threshold (default: 30s)

	// File output — empty LogDir means stderr-only (dev mode).
	LogDir     string `json:"log_dir" yaml:"log_dir"`           // directory for log files
	MaxSizeMB  int    `json:"max_size_mb" yaml:"max_size_mb"`   // max size per log file before rotation in MB (default 500)
	MaxBackups int    `json:"max_backups" yaml:"max_backups"`   // max rotated files to keep (default 5)
	MaxAgeDays int    `json:"max_age_days" yaml:"max_age_days"` // max age of rotated files in days (default 30)
	Compress   bool   `json:"compress" yaml:"compress"`         // gzip compress rotated files (default true)

	// Access log sampling — controls how many log lines per second are emitted.
	// Set SampleInitial=0 and SampleThereafter=0 to disable sampling entirely.
	SampleInitial    int `json:"sample_initial" yaml:"sample_initial"`       // first N logs per second emitted (default 100)
	SampleThereafter int `json:"sample_thereafter" yaml:"sample_thereafter"` // then 1-in-N (default 1000; 0=drop all after initial)

	// Independent audit log rotation (request_audit.log).
	Audit AuditLogConfig `json:"audit" yaml:"audit"`
}

// AuditLogConfig configures the independent request audit log file.
// Audit logs have longer retention (90 days) and larger file sizes (1000MB)
// than access logs — forensic evidence must survive longer.
type AuditLogConfig struct {
	MaxSizeMB  int  `json:"max_size_mb" yaml:"max_size_mb"`   // default: 1000
	MaxBackups int  `json:"max_backups" yaml:"max_backups"`   // default: 10
	MaxAgeDays int  `json:"max_age_days" yaml:"max_age_days"` // default: 90
	Compress   bool `json:"compress" yaml:"compress"`         // default: true
}

// MetricsConfig holds observability settings.
type MetricsConfig struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// MetricsCollectorConfig configures the backend metrics collector.
// Health checking is handled by HealthCheckerConfig (separate component).
type MetricsCollectorConfig struct {
	Enabled        bool     `json:"enabled" yaml:"enabled"`                 // default: false
	DefaultBackend string   `json:"default_backend" yaml:"default_backend"` // "fastdeploy"
	ScrapeInterval Duration `json:"scrape_interval" yaml:"scrape_interval"` // default: 2s
	ScrapeTimeout  Duration `json:"scrape_timeout" yaml:"scrape_timeout"`   // default: 1s
	MaxConcurrency int      `json:"max_concurrency" yaml:"max_concurrency"` // default: 128
}

// HealthCheckerConfig configures the independent /health probe.
// Writes NodeState.Healthy; fully decoupled from MetricsCollector.
type HealthCheckerConfig struct {
	Enabled          bool     `json:"enabled" yaml:"enabled"`                     // default: false
	HealthPath       string   `json:"health_path" yaml:"health_path"`             // default: "/health"
	Interval         Duration `json:"interval" yaml:"interval"`                   // default: 3s
	Timeout          Duration `json:"timeout" yaml:"timeout"`                     // default: 2s
	FailThreshold    int      `json:"fail_threshold" yaml:"fail_threshold"`       // default: 3
	SuccessThreshold int      `json:"success_threshold" yaml:"success_threshold"` // default: 2
	MaxConcurrency   int      `json:"max_concurrency" yaml:"max_concurrency"`     // default: 128
}

// CircuitBreakerConfig configures per-request circuit breaking.
// Writes NodeState.CircuitOpen from the scheduler event-loop.
type CircuitBreakerConfig struct {
	Enabled          bool     `json:"enabled" yaml:"enabled"`                     // default: false
	FailThreshold    int      `json:"fail_threshold" yaml:"fail_threshold"`       // default: 3
	SuccessThreshold int      `json:"success_threshold" yaml:"success_threshold"` // default: 2
	OpenDuration     Duration `json:"open_duration" yaml:"open_duration"`         // default: 30s
}

// RateLimitConfig configures request rate limiting and concurrency control.
//
// Two independent layers:
//   - Scheduler-side: GlobalMaxInflight caps total in-flight allocations across
//     all gateways (enforced in the event-loop, zero lock overhead).
//   - Gateway-side: local FlowController (concurrency semaphore or token bucket)
//     for fast rejection before RPC to the scheduler.
type RateLimitConfig struct {
	// --- Scheduler-side global limit (scheduler/hybrid mode only) ---
	GlobalMaxInflight int64 `json:"global_max_inflight" yaml:"global_max_inflight"` // 0 = unlimited (default)

	// --- Gateway-side local limit (gateway/hybrid mode only) ---
	Enabled bool   `json:"enabled" yaml:"enabled"` // default: false
	Mode    string `json:"mode" yaml:"mode"`       // "concurrency" (default) or "rate"

	// Concurrency mode: limits simultaneous in-flight requests (semaphore).
	MaxConcurrentRequests int `json:"max_concurrent_requests" yaml:"max_concurrent_requests"` // default: 1000

	// Rate mode: limits requests per second via token bucket.
	RatePerSecond float64 `json:"rate_per_second" yaml:"rate_per_second"` // tokens/sec; required when mode="rate"
	Burst         int     `json:"burst" yaml:"burst"`                     // max burst; default: max_concurrent_requests

	// Queue parameters (shared by both modes).
	QueueSize    int      `json:"queue_size" yaml:"queue_size"`       // max waiters; 0 = no queue (default: 500)
	QueueTimeout Duration `json:"queue_timeout" yaml:"queue_timeout"` // default: 10s
}

// TracingConfig configures OpenTelemetry distributed tracing.
// When Enabled is false, a noop TracerProvider is used (zero overhead).
type TracingConfig struct {
	Enabled     bool    `json:"enabled" yaml:"enabled"`           // default: false
	Endpoint    string  `json:"endpoint" yaml:"endpoint"`         // OTLP gRPC endpoint (e.g. "localhost:4317")
	ServiceName string  `json:"service_name" yaml:"service_name"` // default: "rl-router"
	SampleRate  float64 `json:"sample_rate" yaml:"sample_rate"`   // default: 1.0 (100%)
	Insecure    bool    `json:"insecure" yaml:"insecure"`         // default: true (no TLS)
}

// WaitingQueueConfig configures the scheduler-side waiting queue.
// In RL training scenarios, all requests must complete — rejecting on overload
// would abort the training step. When enabled, requests that cannot be allocated
// immediately are queued FIFO and drained as capacity frees up.
//
// These values serve as defaults; each StartStep call can override them per-step.
type WaitingQueueConfig struct {
	Enabled bool     `json:"enabled" yaml:"enabled"`   // default: true
	MaxSize int64    `json:"max_size" yaml:"max_size"` // max queued requests; 0 = 100000
	Timeout Duration `json:"timeout" yaml:"timeout"`   // per-request queue timeout; 0 = 600s
}

// SplitwiseConfig configures PD separation (prefill/decode disaggregation).
type SplitwiseConfig struct {
	Enabled                      bool     `json:"enabled" yaml:"enabled"`                                                 // default: false
	PrefillPolicy                string   `json:"prefill_policy" yaml:"prefill_policy"`                                   // default: "pd_cache_aware"
	DecodePolicy                 string   `json:"decode_policy" yaml:"decode_policy"`                                     // default: "request_num"
	MaxRescheduleRetries         int      `json:"max_reschedule_retries" yaml:"max_reschedule_retries"`                   // default: 1000
	MaxAllocRetries              int      `json:"max_alloc_retries" yaml:"max_alloc_retries"`                             // default: 100
	BackendResponseHeaderTimeout Duration `json:"backend_response_header_timeout" yaml:"backend_response_header_timeout"` // default: 5m; 0 disables
	CacheBlockSize               int      `json:"cache_block_size" yaml:"cache_block_size"`                               // default: 64
	HitRatioWeight               float64  `json:"hit_ratio_weight" yaml:"hit_ratio_weight"`                               // default: 1.0
	LoadBalanceWeight            float64  `json:"load_balance_weight" yaml:"load_balance_weight"`                         // default: 0.5
	BalanceAbsThreshold          float64  `json:"balance_abs_threshold" yaml:"balance_abs_threshold"`                     // default: 30.0
	BalanceRelThreshold          float64  `json:"balance_rel_threshold" yaml:"balance_rel_threshold"`                     // default: 0.01
}

// Defaults returns a Config populated with sensible default values.
func Defaults() *Config {
	return &Config{
		Mode:                     ModeHybrid,
		ListenAddr:               ":8080",
		AdminAddr:                ":8081",
		GRPCAddr:                 ":9090",
		Policy:                   "min_load",
		HeartbeatInterval:        Dur(5 * time.Second),
		HeartbeatTimeout:         Dur(15 * time.Second),
		ShutdownGrace:            Dur(15 * time.Second),
		BackendIdleTimeout:       Dur(3 * time.Second),
		MaxRequestBodyBytes:      64 << 20,
		SchedulerRPCTimeout:      Dur(3 * time.Second),
		SchedulerAllocateTimeout: Dur(0),
		Log: LogConfig{
			Level:    "info",
			Format:   "console",
			Compress: true,
		},
		Metrics: MetricsConfig{
			Enabled: true,
		},
		MetricsCollector: MetricsCollectorConfig{
			Enabled:        false,
			DefaultBackend: "fastdeploy",
			ScrapeInterval: Dur(2 * time.Second),
			ScrapeTimeout:  Dur(1 * time.Second),
			MaxConcurrency: 128,
		},
		HealthChecker: HealthCheckerConfig{
			Enabled:          false,
			HealthPath:       "/health",
			Interval:         Dur(3 * time.Second),
			Timeout:          Dur(2 * time.Second),
			FailThreshold:    3,
			SuccessThreshold: 2,
			MaxConcurrency:   128,
		},
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:          false,
			FailThreshold:    3,
			SuccessThreshold: 2,
			OpenDuration:     Dur(30 * time.Second),
		},
		RateLimit: RateLimitConfig{
			GlobalMaxInflight:     0,
			Enabled:               false,
			Mode:                  "concurrency",
			MaxConcurrentRequests: 1000,
			QueueSize:             500,
			QueueTimeout:          Dur(10 * time.Second),
		},
		Tracing: TracingConfig{
			Enabled:     false,
			ServiceName: "rl-router",
			SampleRate:  1.0,
			Insecure:    true,
		},
		WaitingQueue: WaitingQueueConfig{
			Enabled: true,
			MaxSize: 100000,
			Timeout: Dur(600 * time.Second),
		},
		Splitwise: SplitwiseConfig{
			Enabled:                      false,
			PrefillPolicy:                "pd_cache_aware",
			DecodePolicy:                 "request_num",
			MaxRescheduleRetries:         1000,
			MaxAllocRetries:              100,
			BackendResponseHeaderTimeout: Dur(5 * time.Minute),
			CacheBlockSize:               64,
			HitRatioWeight:               1.0,
			LoadBalanceWeight:            0.5,
			BalanceAbsThreshold:          30.0,
			BalanceRelThreshold:          0.01,
		},
	}
}

// LoadFromFile reads a YAML configuration file and unmarshals it into c.
// Fields not present in the file retain their previous values.
func (c *Config) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("parse config file: %w", err)
	}
	return nil
}

// Validate checks the configuration for logical errors.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeGateway, ModeScheduler, ModeHybrid:
	default:
		return fmt.Errorf("invalid mode %q: must be gateway, scheduler, or hybrid", c.Mode)
	}

	if c.Mode == ModeGateway && c.SchedulerAddr == "" {
		return fmt.Errorf("scheduler_addr is required in gateway mode")
	}

	if c.AdminAddr != "" && c.AdminAddr == c.ListenAddr {
		return fmt.Errorf("admin_addr (%s) must differ from listen_addr (%s) when set",
			c.AdminAddr, c.ListenAddr)
	}

	if c.HeartbeatInterval.Duration <= 0 {
		return fmt.Errorf("heartbeat_interval must be > 0")
	}
	if c.HeartbeatTimeout.Duration <= 0 {
		return fmt.Errorf("heartbeat_timeout must be > 0")
	}
	if c.HeartbeatTimeout.Duration <= c.HeartbeatInterval.Duration {
		return fmt.Errorf("heartbeat_timeout (%s) must be > heartbeat_interval (%s)",
			c.HeartbeatTimeout.Duration, c.HeartbeatInterval.Duration)
	}
	if c.ShutdownGrace.Duration <= 0 {
		return fmt.Errorf("shutdown_grace must be > 0")
	}
	if c.BackendIdleTimeout.Duration < 0 {
		return fmt.Errorf("backend_idle_timeout must be >= 0")
	}
	if c.MaxRequestBodyBytes < 0 {
		return fmt.Errorf("max_request_body_bytes must be >= 0")
	}
	if c.SchedulerRPCTimeout.Duration < 0 {
		return fmt.Errorf("scheduler_rpc_timeout must be >= 0")
	}
	if c.SchedulerAllocateTimeout.Duration < 0 {
		return fmt.Errorf("scheduler_allocate_timeout must be >= 0")
	}

	if err := c.MetricsCollector.validate(c.Mode); err != nil {
		return fmt.Errorf("metrics_collector: %w", err)
	}
	if err := c.HealthChecker.validate(c.Mode); err != nil {
		return fmt.Errorf("health_checker: %w", err)
	}
	if err := c.CircuitBreaker.validate(c.Mode); err != nil {
		return fmt.Errorf("circuit_breaker: %w", err)
	}
	if err := c.RateLimit.validate(c.Mode); err != nil {
		return fmt.Errorf("rate_limit: %w", err)
	}
	if err := c.Tracing.validate(); err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	if err := c.WaitingQueue.validate(c.Mode); err != nil {
		return fmt.Errorf("waiting_queue: %w", err)
	}
	if err := c.Splitwise.validate(); err != nil {
		return fmt.Errorf("splitwise: %w", err)
	}

	return nil
}

// validate checks MetricsCollectorConfig for logical errors.
func (mc *MetricsCollectorConfig) validate(mode Mode) error {
	if !mc.Enabled {
		return nil
	}

	if mode == ModeGateway {
		return fmt.Errorf("metrics collector is not supported in gateway mode")
	}

	if mc.ScrapeInterval.Duration <= 0 {
		return fmt.Errorf("scrape_interval must be > 0")
	}
	if mc.ScrapeTimeout.Duration <= 0 {
		return fmt.Errorf("scrape_timeout must be > 0")
	}
	if mc.ScrapeTimeout.Duration >= mc.ScrapeInterval.Duration {
		return fmt.Errorf("scrape_timeout (%s) should be < scrape_interval (%s)",
			mc.ScrapeTimeout.Duration, mc.ScrapeInterval.Duration)
	}
	if mc.MaxConcurrency < 1 || mc.MaxConcurrency > 512 {
		return fmt.Errorf("max_concurrency must be in [1, 512], got %d", mc.MaxConcurrency)
	}

	return nil
}

// validate checks HealthCheckerConfig for logical errors.
func (hc *HealthCheckerConfig) validate(mode Mode) error {
	if !hc.Enabled {
		return nil
	}
	if mode == ModeGateway {
		return fmt.Errorf("health checker is not supported in gateway mode")
	}
	if hc.Interval.Duration <= 0 {
		return fmt.Errorf("interval must be > 0")
	}
	if hc.Timeout.Duration <= 0 {
		return fmt.Errorf("timeout must be > 0")
	}
	if hc.Timeout.Duration >= hc.Interval.Duration {
		return fmt.Errorf("timeout (%s) should be < interval (%s)",
			hc.Timeout.Duration, hc.Interval.Duration)
	}
	if hc.FailThreshold < 1 {
		return fmt.Errorf("fail_threshold must be >= 1, got %d", hc.FailThreshold)
	}
	if hc.SuccessThreshold < 1 {
		return fmt.Errorf("success_threshold must be >= 1, got %d", hc.SuccessThreshold)
	}
	if hc.MaxConcurrency < 1 || hc.MaxConcurrency > 512 {
		return fmt.Errorf("max_concurrency must be in [1, 512], got %d", hc.MaxConcurrency)
	}
	return nil
}

// validate checks CircuitBreakerConfig for logical errors.
func (cb *CircuitBreakerConfig) validate(mode Mode) error {
	if !cb.Enabled {
		return nil
	}
	if mode == ModeGateway {
		return fmt.Errorf("circuit breaker is not supported in gateway mode")
	}
	if cb.FailThreshold < 1 {
		return fmt.Errorf("fail_threshold must be >= 1, got %d", cb.FailThreshold)
	}
	if cb.SuccessThreshold < 1 {
		return fmt.Errorf("success_threshold must be >= 1, got %d", cb.SuccessThreshold)
	}
	if cb.OpenDuration.Duration <= 0 {
		return fmt.Errorf("open_duration must be > 0")
	}
	return nil
}

// validate checks RateLimitConfig for logical errors.
func (rl *RateLimitConfig) validate(mode Mode) error {
	if rl.GlobalMaxInflight < 0 {
		return fmt.Errorf("global_max_inflight must be >= 0, got %d", rl.GlobalMaxInflight)
	}
	if rl.GlobalMaxInflight > 0 && mode == ModeGateway {
		return fmt.Errorf("global_max_inflight is not supported in gateway mode (scheduler manages global limit)")
	}

	if !rl.Enabled {
		return nil
	}
	if mode == ModeScheduler {
		return fmt.Errorf("gateway-side rate limiting is not supported in scheduler-only mode")
	}

	switch rl.Mode {
	case "concurrency":
		if rl.MaxConcurrentRequests < 1 {
			return fmt.Errorf("max_concurrent_requests must be >= 1, got %d", rl.MaxConcurrentRequests)
		}
	case "rate":
		if rl.RatePerSecond <= 0 {
			return fmt.Errorf("rate_per_second must be > 0 when mode is \"rate\", got %f", rl.RatePerSecond)
		}
		if rl.Burst < 1 {
			// Use MaxConcurrentRequests as default burst if not set.
			if rl.MaxConcurrentRequests >= 1 {
				rl.Burst = rl.MaxConcurrentRequests
			} else {
				return fmt.Errorf("burst must be >= 1 when mode is \"rate\", got %d", rl.Burst)
			}
		}
	default:
		return fmt.Errorf("mode must be \"concurrency\" or \"rate\", got %q", rl.Mode)
	}

	if rl.QueueSize < 0 {
		return fmt.Errorf("queue_size must be >= 0, got %d", rl.QueueSize)
	}
	if rl.QueueSize > 0 && rl.QueueTimeout.Duration <= 0 {
		return fmt.Errorf("queue_timeout must be > 0 when queue_size > 0")
	}
	return nil
}

// validate checks TracingConfig for logical errors.
func (tc *TracingConfig) validate() error {
	if !tc.Enabled {
		return nil
	}
	if tc.Endpoint == "" {
		return fmt.Errorf("endpoint is required when tracing is enabled")
	}
	if tc.SampleRate < 0 || tc.SampleRate > 1 {
		return fmt.Errorf("sample_rate must be in [0.0, 1.0], got %f", tc.SampleRate)
	}
	return nil
}

// validate checks WaitingQueueConfig for logical errors.
func (wq *WaitingQueueConfig) validate(mode Mode) error {
	if !wq.Enabled || mode == ModeGateway {
		return nil
	}
	if wq.MaxSize < 0 {
		return fmt.Errorf("max_size must be >= 0, got %d", wq.MaxSize)
	}
	if wq.Timeout.Duration < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	return nil
}

// validate checks SplitwiseConfig for logical errors.
func (sc *SplitwiseConfig) validate() error {
	if !sc.Enabled {
		return nil
	}
	if err := policy.ValidatePrefillPolicy(sc.PrefillPolicy); err != nil {
		return err
	}
	if err := policy.ValidateDecodePolicy(sc.DecodePolicy); err != nil {
		return err
	}
	if sc.MaxRescheduleRetries < 0 {
		return fmt.Errorf("max_reschedule_retries must be >= 0, got %d", sc.MaxRescheduleRetries)
	}
	if sc.MaxAllocRetries < 1 {
		return fmt.Errorf("max_alloc_retries must be >= 1, got %d", sc.MaxAllocRetries)
	}
	if sc.BackendResponseHeaderTimeout.Duration < 0 {
		return fmt.Errorf("backend_response_header_timeout must be >= 0")
	}
	if sc.CacheBlockSize < 1 {
		return fmt.Errorf("cache_block_size must be >= 1, got %d", sc.CacheBlockSize)
	}
	return nil
}

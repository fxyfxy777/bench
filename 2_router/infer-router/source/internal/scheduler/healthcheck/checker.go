// Package healthcheck provides an independent background health checker
// that periodically probes backend instances' /health endpoint and updates
// NodeState.Healthy. It is fully decoupled from MetricsCollector:
//   - Uses a dedicated /health endpoint (not /metrics)
//   - Requires SuccessThreshold consecutive successes to recover (SGLang pattern)
//   - Both transport errors AND HTTP non-200 count as failures
package healthcheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

// NodeStateGetter provides read-only access to the current node set.
// Same contract as collector.NodeStateGetter but kept separate to avoid
// coupling between the two packages.
type NodeStateGetter interface {
	GetNodes() map[string]*domain.NodeState
}

// Config configures the Checker.
type Config struct {
	Interval         time.Duration // probe interval (default: 3s)
	Timeout          time.Duration // per-instance probe timeout (default: 2s)
	FailThreshold    int           // consecutive failures to mark unhealthy (default: 3)
	SuccessThreshold int           // consecutive successes to recover from unhealthy (default: 2)
	HealthPath       string        // health endpoint path (default: "/health")
	MaxConcurrency   int           // max concurrent probes (default: 128)
}

// applyDefaults fills zero-value fields with sensible defaults.
func (cfg *Config) applyDefaults() {
	if cfg.Interval <= 0 {
		cfg.Interval = 3 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = 3
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = 2
	}
	if cfg.HealthPath == "" {
		cfg.HealthPath = "/health"
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 128
	}
}

// Checker periodically probes backend instances' /health endpoint
// and writes NodeState.Healthy. Runs as an independent background goroutine.
// Does NOT read or write load metadata -- that is MetricsCollector's job.
type Checker struct {
	state  NodeStateGetter
	client *http.Client
	cfg    Config
	log    *zap.Logger

	// per-instance tracking -- accessed by concurrent probe workers, protected by mu.
	mu                   sync.Mutex
	consecutiveFails     map[string]int
	consecutiveSuccesses map[string]int

	// Overlap guard: prevents sweep overlap if previous sweep exceeds interval.
	sweeping atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a Checker. The caller must call Start() to begin probing.
//
// Parameters:
//   - state: provides access to the current node set (read-only)
//   - client: shared HTTP client for health probes (may be nil, will use default)
//   - cfg: checker configuration (zero values are replaced with defaults)
//   - log: zap logger
func New(state NodeStateGetter, client *http.Client, cfg Config, log *zap.Logger) *Checker {
	cfg.applyDefaults()

	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}

	return &Checker{
		state:                state,
		client:               client,
		cfg:                  cfg,
		log:                  log.Named("health_checker"),
		consecutiveFails:     make(map[string]int),
		consecutiveSuccesses: make(map[string]int),
	}
}

// Start begins the background probe loop. Non-blocking.
func (c *Checker) Start() {
	c.ctx, c.cancel = context.WithCancel(context.Background())
	go c.loop()
}

// Stop signals the checker to stop. The current sweep (if any) finishes naturally
// because probe workers respect context cancellation.
func (c *Checker) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

// sweepCtx returns the checker's context, or context.Background() if Start() was not called.
// This allows sweep() to be called directly in tests without calling Start().
func (c *Checker) sweepCtx() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

// loop runs the periodic probe ticker.
func (c *Checker) loop() {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.sweep()
		}
	}
}

// sweep performs one complete round of health-probing all instances.
func (c *Checker) sweep() {
	// Overlap guard: skip if previous sweep is still running.
	if !c.sweeping.CompareAndSwap(false, true) {
		c.log.Warn("health sweep skipped: previous sweep still running",
			logger.Event(logger.EventHealthProbe))
		return
	}
	defer c.sweeping.Store(false)

	nodes := c.state.GetNodes()
	if len(nodes) == 0 {
		return
	}

	start := time.Now()

	// Bounded concurrency via semaphore channel.
	sem := make(chan struct{}, c.cfg.MaxConcurrency)
	var unhealthyCount int64

	var wg sync.WaitGroup
	for id, ns := range nodes {
		sem <- struct{}{} // acquire slot
		wg.Go(func() {
			defer func() { <-sem }() // release slot
			if c.probeOne(id, ns) {
				atomic.AddInt64(&unhealthyCount, 1)
			}
		})
	}
	wg.Wait()

	elapsed := time.Since(start)

	// Update Prometheus metrics.
	metrics.HealthProbeUnhealthyInstances.Set(float64(unhealthyCount))
	metrics.HealthSweepDuration.Observe(elapsed.Seconds())

	c.log.Info("health sweep completed",
		logger.Event(logger.EventHealthProbe),
		zap.Int("total", len(nodes)),
		zap.Int64("unhealthy", unhealthyCount),
		zap.Duration("elapsed", elapsed),
	)
}

// probeOne probes a single instance's health endpoint.
// Returns true if the instance is currently unhealthy (for counting).
func (c *Checker) probeOne(instanceID string, ns *domain.NodeState) bool {
	inst := ns.Instance
	if inst == nil {
		return false
	}

	endpoint := inst.MetricsEndpoint
	if endpoint == "" {
		endpoint = inst.Endpoint
	}
	if endpoint == "" {
		return false
	}

	url := fmt.Sprintf("http://%s%s", endpoint, c.cfg.HealthPath)

	ctx, cancel := context.WithTimeout(c.sweepCtx(), c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.log.Error("failed to create health probe request",
			logger.Event(logger.EventHealthProbe),
			zap.String("instance_id", instanceID),
			zap.String("url", url),
			zap.Error(err),
		)
		return !ns.LoadHealthy()
	}

	resp, err := c.client.Do(req)
	if err != nil {
		// Transport error: connection refused, timeout, DNS failure.
		return c.handleFailure(instanceID, ns, err)
	}
	defer func() {
		// Drain body to allow connection reuse.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		// HTTP non-200 also counts as failure for health checks
		// (unlike MetricsCollector where only transport errors affect health).
		return c.handleFailure(instanceID, ns, fmt.Errorf("health endpoint %s returned status %d", url, resp.StatusCode))
	}

	// Success path.
	return c.handleSuccess(instanceID, ns)
}

// handleFailure increments consecutive failures, resets consecutive successes,
// and marks the instance unhealthy if the failure threshold is reached.
// Returns true if the instance is currently unhealthy.
func (c *Checker) handleFailure(instanceID string, ns *domain.NodeState, err error) bool {
	c.mu.Lock()
	c.consecutiveFails[instanceID]++
	fails := c.consecutiveFails[instanceID]
	c.consecutiveSuccesses[instanceID] = 0
	c.mu.Unlock()

	metrics.HealthProbeResults.WithLabelValues("failure").Inc()

	if fails >= c.cfg.FailThreshold {
		wasHealthy := ns.LoadHealthy()
		ns.StoreHealthy(false)

		if wasHealthy {
			metrics.HealthProbeResults.WithLabelValues("unhealthy_mark").Inc()
			c.log.Warn("instance marked unhealthy by health checker",
				logger.Event(logger.EventInstanceUnhealthy),
				zap.String("instance_id", instanceID),
				zap.Int("consecutive_fails", fails),
				zap.Int("fail_threshold", c.cfg.FailThreshold),
				zap.Error(err),
			)
		}
		return true // currently unhealthy
	}

	return !ns.LoadHealthy()
}

// handleSuccess increments consecutive successes, resets consecutive failures,
// and marks the instance healthy if it was unhealthy and the success threshold is reached.
// Returns true if the instance is currently unhealthy.
func (c *Checker) handleSuccess(instanceID string, ns *domain.NodeState) bool {
	c.mu.Lock()
	c.consecutiveSuccesses[instanceID]++
	successes := c.consecutiveSuccesses[instanceID]
	c.consecutiveFails[instanceID] = 0
	c.mu.Unlock()

	metrics.HealthProbeResults.WithLabelValues("success").Inc()

	wasHealthy := ns.LoadHealthy()
	if !wasHealthy && successes >= c.cfg.SuccessThreshold {
		ns.StoreHealthy(true)
		metrics.HealthProbeResults.WithLabelValues("recovery").Inc()
		c.log.Info("instance recovered via health probe",
			logger.Event(logger.EventHealthRecovery),
			zap.String("instance_id", instanceID),
			zap.Int("consecutive_successes", successes),
			zap.Int("success_threshold", c.cfg.SuccessThreshold),
		)
		return false // now healthy
	}

	return !wasHealthy
}

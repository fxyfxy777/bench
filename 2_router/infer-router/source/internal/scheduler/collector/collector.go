package collector

import (
	"cmp"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

const (
	// MaxCacheUsagePerc is the maximum GPU cache usage percentage (100%).
	MaxCacheUsagePerc = 1.0
	// MinCacheUsagePerc is the minimum GPU cache usage percentage for calculation (1%).
	// Below this, we assume 1% to avoid division by zero and extreme values.
	MinCacheUsagePerc = 0.01
)

// calculateTotalBlocks estimates total GPU KV block capacity from available
// blocks and cache usage percentage. Formula: total = available / (1 - usage).
// When usage is 0 or >= 1, returns available as a safe fallback.
func calculateTotalBlocks(available int64, cacheUsagePerc float64) int64 {
	if cacheUsagePerc <= 0 {
		// No usage info: assume blocks are all available (conservative)
		return available
	}
	if cacheUsagePerc >= MaxCacheUsagePerc {
		// 100% usage: can't calculate total, use available as fallback
		return available
	}
	// Clamp to minimum to avoid extreme values when usage is very low
	usage := max(cacheUsagePerc, MinCacheUsagePerc)
	return int64(float64(available) / (MaxCacheUsagePerc - usage))
}

// Option configures a MetricsCollector.
type Option func(*MetricsCollector)

// WithUpdater sets the MetaUpdater for feeding load metrics to the policy.
// If nil, metrics are still scraped but not forwarded to any policy.
func WithUpdater(u MetaUpdater) Option {
	return func(c *MetricsCollector) { c.updater = u }
}

// WithMaxConcurrency sets the maximum number of concurrent scrape workers.
func WithMaxConcurrency(n int) Option {
	return func(c *MetricsCollector) { c.maxConcurrency = n }
}

// MetricsCollector periodically scrapes backend instances for load metrics.
// It runs as a background goroutine and feeds data into the scheduling policy
// via MetaUpdater. Health checking is handled by a separate HealthChecker.
type MetricsCollector struct {
	fetchers       map[string]MetricsFetcher // backend_type -> fetcher
	defaultFetcher MetricsFetcher            // fallback when BackendType is empty
	updater        MetaUpdater               // nil = no-op (metrics still scraped but not forwarded)
	state          NodeStateGetter
	log            *zap.Logger

	// Config
	scrapeInterval time.Duration
	scrapeTimeout  time.Duration
	maxConcurrency int

	// Reusable update buffer
	updateBuf []InstanceMetaUpdate

	// Overlap guard: prevents sweep overlap if previous sweep exceeds interval.
	sweeping atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a MetricsCollector.
//
// Parameters:
//   - fetchers: map of backend_type -> MetricsFetcher (at least one entry or defaultFetcher)
//   - defaultFetcher: used when Instance.BackendType is empty (may be nil if all instances have types)
//   - state: provides access to the current node set
//   - scrapeInterval: fixed interval between sweeps
//   - scrapeTimeout: per-instance scrape timeout
//   - log: zap logger
//   - opts: functional options for additional configuration
func New(
	fetchers map[string]MetricsFetcher,
	defaultFetcher MetricsFetcher,
	state NodeStateGetter,
	scrapeInterval, scrapeTimeout time.Duration,
	log *zap.Logger,
	opts ...Option,
) *MetricsCollector {
	c := &MetricsCollector{
		fetchers:       fetchers,
		defaultFetcher: defaultFetcher,
		state:          state,
		log:            log,
		scrapeInterval: scrapeInterval,
		scrapeTimeout:  scrapeTimeout,
		maxConcurrency: 128,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Start begins the background scrape loop. Non-blocking.
func (c *MetricsCollector) Start() {
	c.ctx, c.cancel = context.WithCancel(context.Background())
	go c.loop()
}

// Stop signals the collector to stop and waits for the current sweep to finish.
func (c *MetricsCollector) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

// sweepCtx returns the collector's context, or background if Start() was not called.
// This allows sweep() to be called directly in tests.
func (c *MetricsCollector) sweepCtx() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

// loop runs the periodic sweep ticker.
func (c *MetricsCollector) loop() {
	ticker := time.NewTicker(c.scrapeInterval)
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

// sweep performs one complete round of scraping all instances.
func (c *MetricsCollector) sweep() {
	// Overlap guard: skip if previous sweep is still running.
	if !c.sweeping.CompareAndSwap(false, true) {
		c.log.Warn("metrics sweep skipped: previous sweep still running",
			logger.Event(logger.EventMetricsSweep))
		return
	}
	defer c.sweeping.Store(false)

	nodes := c.state.GetNodes()
	total := len(nodes)
	if total == 0 {
		return
	}

	start := time.Now()

	// Prepare update buffer.
	c.updateBuf = c.updateBuf[:0]
	if cap(c.updateBuf) < total {
		c.updateBuf = make([]InstanceMetaUpdate, 0, total)
	}

	// Bounded concurrency via semaphore.
	sem := make(chan struct{}, c.maxConcurrency)
	var mu sync.Mutex
	var failures int

	var wg sync.WaitGroup
	for id, ns := range nodes {
		sem <- struct{}{} // acquire
		wg.Add(1)
		go func(instanceID string, ns *domain.NodeState) {
			defer func() {
				<-sem // release
				wg.Done()
			}()
			c.scrapeOne(instanceID, ns, &mu, &failures)
		}(id, ns)
	}
	wg.Wait()

	// Feed updates to the policy (if supported).
	mu.Lock()
	updates := c.updateBuf
	mu.Unlock()

	if c.updater != nil && len(updates) > 0 {
		c.updater.BatchUpdateInstanceMeta(updates)
	}

	elapsed := time.Since(start)

	// Update Prometheus metrics.
	metrics.MetricsSweepDuration.Observe(elapsed.Seconds())
	metrics.MetricsSweepInstances.Set(float64(total))
	metrics.MetricsSweepFailures.Set(float64(failures))

	c.log.Debug("metrics sweep completed",
		logger.Event(logger.EventMetricsSweep),
		zap.Int("total", total),
		zap.Int("failures", failures),
		zap.Duration("elapsed", elapsed),
	)
}

// scrapeOne scrapes a single instance and appends load metrics to updateBuf.
// Health checking is handled by a separate HealthChecker component.
func (c *MetricsCollector) scrapeOne(
	instanceID string,
	ns *domain.NodeState,
	mu *sync.Mutex,
	failures *int,
) {
	inst := ns.Instance
	if inst == nil {
		return
	}

	endpoint := cmp.Or(inst.MetricsEndpoint, inst.Endpoint)
	if endpoint == "" {
		return
	}

	fetcher := c.fetcherFor(inst.BackendType)
	if fetcher == nil {
		return
	}

	ctx, cancel := context.WithTimeout(c.sweepCtx(), c.scrapeTimeout)
	defer cancel()

	m, err := fetcher.Fetch(ctx, endpoint)
	if err != nil {
		c.log.Warn("metrics fetch failed",
			logger.Event(logger.EventMetricsSweep),
			zap.String("instance_id", instanceID),
			zap.Error(err))
		mu.Lock()
		*failures++
		mu.Unlock()
		return
	}

	mu.Lock()
	c.updateBuf = append(c.updateBuf, InstanceMetaUpdate{
		InstanceID:      instanceID,
		WaitingCount:    m.WaitingCount,
		AvailableBlocks: m.AvailableBlocks,
		TotalBlocks:     calculateTotalBlocks(m.AvailableBlocks, m.GpuCacheUsagePerc),
	})
	mu.Unlock()
}

// fetcherFor returns the MetricsFetcher for the given backend type,
// falling back to defaultFetcher if no specific fetcher is registered.
func (c *MetricsCollector) fetcherFor(backendType string) MetricsFetcher {
	if backendType != "" {
		if f, ok := c.fetchers[backendType]; ok {
			return f
		}
	}
	return c.defaultFetcher
}

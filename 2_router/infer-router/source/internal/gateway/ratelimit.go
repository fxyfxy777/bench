package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"

	"github.com/yzx/rl-router/pkg/metrics"
)

// Sentinel errors returned by FlowController.Acquire.
var (
	ErrQueueFull    = errors.New("rate limit: queue full")
	ErrQueueTimeout = errors.New("rate limit: queue timeout")
)

// FlowController abstracts gateway-local request admission control.
//
// Two implementations:
//   - ConcurrencyLimiter: semaphore mode (RL training, limit in-flight)
//   - RateLimiter:        token bucket mode (online inference, limit RPS)
type FlowController interface {
	// Acquire blocks until a token is available, the queue is full,
	// the queue timeout expires, or ctx is cancelled.
	Acquire(ctx context.Context) error

	// TryAcquire is the non-blocking fast path. Returns true if a token
	// was acquired immediately, false otherwise.
	TryAcquire() bool

	// Release returns a token. Must be called exactly once after a
	// successful Acquire or TryAcquire.
	// For RateLimiter (token bucket with refill), Release is a no-op
	// since tokens are auto-refilled by time.
	Release()
}

// ---------- ConcurrencyLimiter ----------
//
// Limits simultaneous in-flight requests using a weighted semaphore.
// Engine: golang.org/x/sync/semaphore.Weighted (FIFO wait list, context-aware).
// Typical use: RL training rollout — protect backend GPU instances from overload.

// ConcurrencyLimiter implements FlowController using a counting semaphore.
type ConcurrencyLimiter struct {
	sem          *semaphore.Weighted
	maxQueue     int
	queueTimeout time.Duration
	queueDepth   atomic.Int64
	logger       *zap.Logger
}

// NewConcurrencyLimiter creates a FlowController that limits concurrent
// in-flight requests to maxConcurrent, with an optional bounded queue.
func NewConcurrencyLimiter(maxConcurrent, maxQueue int, queueTimeout time.Duration, logger *zap.Logger) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		sem:          semaphore.NewWeighted(int64(maxConcurrent)),
		maxQueue:     maxQueue,
		queueTimeout: queueTimeout,
		logger:       logger,
	}
}

// Acquire attempts to obtain a concurrency token.
//
// Fast path: tries non-blocking acquire first.
// Slow path: queues up to maxQueue waiters with queueTimeout.
func (cl *ConcurrencyLimiter) Acquire(ctx context.Context) error {
	// Fast path — non-blocking try.
	if cl.sem.TryAcquire(1) {
		metrics.RateLimitTotal.WithLabelValues("acquired").Inc()
		metrics.RateLimitConcurrentRequests.Inc()
		return nil
	}

	// Check queue capacity.
	if cl.maxQueue <= 0 || cl.queueDepth.Load() >= int64(cl.maxQueue) {
		metrics.RateLimitTotal.WithLabelValues("rejected_full").Inc()
		return ErrQueueFull
	}

	// Enter queue.
	depth := cl.queueDepth.Add(1)
	metrics.RateLimitQueueDepth.Set(float64(depth))
	waitStart := time.Now()

	defer func() {
		newDepth := cl.queueDepth.Add(-1)
		metrics.RateLimitQueueDepth.Set(float64(newDepth))
	}()

	// Wait with timeout.
	timeoutCtx, cancel := context.WithTimeout(ctx, cl.queueTimeout)
	defer cancel()

	err := cl.sem.Acquire(timeoutCtx, 1)
	waitMs := float64(time.Since(waitStart).Milliseconds())

	if err != nil {
		// Distinguish queue timeout from caller cancellation.
		if ctx.Err() != nil {
			// Original context was cancelled (client disconnect).
			return ctx.Err()
		}
		metrics.RateLimitTotal.WithLabelValues("rejected_timeout").Inc()
		metrics.RateLimitWaitDuration.Observe(waitMs)
		return ErrQueueTimeout
	}

	metrics.RateLimitTotal.WithLabelValues("queued_acquired").Inc()
	metrics.RateLimitWaitDuration.Observe(waitMs)
	metrics.RateLimitConcurrentRequests.Inc()
	return nil
}

// TryAcquire is the non-blocking fast path.
func (cl *ConcurrencyLimiter) TryAcquire() bool {
	if cl.sem.TryAcquire(1) {
		metrics.RateLimitTotal.WithLabelValues("acquired").Inc()
		metrics.RateLimitConcurrentRequests.Inc()
		return true
	}
	return false
}

// Release returns one concurrency token.
func (cl *ConcurrencyLimiter) Release() {
	cl.sem.Release(1)
	metrics.RateLimitConcurrentRequests.Dec()
}

// ---------- RateLimiter ----------
//
// Limits request rate (RPS) using a token bucket with auto-refill.
// Engine: golang.org/x/time/rate.Limiter (battle-tested, context-aware).
// Typical use: online inference — smooth traffic to protect backend throughput.

// RateLimiter implements FlowController using a token bucket.
type RateLimiter struct {
	limiter      *rate.Limiter
	maxQueue     int
	queueTimeout time.Duration
	queueDepth   atomic.Int64
	logger       *zap.Logger
}

// NewRateLimiter creates a FlowController that limits request rate to
// rps tokens per second, allowing bursts up to burst.
func NewRateLimiter(rps float64, burst, maxQueue int, queueTimeout time.Duration, logger *zap.Logger) *RateLimiter {
	return &RateLimiter{
		limiter:      rate.NewLimiter(rate.Limit(rps), burst),
		maxQueue:     maxQueue,
		queueTimeout: queueTimeout,
		logger:       logger,
	}
}

// Acquire attempts to consume one token from the bucket.
//
// Fast path: tries non-blocking Allow first.
// Slow path: queues up to maxQueue waiters with queueTimeout.
func (rl *RateLimiter) Acquire(ctx context.Context) error {
	// Fast path — non-blocking.
	if rl.limiter.Allow() {
		metrics.RateLimitTotal.WithLabelValues("acquired").Inc()
		return nil
	}

	// Check queue capacity.
	if rl.maxQueue <= 0 || rl.queueDepth.Load() >= int64(rl.maxQueue) {
		metrics.RateLimitTotal.WithLabelValues("rejected_full").Inc()
		return ErrQueueFull
	}

	// Enter queue.
	depth := rl.queueDepth.Add(1)
	metrics.RateLimitQueueDepth.Set(float64(depth))
	waitStart := time.Now()

	defer func() {
		newDepth := rl.queueDepth.Add(-1)
		metrics.RateLimitQueueDepth.Set(float64(newDepth))
	}()

	// Wait for token refill with timeout.
	timeoutCtx, cancel := context.WithTimeout(ctx, rl.queueTimeout)
	defer cancel()

	err := rl.limiter.Wait(timeoutCtx)
	waitMs := float64(time.Since(waitStart).Milliseconds())

	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		metrics.RateLimitTotal.WithLabelValues("rejected_timeout").Inc()
		metrics.RateLimitWaitDuration.Observe(waitMs)
		return ErrQueueTimeout
	}

	metrics.RateLimitTotal.WithLabelValues("queued_acquired").Inc()
	metrics.RateLimitWaitDuration.Observe(waitMs)
	return nil
}

// TryAcquire is the non-blocking fast path.
func (rl *RateLimiter) TryAcquire() bool {
	if rl.limiter.Allow() {
		metrics.RateLimitTotal.WithLabelValues("acquired").Inc()
		return true
	}
	return false
}

// Release is a no-op for RateLimiter. Token bucket tokens are auto-refilled
// by time, not by explicit return. This method exists to satisfy the
// FlowController interface, making `defer limiter.Release()` safe for both modes.
func (rl *RateLimiter) Release() {}

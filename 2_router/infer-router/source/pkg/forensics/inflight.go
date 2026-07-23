package forensics

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/yzx/rl-router/pkg/metrics"
)

// InflightTracker tracks in-flight requests and periodically samples their age
// into a Prometheus histogram. This allows detecting hung requests without
// per-request goroutines.
//
// Usage:
//
//	tracker := NewInflightTracker()
//	tracker.Start(5 * time.Second)
//	defer tracker.Stop()
//
//	tracker.Track(requestID)
//	defer tracker.Untrack(requestID)
type InflightTracker struct {
	entries sync.Map // requestID (string) → startNano (int64)
	stopCh  chan struct{}
	done    chan struct{}
	started atomic.Bool // true after Start() is called
}

// NewInflightTracker creates a new tracker. Call Start to begin periodic sweeps.
func NewInflightTracker() *InflightTracker {
	return &InflightTracker{
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Track registers a request as in-flight using the current time.
func (t *InflightTracker) Track(requestID string) {
	t.entries.Store(requestID, time.Now().UnixNano())
}

// Untrack removes a request from the in-flight set.
func (t *InflightTracker) Untrack(requestID string) {
	t.entries.Delete(requestID)
}

// Count returns the number of currently tracked in-flight requests.
func (t *InflightTracker) Count() int {
	count := 0
	t.entries.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// Start begins the periodic sweep goroutine that samples inflight request
// ages into the rl_router_inflight_request_age_seconds histogram.
func (t *InflightTracker) Start(interval time.Duration) {
	t.started.Store(true)
	go t.sweepLoop(interval)
}

// Stop terminates the sweep goroutine and waits for it to finish.
// Safe to call even if Start was never called (no-op in that case).
func (t *InflightTracker) Stop() {
	if !t.started.Load() {
		return
	}
	close(t.stopCh)
	<-t.done
}

func (t *InflightTracker) sweepLoop(interval time.Duration) {
	defer close(t.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.sweep()
		}
	}
}

func (t *InflightTracker) sweep() {
	now := time.Now().UnixNano()
	t.entries.Range(func(_, value any) bool {
		startNano := value.(int64)
		ageSec := float64(now-startNano) / 1e9
		metrics.InflightRequestAge.Observe(ageSec)
		return true
	})
}

// InflightSnapshot represents a single in-flight request for admin/debug endpoints.
type InflightSnapshot struct {
	RequestID string        `json:"request_id"`
	Age       time.Duration `json:"age"`
}

// Snapshot returns all currently in-flight requests with their ages.
// Intended for admin/debug endpoints, not for hot-path use.
func (t *InflightTracker) Snapshot() []InflightSnapshot {
	now := time.Now().UnixNano()
	var result []InflightSnapshot
	t.entries.Range(func(key, value any) bool {
		requestID := key.(string)
		startNano := value.(int64)
		result = append(result, InflightSnapshot{
			RequestID: requestID,
			Age:       time.Duration(now - startNano),
		})
		return true
	})
	return result
}

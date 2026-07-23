package scheduler

// server_queue.go contains normal and PD waiting queue management: enqueue,
// drain, reject, pause/continue, and queue getters.
// All mutating methods are called exclusively from the event-loop (single-threaded).
// Getter methods use atomic reads and are safe to call from any goroutine.

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

type queueDrainMetrics struct {
	drainTotal   *prometheus.CounterVec
	waitDuration prometheus.Observer
}

func (m queueDrainMetrics) observe(result string, waitMs float64) {
	m.drainTotal.WithLabelValues(result).Inc()
	m.waitDuration.Observe(waitMs)
}

type queueDrainStats struct {
	drained   int
	timedOut  int
	cancelled int
}

func queueWaitMs(ev *event, now time.Time) float64 {
	return float64(now.Sub(ev.queueEntryTime).Microseconds()) / 1000.0
}

func (s *Server) finishQueuedIfCancelled(
	ev *event,
	now time.Time,
	stats *queueDrainStats,
	qm queueDrainMetrics,
	message string,
) bool {
	select {
	case <-ev.ctx.Done():
		requestID := ev.route.RequestID
		ctxErr := ev.ctx.Err()
		waitMs := ev.finishQueued(now, ctxErr)
		s.accessLog.Debug(message,
			logger.Event(logger.EventQueueCancel),
			zap.String("request_id", requestID),
			zap.Float64("wait_ms", waitMs))
		stats.cancelled++
		qm.observe("cancelled", waitMs)
		return true
	default:
		return false
	}
}

func (s *Server) finishQueuedIfTimedOut(
	ev *event,
	now time.Time,
	stats *queueDrainStats,
	qm queueDrainMetrics,
	message string,
) bool {
	if now.Sub(ev.queueEntryTime) < s.queueTimeout {
		return false
	}
	requestID := ev.route.RequestID
	waitMs := ev.finishQueued(now, ErrWaitingQueueTimeout)
	s.accessLog.Warn(message,
		logger.Event(logger.EventQueueTimeout),
		zap.String("request_id", requestID),
		zap.Float64("wait_ms", waitMs),
		zap.Duration("queue_timeout", s.queueTimeout))
	stats.timedOut++
	qm.observe("timeout", waitMs)
	return true
}

func keepQueueTail(queue []*event, readIdx, writeIdx int) int {
	queue[writeIdx] = queue[readIdx]
	writeIdx++
	for readIdx++; readIdx < len(queue); readIdx++ {
		queue[writeIdx] = queue[readIdx]
		writeIdx++
	}
	return writeIdx
}

func compactQueue(queue *[]*event, writeIdx int, depthAtomic *atomic.Int64, depthGauge prometheus.Gauge) {
	for i := writeIdx; i < len(*queue); i++ {
		(*queue)[i] = nil
	}
	*queue = (*queue)[:writeIdx]
	depthAtomic.Store(int64(writeIdx))
	depthGauge.Set(float64(writeIdx))
}

// tryEnqueue is the shared capacity-check + append used by both the centralized
// and PD waiting queues. Returns false when the queue is at queueMaxSize so the
// caller can reject the event; on success it transfers ownership to the queue
// (markEventQueued) and updates depth/enqueue metrics. Called only from the
// event-loop.
func (s *Server) tryEnqueue(
	queue *[]*event,
	depthAtomic *atomic.Int64,
	depthGauge prometheus.Gauge,
	enqueueTotal prometheus.Counter,
	ev *event,
	queueName string,
) bool {
	if int64(len(*queue)) >= s.queueMaxSize {
		s.accessLog.Warn(queueName+" full, rejecting request",
			logger.Event(logger.EventQueueFullReject),
			zap.String("request_id", ev.route.RequestID),
			zap.Int("queue_depth", len(*queue)),
			zap.Int64("max_size", s.queueMaxSize))
		return false
	}
	markEventQueued(ev)
	*queue = append(*queue, ev)
	depth := int64(len(*queue))
	depthAtomic.Store(depth)
	depthGauge.Set(float64(depth))
	enqueueTotal.Inc()
	s.accessLog.Debug("request enqueued to "+queueName,
		logger.Event(logger.EventQueueEnqueue),
		zap.String("request_id", ev.route.RequestID),
		zap.Int("queue_depth", int(depth)))
	return true
}

// rejectAll drains queue, sending reason on each event's result channel and
// returning the slot count back to the pool. Updates depth metrics and
// records a "step_reject" entry on drainTotal. Returns the number of rejected
// events. Called only from the event-loop during step transitions or pause.
func (s *Server) rejectAll(
	queue *[]*event,
	depthAtomic *atomic.Int64,
	depthGauge prometheus.Gauge,
	drainTotal *prometheus.CounterVec,
	queueName string,
	reason error,
) int {
	if len(*queue) == 0 {
		return 0
	}
	rejected := len(*queue)
	for _, ev := range *queue {
		ev.sendError(reason)
		domain.ReleaseRouteContext(ev.route)
		putEvent(ev)
	}
	for i := range *queue {
		(*queue)[i] = nil
	}
	*queue = (*queue)[:0]
	depthAtomic.Store(0)
	depthGauge.Set(0)
	drainTotal.WithLabelValues("step_reject").Add(float64(rejected))
	logMsg := "all queued requests rejected"
	if queueName != "" {
		logMsg = "all queued " + queueName + " requests rejected"
	}
	s.logger.Warn(logMsg,
		logger.Event(logger.EventQueueStepReject),
		zap.Int("rejected_count", rejected),
		zap.Error(reason))
	return rejected
}

// enqueueToWaitingQueue appends an event to the waiting queue.
// Returns false if the queue is full. Called only from the event-loop.
func (s *Server) enqueueToWaitingQueue(ev *event) bool {
	return s.tryEnqueue(
		&s.waitingQueue,
		&s.queueDepthAtomic,
		metrics.WaitingQueueDepth,
		metrics.WaitingQueueEnqueueTotal,
		ev,
		"waiting queue",
	)
}

// drainWaitingQueue attempts to allocate queued requests using freed capacity.
// Uses in-place compaction (writeIdx pattern) with early exit on ErrAllOverloaded.
// Called only from the event-loop after processBatch.
func (s *Server) drainWaitingQueue() {
	metrics.WaitingQueueDrainCycles.Inc()
	now := time.Now()
	nodes := s.currentNodes()
	stepID := s.stepID.Load()
	qm := queueDrainMetrics{
		drainTotal:   metrics.WaitingQueueDrainTotal,
		waitDuration: metrics.WaitingQueueWaitDuration,
	}

	s.preparePolicyForNodes(nodes)

	var stats queueDrainStats
	writeIdx := 0

	for readIdx := 0; readIdx < len(s.waitingQueue); readIdx++ {
		ev := s.waitingQueue[readIdx]

		if s.finishQueuedIfCancelled(ev, now, &stats, qm, "queued request cancelled by client") {
			continue
		}
		if s.finishQueuedIfTimedOut(ev, now, &stats, qm, "queued request timed out") {
			continue
		}

		// Check global rate limit headroom (must account for both normal and PD in-flight).
		if s.globalMaxInflight > 0 && s.totalActiveCount() >= s.globalMaxInflight {
			// Keep all remaining entries — no capacity.
			writeIdx = keepQueueTail(s.waitingQueue, readIdx, writeIdx)
			break
		}
		if s.activeGroup != nil && s.activeGroup.maxInflight > 0 && s.currentGroupActiveCount() >= s.activeGroup.maxInflight {
			writeIdx = keepQueueTail(s.waitingQueue, readIdx, writeIdx)
			break
		}

		// Try policy.Select.
		inst, err := s.policy.Select(ev.ctx, ev.route, nodes)
		if err != nil {
			if errors.Is(err, policy.ErrAllOverloaded) {
				// Early exit: all instances at max requestLoad — no request can succeed.
				writeIdx = keepQueueTail(s.waitingQueue, readIdx, writeIdx)
				metrics.WaitingQueueDrainCapped.Inc()
				break
			}
			if errors.Is(err, policy.ErrAllExceedSession) {
				// Session admission blocked: keep THIS request in queue but continue
				// processing subsequent entries. Existing-session requests bypass
				// admission control and may succeed even when new sessions cannot.
				// Without this, a new-session at the queue head blocks existing-session
				// requests behind it → inference stalls → session_finish never arrives
				// → deadlock.
				s.waitingQueue[writeIdx] = ev
				writeIdx++
				continue
			}
			waitMs := ev.finishQueued(now, err)
			qm.observe("error", waitMs)
			continue
		}

		// Success — allocate.
		waitMs := queueWaitMs(ev, now)
		requestID := ev.route.RequestID
		s.recordAllocation(ev, inst, stepID)
		s.accessLog.Debug("queued request allocated",
			logger.Event(logger.EventQueueDrained),
			zap.String("request_id", requestID),
			zap.String("instance", inst.ID),
			zap.Float64("wait_ms", waitMs))
		domain.ReleaseRouteContext(ev.route)
		putEvent(ev)
		stats.drained++
		qm.observe("success", waitMs)
	}

	compactQueue(&s.waitingQueue, writeIdx, &s.queueDepthAtomic, metrics.WaitingQueueDepth)
	s.auditDrainHealth()

	if stats.drained > 0 || stats.timedOut > 0 || stats.cancelled > 0 {
		s.accessLog.Debug("drain cycle completed",
			logger.Event(logger.EventQueueDrainCycle),
			zap.Int("drained", stats.drained),
			zap.Int("timed_out", stats.timedOut),
			zap.Int("cancelled", stats.cancelled),
			zap.Int("remaining", len(s.waitingQueue)))
	}
}

// auditDrainHealth tracks consecutive drain cycles without a capacity-freeing event.
// Emits a warning after 10 cycles to detect stuck requests.
// Includes queue composition (new vs existing sessions) to aid root-cause analysis.
func (s *Server) auditDrainHealth() {
	if s.lastDrainHadCapacityFree {
		s.drainCyclesWithoutRelease = 0
		s.lastDrainHadCapacityFree = false
	} else if len(s.waitingQueue) > 0 {
		s.drainCyclesWithoutRelease++
	}
	if s.drainCyclesWithoutRelease >= 10 && len(s.waitingQueue) > 0 {
		// Count new-session vs existing-session requests to surface head-of-line blocking.
		var newSession, existingSession int
		if sr, ok := s.policy.(policy.SessionQuerier); ok {
			for _, ev := range s.waitingQueue {
				if ev.route != nil && ev.route.SessionID != "" && sr.HasSession(ev.route.SessionID) {
					existingSession++
				} else {
					newSession++
				}
			}
		}
		s.logger.Warn("queue non-empty but no capacity-freeing event (release/session_remove/gateway_cleanup)",
			logger.Event(logger.EventNoReleaseAlarm),
			zap.Int("queue_depth", len(s.waitingQueue)),
			zap.Int("cycles_without_capacity_free", s.drainCyclesWithoutRelease),
			zap.Int("new_sessions_queued", newSession),
			zap.Int("existing_sessions_queued", existingSession))
	}
}

// rejectWaitingQueue rejects all queued requests with the given error and clears the queue.
// Called only from the event-loop during step transitions.
func (s *Server) rejectWaitingQueue(reason error) {
	s.rejectAll(
		&s.waitingQueue,
		&s.queueDepthAtomic,
		metrics.WaitingQueueDepth,
		metrics.WaitingQueueDrainTotal,
		"",
		reason,
	)
}

// GetWaitingQueueDepth returns the aggregate normal waiting queue depth.
// Safe to call from any goroutine (reads atomic mirror).
func (s *Server) GetWaitingQueueDepth() int64 {
	return s.queueDepthAtomic.Load()
}

// IsQueueEnabled returns whether the waiting queue is currently enabled.
// Safe to call from any goroutine (reads atomic mirror).
func (s *Server) IsQueueEnabled() bool {
	return s.queueEnabledAtomic.Load()
}

// GetQueueMaxSize returns the current queue max size.
// Safe to call from any goroutine (reads atomic mirror).
func (s *Server) GetQueueMaxSize() int64 {
	return s.queueMaxSizeAtomic.Load()
}

// GetQueueTimeoutSec returns the current queue timeout in seconds.
// Safe to call from any goroutine (reads atomic mirror).
func (s *Server) GetQueueTimeoutSec() int64 {
	return s.queueTimeoutAtomic.Load()
}

// enqueueToPDWaitingQueue appends an event to the PD waiting queue.
// Returns false if the queue is full. Called only from the event-loop.
func (s *Server) enqueueToPDWaitingQueue(ev *event) bool {
	return s.tryEnqueue(
		&s.pdWaitingQueue,
		&s.pdQueueDepthAtomic,
		metrics.PDWaitingQueueDepth,
		metrics.PDWaitingQueueEnqueueTotal,
		ev,
		"pd waiting queue",
	)
}

// enqueuePDOrReject queues a PD allocation event or reports queue-full to the caller.
func (s *Server) enqueuePDOrReject(ev *event) {
	if !s.enqueueToPDWaitingQueue(ev) {
		ev.sendError(ErrWaitingQueueFull)
	}
}

// drainPDWaitingQueue attempts to allocate queued PD requests using freed capacity.
// Called only from the event-loop after processBatch.
func (s *Server) drainPDWaitingQueue() {
	now := time.Now()
	prefillNodes := s.currentNodesByRole("prefill")
	decodeNodes := s.currentNodesByRole("decode")
	stepID := s.stepID.Load()
	qm := queueDrainMetrics{
		drainTotal:   metrics.PDWaitingQueueDrainTotal,
		waitDuration: metrics.PDWaitingQueueWaitDuration,
	}

	var stats queueDrainStats
	writeIdx := 0

	for readIdx := 0; readIdx < len(s.pdWaitingQueue); readIdx++ {
		ev := s.pdWaitingQueue[readIdx]

		if s.finishQueuedIfCancelled(ev, now, &stats, qm, "queued pd request cancelled by client") {
			continue
		}
		if s.finishQueuedIfTimedOut(ev, now, &stats, qm, "queued pd request timed out") {
			continue
		}

		// Check global rate limit headroom (prefill + decode = 2 slots).
		if s.globalMaxInflight > 0 && s.activeCount+s.pdActiveCount+2 > s.globalMaxInflight {
			writeIdx = keepQueueTail(s.pdWaitingQueue, readIdx, writeIdx)
			break
		}
		if s.activeGroup != nil && s.activeGroup.maxInflight > 0 && s.currentGroupActiveCount()+2 > s.activeGroup.maxInflight {
			writeIdx = keepQueueTail(s.pdWaitingQueue, readIdx, writeIdx)
			break
		}

		// Keep and continue: nodes or policies may become available later.
		if len(prefillNodes) == 0 || len(decodeNodes) == 0 || s.pdPrefillPolicy == nil || s.pdDecodePolicy == nil {
			s.pdWaitingQueue[writeIdx] = ev
			writeIdx++
			continue
		}

		prefillInst, err := s.pdPrefillPolicy.Select(ev.ctx, ev.route, prefillNodes)
		if err != nil {
			s.logger.Warn("prefill policy queue select failed again",
				zap.Error(err),
				zap.String("session_id", ev.route.SessionID),
				zap.String("request_id", ev.route.RequestID))
			if errors.Is(err, policy.ErrAllOverloaded) {
				writeIdx = keepQueueTail(s.pdWaitingQueue, readIdx, writeIdx)
				break
			}
			waitMs := ev.finishQueued(now, err)
			qm.observe("error", waitMs)
			continue
		}

		decodeInst, err := s.pdDecodePolicy.Select(ev.ctx, ev.route, decodeNodes)
		if err != nil {
			s.logger.Warn("decode policy queue select failed again",
				zap.Error(err),
				zap.String("session_id", ev.route.SessionID),
				zap.String("request_id", ev.route.RequestID))
			// Roll back prefill policy's internal soft-counters to avoid load-count leak.
			s.pdPrefillPolicy.Feedback(prefillInst.ID, nil)

			if errors.Is(err, policy.ErrAllOverloaded) {
				writeIdx = keepQueueTail(s.pdWaitingQueue, readIdx, writeIdx)
				break
			}
			waitMs := ev.finishQueued(now, err)
			qm.observe("error", waitMs)
			continue
		}

		waitMs := queueWaitMs(ev, now)
		requestID := ev.route.RequestID
		s.recordPDAllocation(ev, prefillInst, decodeInst, stepID)
		s.accessLog.Debug("queued pd request allocated",
			logger.Event(logger.EventQueueDrained),
			zap.String("request_id", requestID),
			zap.String("prefill", prefillInst.ID),
			zap.String("decode", decodeInst.ID),
			zap.Float64("wait_ms", waitMs))
		domain.ReleaseRouteContext(ev.route)
		putEvent(ev)
		stats.drained++
		qm.observe("success", waitMs)
	}

	compactQueue(&s.pdWaitingQueue, writeIdx, &s.pdQueueDepthAtomic, metrics.PDWaitingQueueDepth)

	if stats.drained > 0 || stats.timedOut > 0 || stats.cancelled > 0 {
		s.accessLog.Debug("pd drain cycle completed",
			logger.Event(logger.EventQueueDrainCycle),
			zap.Int("drained", stats.drained),
			zap.Int("timed_out", stats.timedOut),
			zap.Int("cancelled", stats.cancelled),
			zap.Int("remaining", len(s.pdWaitingQueue)))
	}
}

// rejectPDWaitingQueue rejects all queued PD requests with the given error and clears the queue.
// Called only from the event-loop during step transitions.
func (s *Server) rejectPDWaitingQueue(reason error) {
	s.rejectAll(
		&s.pdWaitingQueue,
		&s.pdQueueDepthAtomic,
		metrics.PDWaitingQueueDepth,
		metrics.PDWaitingQueueDrainTotal,
		"pd",
		reason,
	)
}

// GetPDWaitingQueueDepth returns the aggregate PD waiting queue depth.
// Safe to call from any goroutine (reads atomic mirror).
func (s *Server) GetPDWaitingQueueDepth() int64 {
	return s.pdQueueDepthAtomic.Load()
}

// handlePause pauses all allocation (centralized + PD): rejects all queued
// requests in both waitingQueue and pdWaitingQueue and blocks new allocations
// until handleContinue is called.
// Called only from the event-loop (single-threaded).
func (s *Server) handlePause(ev *event) {
	g := s.getGroup(ev.resourceGroup)
	if g == nil {
		ev.pauseResult <- fmt.Errorf("%w: %s", ErrUnknownResourceGroup, ev.resourceGroup)
		return
	}
	s.withGroup(g, func() {
		s.handlePauseCurrent(ev)
	})
}

func (s *Server) handlePauseCurrent(ev *event) {
	phase := domain.StepPhase(s.stepPhase.Load())
	if phase != domain.StepServing {
		ev.pauseResult <- fmt.Errorf(
			"scheduler: cannot pause in phase %s (step_id=%d), must be SERVING",
			phase, s.stepID.Load())
		return
	}

	if s.paused {
		s.logger.Info("scheduler already paused (idempotent hit)",
			logger.Event(logger.EventPDPause),
			zap.Int64("step_id", s.stepID.Load()))
		ev.pauseResult <- nil
		return
	}

	s.paused = true
	s.pausedAtomic.Store(true)

	rejectedNormal := len(s.waitingQueue)
	rejectedPD := len(s.pdWaitingQueue)
	s.rejectWaitingQueue(ErrPaused)
	s.rejectPDWaitingQueue(ErrPaused)

	s.logger.Info("scheduler allocation paused",
		logger.Event(logger.EventPDPause),
		zap.Int64("step_id", s.stepID.Load()),
		zap.Int("rejected_queued_normal", rejectedNormal),
		zap.Int("rejected_queued_pd", rejectedPD))
	metrics.ControlPlaneOpsTotal.WithLabelValues("pause", "ok").Inc()
	ev.pauseResult <- nil
}

// handleContinue resumes allocation after a pause.
// Called only from the event-loop (single-threaded).
func (s *Server) handleContinue(ev *event) {
	g := s.getGroup(ev.resourceGroup)
	if g == nil {
		ev.pauseResult <- fmt.Errorf("%w: %s", ErrUnknownResourceGroup, ev.resourceGroup)
		return
	}
	s.withGroup(g, func() {
		s.handleContinueCurrent(ev)
	})
}

func (s *Server) handleContinueCurrent(ev *event) {
	phase := domain.StepPhase(s.stepPhase.Load())
	if phase != domain.StepServing {
		ev.pauseResult <- fmt.Errorf(
			"scheduler: cannot continue in phase %s (step_id=%d), must be SERVING",
			phase, s.stepID.Load())
		return
	}

	if !s.paused {
		s.logger.Info("scheduler already running (idempotent hit)",
			logger.Event(logger.EventPDContinue),
			zap.Int64("step_id", s.stepID.Load()))
		ev.pauseResult <- nil
		return
	}

	s.paused = false
	s.pausedAtomic.Store(false)

	s.logger.Info("scheduler allocation resumed",
		logger.Event(logger.EventPDContinue),
		zap.Int64("step_id", s.stepID.Load()))
	metrics.ControlPlaneOpsTotal.WithLabelValues("continue", "ok").Inc()
	ev.pauseResult <- nil
}

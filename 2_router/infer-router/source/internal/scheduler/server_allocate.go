package scheduler

// server_allocate.go contains the allocation pipeline: batchAllocate orchestrator,
// its 7 helper methods, recordAllocation, enrichPolicyError, and handleRelease.
// All methods are called exclusively from the event-loop (single-threaded).

import (
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/circuitbreaker"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

func (s *Server) batchAllocate(allocs []*event) {
	groups := make(map[string][]*event, 1)
	groupOrder := make([]string, 0, 1)
	for _, ev := range allocs {
		group := resourceGroupFromRoute(ev.route)
		if _, ok := groups[group]; !ok {
			groupOrder = append(groupOrder, group)
		}
		groups[group] = append(groups[group], ev)
	}
	for _, group := range groupOrder {
		groupAllocs := groups[group]
		if !s.hasResourceGroupRuntimeOrInstances(group) {
			s.rejectUnknownResourceGroup(group, groupAllocs)
			continue
		}
		g := s.getOrCreateGroup(group)
		s.withGroup(g, func() {
			s.batchAllocateCurrent(groupAllocs)
		})
	}
}

func (s *Server) rejectUnknownResourceGroup(group string, allocs []*event) {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	err := fmt.Errorf("%w: %s", ErrUnknownResourceGroup, group)
	for _, ev := range allocs {
		ev.resultCh <- allocResult{err: err}
	}
	s.accessLog.Warn("batch allocate rejected: unknown resource group",
		logger.Event(logger.EventAllocate),
		logger.Status(logger.StatusFail),
		zap.String("resource_group", group),
		zap.Int("batch_size", len(allocs)))
}

func (s *Server) batchAllocateCurrent(allocs []*event) {
	s.recordQueueWaitMetrics(allocs)

	if !s.checkServingPhase(allocs) {
		return
	}
	if s.checkPaused(allocs) {
		return
	}
	allocs = s.applyGlobalRateLimit(allocs)
	if len(allocs) == 0 {
		return
	}
	allocs = s.applyResourceGroupRateLimit(allocs)
	if len(allocs) == 0 {
		return
	}
	if s.enqueueIfWaitingQueueNonEmpty(allocs) {
		return
	}
	pending := s.separateDedupHits(allocs)
	if len(pending) == 0 {
		return
	}
	s.selectAndAssign(allocs, pending)
}

// checkPaused rejects the entire batch with ErrPaused if scheduler is paused.
// Returns true if rejected (paused), false otherwise.
func (s *Server) checkPaused(allocs []*event) bool {
	if !s.paused {
		return false
	}
	s.accessLog.Warn("batch allocate rejected: paused",
		logger.Event(logger.EventAllocate),
		logger.Status(logger.StatusFail),
		zap.Int64("step_id", s.stepID.Load()),
		zap.Int("batch_size", len(allocs)))
	for i := range allocs {
		allocs[i].resultCh <- allocResult{err: ErrPaused}
	}
	return true
}

// recordQueueWaitMetrics emits AllocQueueWaitMs for each event in the batch.
func (s *Server) recordQueueWaitMetrics(allocs []*event) {
	now := time.Now()
	for i := range allocs {
		if !allocs[i].enqueueTime.IsZero() {
			metrics.AllocQueueWaitMs.Observe(float64(now.Sub(allocs[i].enqueueTime).Microseconds()) / 1000.0)
		}
	}
}

// checkServingPhase rejects the entire batch if not in SERVING phase.
// Returns true if serving, false if rejected.
func (s *Server) checkServingPhase(allocs []*event) bool {
	phase := domain.StepPhase(s.stepPhase.Load())
	if phase == domain.StepServing {
		return true
	}
	stepID := s.stepID.Load()
	notServingErr := fmt.Errorf("[Allocate] Not Serving: scheduler phase is %s (step_id=%d).\n"+
		"  Impact: All allocation requests rejected.\n"+
		"  Action: Call POST /api/v2/start_infer to transition to SERVING phase.",
		phase, stepID)
	s.accessLog.Warn("batch allocate rejected",
		logger.Event(logger.EventAllocate),
		logger.Status(logger.StatusFail),
		logger.Reason(logger.ReasonSchedulerNotServing),
		zap.String("phase", phase.String()),
		zap.Int64("step_id", stepID),
		zap.Int("batch_size", len(allocs)))
	for i := range allocs {
		allocs[i].resultCh <- allocResult{err: notServingErr}
	}
	return false
}

// applyGlobalRateLimit applies the global_max_inflight cap.
// Full rejection: returns nil slice.
// Partial admission: returns the admitted prefix, rejects the rest.
// No limit or all admitted: returns allocs unchanged.
func (s *Server) applyGlobalRateLimit(allocs []*event) []*event {
	if s.globalMaxInflight <= 0 {
		return allocs
	}
	totalActive := s.totalActiveCount()
	headroom := s.globalMaxInflight - totalActive
	if headroom <= 0 {
		s.rejectBatchRateLimit(allocs, 0)
		return nil
	}
	if headroom >= int64(len(allocs)) {
		return allocs
	}
	// Partial: first headroom events pass through, rest rejected.
	s.rejectBatchRateLimit(allocs[headroom:], headroom)
	return allocs[:headroom]
}

// rejectBatchRateLimit sends rate-limit errors to each event and logs.
// admitted is the number of events that were allowed (for log context).
func (s *Server) rejectBatchRateLimit(rejected []*event, admitted int64) {
	stepID := s.stepID.Load()
	total := admitted + int64(len(rejected))
	rateLimitErr := fmt.Errorf("%w\n"+
		"  [Allocate] Rate Limited: in-flight %d/%d (global_max_inflight), step #%d.\n"+
		"  Impact: %d/%d requests admitted, %d rejected.\n"+
		"  Action: Reduce request concurrency, or raise 'global_max_inflight' in router config.",
		ErrGlobalRateLimitExceeded, s.totalActiveCount(), s.globalMaxInflight, stepID,
		admitted, total, int64(len(rejected)))
	for i := range rejected {
		rejected[i].resultCh <- allocResult{err: rateLimitErr}
	}
	metrics.GlobalRateLimitRejects.Add(float64(len(rejected)))
	s.accessLog.Warn("batch allocate: global rate limit",
		logger.Event(logger.EventAllocate),
		zap.Int64("admitted", admitted),
		zap.Int64("rejected", int64(len(rejected))),
		zap.Int64("active_count", s.activeCount),
		zap.Int64("pd_active_count", s.pdActiveCount),
		zap.Int64("global_max_inflight", s.globalMaxInflight))
}

func (s *Server) applyResourceGroupRateLimit(allocs []*event) []*event {
	if s.activeGroup == nil || s.activeGroup.maxInflight <= 0 {
		return allocs
	}
	active := s.currentGroupActiveCount()
	headroom := s.activeGroup.maxInflight - active
	if headroom <= 0 {
		s.rejectBatchResourceGroupRateLimit(allocs, 0)
		return nil
	}
	if headroom >= int64(len(allocs)) {
		return allocs
	}
	admitted := int(headroom)
	s.rejectBatchResourceGroupRateLimit(allocs[admitted:], headroom)
	return allocs[:admitted]
}

func (s *Server) rejectBatchResourceGroupRateLimit(rejected []*event, admitted int64) {
	group := s.currentResourceGroup()
	stepID := s.stepID.Load()
	total := admitted + int64(len(rejected))
	limitErr := fmt.Errorf("%w\n"+
		"  [Allocate] Resource Group Rate Limited: resource_group=%s in-flight %d/%d, step #%d.\n"+
		"  Impact: %d/%d requests admitted, %d rejected.\n"+
		"  Action: Reduce per-resource-group concurrency, or raise max_inflight in start config.",
		ErrResourceGroupRateLimitExceeded, group, s.currentGroupActiveCount(), s.activeGroup.maxInflight, stepID,
		admitted, total, int64(len(rejected)))
	for i := range rejected {
		rejected[i].resultCh <- allocResult{err: limitErr}
	}
	s.accessLog.Warn("batch allocate: resource group rate limit",
		logger.Event(logger.EventAllocate),
		zap.String("resource_group", group),
		zap.Int64("admitted", admitted),
		zap.Int64("rejected", int64(len(rejected))),
		zap.Int64("active_count", s.activeCount),
		zap.Int64("pd_active_count", s.pdActiveCount),
		zap.Int64("max_inflight", s.activeGroup.maxInflight))
}

// enqueueIfWaitingQueueNonEmpty redirects all events to the waiting queue tail
// when the queue is enabled and non-empty, preserving FIFO ordering.
// Returns true if events were enqueued (caller should return), false otherwise.
func (s *Server) enqueueIfWaitingQueueNonEmpty(allocs []*event) bool {
	if !s.queueEnabled || len(s.waitingQueue) == 0 {
		return false
	}
	for i := range allocs {
		// Dedup check first.
		if reqID := allocs[i].route.RequestID; reqID != "" {
			if cached, ok := s.allocDedup[reqID]; ok {
				metrics.AllocDedupHits.Inc()
				allocs[i].resultCh <- allocResult{
					inst:         cached.inst,
					allocationID: cached.allocationID,
				}
				continue
			}
		}
		// Skip cancelled callers.
		select {
		case <-allocs[i].ctx.Done():
			allocs[i].resultCh <- allocResult{err: allocs[i].ctx.Err()}
			metrics.AllocCallerGoneSkips.Inc()
			s.accessLog.Debug("allocate: caller cancelled before enqueue",
				zap.String("request_id", allocs[i].route.RequestID),
				zap.String("gateway_id", allocs[i].route.GatewayID),
				zap.Error(allocs[i].ctx.Err()))
			continue
		default:
		}
		if !s.enqueueToWaitingQueue(allocs[i]) {
			allocs[i].resultCh <- allocResult{err: ErrWaitingQueueFull}
		}
	}
	return true
}

// separateDedupHits filters allocs into dedup cache hits (resolved immediately)
// and new requests (returned as indices into allocs). Also skips cancelled callers.
// Uses s.pendingBuf as scratch space.
func (s *Server) separateDedupHits(allocs []*event) []int {
	s.pendingBuf = s.pendingBuf[:0]
	for i := range allocs {
		reqID := allocs[i].route.RequestID
		if reqID != "" {
			if cached, ok := s.allocDedup[reqID]; ok {
				metrics.AllocDedupHits.Inc()
				s.accessLog.Debug("allocate dedup hit",
					zap.String("request_id", reqID),
					zap.String("allocation_id", cached.allocationID))
				allocs[i].resultCh <- allocResult{
					inst:         cached.inst,
					allocationID: cached.allocationID,
				}
				continue
			}
		}
		// Skip if caller already gave up — avoids orphan allocation that nobody will Release.
		select {
		case <-allocs[i].ctx.Done():
			allocs[i].resultCh <- allocResult{err: allocs[i].ctx.Err()}
			metrics.AllocCallerGoneSkips.Inc()
			s.accessLog.Debug("allocate: caller cancelled before selection",
				zap.String("request_id", allocs[i].route.RequestID),
				zap.String("gateway_id", allocs[i].route.GatewayID),
				zap.Error(allocs[i].ctx.Err()))
			continue
		default:
		}
		s.pendingBuf = append(s.pendingBuf, i)
	}
	return s.pendingBuf
}

// selectAndAssign runs policy selection for pending events and records allocations.
// pending contains indices into allocs that need selection.
func (s *Server) selectAndAssign(allocs []*event, pending []int) {
	nodes := s.currentNodes()
	stepID := s.stepID.Load()

	// Notify generation-aware policies of node-set changes so caches are rebuilt.
	s.preparePolicyForNodes(nodes)

	pendingCount := len(pending)
	candidateCount := len(nodes)
	var failCount int

	// Lazy error enrichment — at most one fmt.Errorf per sentinel type per batch.
	var enrichedNoInst, enrichedOverload, enrichedSession error
	enrichOnce := func(err error) error {
		switch {
		case errors.Is(err, policy.ErrNoInstances):
			if enrichedNoInst == nil {
				enrichedNoInst = s.enrichPolicyError(err, candidateCount, nodes)
			}
			return enrichedNoInst
		case errors.Is(err, policy.ErrAllOverloaded):
			if enrichedOverload == nil {
				enrichedOverload = s.enrichPolicyError(err, candidateCount, nodes)
			}
			return enrichedOverload
		case errors.Is(err, policy.ErrAllExceedSession):
			if enrichedSession == nil {
				enrichedSession = s.enrichPolicyError(err, candidateCount, nodes)
			}
			return enrichedSession
		default:
			return err
		}
	}

	policyStart := time.Now()

	if s.batchSelector != nil {
		s.reqsBuf = s.reqsBuf[:0]
		for _, idx := range pending {
			s.reqsBuf = append(s.reqsBuf, allocs[idx].route)
		}
		results := s.batchSelector.BatchSelect(s.reqsBuf, nodes)
		metrics.PolicySelectDurationMs.Observe(float64(time.Since(policyStart).Microseconds()) / 1000.0)
		for j, res := range results {
			idx := pending[j]
			if res.Err != nil {
				// Queue-eligible error → enqueue instead of rejecting.
				if s.queueEnabled && (errors.Is(res.Err, policy.ErrAllOverloaded) || errors.Is(res.Err, policy.ErrAllExceedSession)) {
					if !s.enqueueToWaitingQueue(allocs[idx]) {
						allocs[idx].resultCh <- allocResult{err: ErrWaitingQueueFull}
					} else {
						s.logEnqueueReason(res.Err, allocs[idx].route)
					}
					continue
				}
				failCount++
				allocs[idx].resultCh <- allocResult{err: enrichOnce(res.Err)}
				continue
			}
			// Re-check caller liveness to shrink the race window.
			select {
			case <-allocs[idx].ctx.Done():
				allocs[idx].resultCh <- allocResult{err: allocs[idx].ctx.Err()}
				metrics.AllocCallerGoneSkips.Inc()
				s.accessLog.Debug("allocate: caller cancelled after batch selection",
					zap.String("request_id", allocs[idx].route.RequestID),
					zap.String("gateway_id", allocs[idx].route.GatewayID),
					zap.Error(allocs[idx].ctx.Err()))
				continue
			default:
			}
			s.recordAllocation(allocs[idx], res.Instance, stepID)
		}
	} else {
		// Fallback: sequential Select for policies without BatchSelector.
		for _, idx := range pending {
			ev := allocs[idx]
			inst, err := s.policy.Select(ev.ctx, ev.route, nodes)
			if err != nil {
				// Queue-eligible error → enqueue instead of rejecting.
				if s.queueEnabled && (errors.Is(err, policy.ErrAllOverloaded) || errors.Is(err, policy.ErrAllExceedSession)) {
					if !s.enqueueToWaitingQueue(ev) {
						ev.resultCh <- allocResult{err: ErrWaitingQueueFull}
					} else {
						s.logEnqueueReason(err, ev.route)
					}
					continue
				}
				failCount++
				ev.resultCh <- allocResult{err: enrichOnce(err)}
				continue
			}
			// Re-check caller liveness to shrink the race window.
			select {
			case <-ev.ctx.Done():
				ev.resultCh <- allocResult{err: ev.ctx.Err()}
				metrics.AllocCallerGoneSkips.Inc()
				s.accessLog.Debug("allocate: caller cancelled after sequential selection",
					zap.String("request_id", ev.route.RequestID),
					zap.String("gateway_id", ev.route.GatewayID),
					zap.Error(ev.ctx.Err()))
				continue
			default:
			}
			s.recordAllocation(ev, inst, stepID)
		}
		metrics.PolicySelectDurationMs.Observe(float64(time.Since(policyStart).Microseconds()) / 1000.0)
	}

	if failCount > 0 {
		s.accessLog.Warn("allocation failures in batch",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonAllOverloaded),
			zap.Int("failed", failCount),
			zap.Int("total", pendingCount),
			zap.Int("candidates", candidateCount))
	}
}

// enrichPolicyError wraps a bare policy sentinel error with human-readable
// operational context following the pattern:
//
//	[Allocate] <headline>.\n  <data>\n  <impact>\n  <fix>
//
// Called only on the error path (not per-request), so fmt.Errorf cost is acceptable.
// The wrapped error preserves errors.Is() compatibility with the original sentinel.
func (s *Server) enrichPolicyError(
	err error,
	candidateCount int,
	nodes map[string]*domain.NodeState,
) error {
	stepID := s.stepID.Load()

	// Count healthy instances (LoadAvailable = circuit breaker is open).
	var healthyCount int
	for _, ns := range nodes {
		if ns.LoadAvailable() {
			healthyCount++
		}
	}

	// Extract config limits from the policy.
	var maxRequestLoad, maxSessionLoad int64
	if cd, ok := s.policy.(policy.ConfigDescriber); ok {
		info := cd.ConfigSummary()
		maxRequestLoad = info.MaxRequestLoad
		maxSessionLoad = info.MaxSessionLoad
	}

	switch {
	case errors.Is(err, policy.ErrNoInstances):
		return fmt.Errorf("%w\n"+
			"  [Allocate] No Instances: 0 healthy instances (%d/%d registered), step #%d.\n"+
			"  Impact: All requests fail until instances are added.\n"+
			"  Action: Register backend instances via PUT /api/v2/instances, then retry.",
			err, healthyCount, candidateCount, stepID)

	case errors.Is(err, policy.ErrAllOverloaded):
		return fmt.Errorf("%w\n"+
			"  [Allocate] Capacity Exhausted: All healthy instances (%d/%d) reached max_request_load (%d), step #%d.\n"+
			"  Impact: New requests blocked until in-flight requests complete.\n"+
			"  Action: Wait for completion, scale out instances, or increase 'max_request_load' in start_infer config.",
			err, healthyCount, candidateCount, maxRequestLoad, stepID)

	case errors.Is(err, policy.ErrAllExceedSession):
		return fmt.Errorf("%w\n"+
			"  [Allocate] Session Exhausted: All healthy instances (%d/%d) reached max_session_load (%d), step #%d.\n"+
			"  Impact: New sessions blocked until existing sessions finish.\n"+
			"  Action: Wait for sessions to complete, scale out instances, or increase 'max_session_load' in start_infer config.",
			err, healthyCount, candidateCount, maxSessionLoad, stepID)

	default:
		return err
	}
}

// logEnqueueReason logs the reason a request was enqueued to the waiting queue.
// Distinguishes session exhaustion from load exhaustion to aid debugging.
// Called only from the event-loop (single-threaded).
func (s *Server) logEnqueueReason(err error, route *domain.RouteContext) {
	reason := "overloaded"
	if errors.Is(err, policy.ErrAllExceedSession) {
		reason = "session_exhausted"
	}
	s.accessLog.Debug("enqueue reason",
		logger.Event(logger.EventQueueEnqueue),
		zap.String("reason", reason),
		zap.String("request_id", route.RequestID),
		zap.String("session_id", route.SessionID))
}

// recordAllocation performs the post-selection bookkeeping for a successful allocation:
// state acquire, counter increment, dedup cache, gateway tracking, and result delivery.
// Called only from the event-loop (single-threaded, no lock needed).
func (s *Server) recordAllocation(ev *event, inst *domain.Instance, stepID int64) {
	s.state.Acquire(inst.ID)
	s.activeCount++
	s.activeCountAtomic.Store(s.activeCount)
	allocID := s.nextAllocationID(stepID)
	gatewayAddr := ev.route.GatewayID
	if gatewayAddr != "" {
		s.trackGatewayAllocation(gatewayAddr, gatewayAllocation{
			instanceID:    inst.ID,
			allocationID:  allocID,
			resourceGroup: s.currentResourceGroup(),
			kind:          allocationKindNormal,
		})
	}
	if reqID := ev.route.RequestID; reqID != "" {
		s.allocDedup[reqID] = allocCacheEntry{
			inst:         inst,
			allocationID: allocID,
		}
	}
	if s.driftTracker != nil {
		s.driftTracker.TrackAcquire(inst.ID)
	}
	if ce := s.accessLog.Check(zap.DebugLevel, "allocation recorded"); ce != nil {
		ce.Write(
			logger.Event(logger.EventAllocate),
			zap.String("resource_group", s.currentResourceGroup()),
			zap.String("request_id", ev.route.RequestID),
			zap.String("session_id", ev.route.SessionID),
			zap.String("instance", inst.ID),
			zap.String("allocation_id", allocID),
			zap.String("gateway_addr", gatewayAddr),
			zap.Bool("gateway_tracked", gatewayAddr != ""),
			zap.Int64("active_count", s.activeCount),
			zap.Int64("total_active_count", s.totalActiveCount()),
		)
	}
	ev.resultCh <- allocResult{inst: inst, allocationID: allocID, newAllocation: true}
}

func (s *Server) handleRelease(ev *event) {
	group := s.resourceGroupForRelease(ev)
	g := s.getOrCreateGroup(group)
	s.withGroup(g, func() {
		s.handleReleaseCurrent(ev)
	})
	s.pruneEmptyResourceGroups()
}

func (s *Server) handleReleaseCurrent(ev *event) {
	resourceGroup := s.currentResourceGroup()
	wasTracked := s.hasTrackedGatewayAllocation(ev.gatewayAddr, ev.allocationID)
	// Idempotent Release: skip if this allocationID was already released.
	// Empty allocationID cannot be deduplicated (no meaningful key), always process normally.
	if ev.allocationID != "" {
		if _, ok := s.releaseDedup[ev.allocationID]; ok {
			metrics.ReleaseDedupHits.Inc()
			if ce := s.accessLog.Check(zap.DebugLevel, "release dedup hit, skipping duplicate"); ce != nil {
				ce.Write(
					zap.String("resource_group", resourceGroup),
					zap.String("allocation_id", ev.allocationID),
					zap.String("instance", ev.instanceID),
					zap.String("gateway_addr", ev.gatewayAddr),
					zap.Bool("gateway_tracked", wasTracked),
					zap.Int64("active_count", s.activeCount),
					zap.Int64("total_active_count", s.totalActiveCount()),
				)
			}
			return
		}
		s.releaseDedup[ev.allocationID] = struct{}{}
	}

	s.state.Release(ev.instanceID)
	s.policy.Feedback(ev.instanceID, ev.costMetrics)
	if s.driftTracker != nil {
		s.driftTracker.TrackRelease(ev.instanceID)
	}

	// Circuit breaker: record outcome from request error code.
	if s.cbManager != nil && ev.costMetrics != nil {
		errorCode := ev.costMetrics.ErrorCode
		now := time.Now()
		oldState, newState, changed := s.cbManager.RecordOutcome(ev.instanceID, errorCode, now)
		if changed {
			if ns, ok := s.state.GetNodes()[ev.instanceID]; ok {
				ns.StoreCircuitOpen(newState == circuitbreaker.Open)
			}
			s.logCircuitTransition(ev.instanceID, errorCode, oldState, newState)
		}
	}

	if s.activeCount > 0 {
		s.activeCount--
		s.activeCountAtomic.Store(s.activeCount)
	}

	// Signal to auditDrainHealth that capacity was freed this batch.
	s.lastDrainHadCapacityFree = true

	// Remove from per-gateway tracking.
	if ev.gatewayAddr != "" {
		s.untrackGatewayAllocation(ev.gatewayAddr, ev.allocationID, ev.instanceID, allocationKindNormal)
	}
	if ev.gatewayAddr != "" && ev.allocationID != "" && !wasTracked {
		if ce := s.accessLog.Check(zap.DebugLevel, "release had no gateway tracking entry"); ce != nil {
			ce.Write(
				logger.Event(logger.EventRelease),
				zap.String("resource_group", resourceGroup),
				zap.String("instance", ev.instanceID),
				zap.String("allocation_id", ev.allocationID),
				zap.String("gateway_addr", ev.gatewayAddr),
			)
		}
	}

	if ce := s.accessLog.Check(zap.DebugLevel, "released"); ce != nil {
		ce.Write(
			logger.Event(logger.EventRelease),
			zap.String("resource_group", resourceGroup),
			zap.String("instance", ev.instanceID),
			zap.String("allocation_id", ev.allocationID),
			zap.String("gateway_addr", ev.gatewayAddr),
			zap.Bool("gateway_tracked", wasTracked),
			zap.Int64("active_count", s.activeCount),
			zap.Int64("total_active_count", s.totalActiveCount()),
		)
	}

	// Auto-transition DRAINING → IDLE when all requests are done.
	s.finishDrainingIfIdle("release")
}

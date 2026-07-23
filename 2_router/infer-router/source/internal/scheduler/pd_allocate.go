package scheduler

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

// handleAllocatePD selects a prefill + decode instance pair via the event-loop.
// It reads NodeState.ActiveRequests for global load balancing instead of local CounterManager.
// Called only from the event-loop (single-threaded, no lock needed).
func (s *Server) handleAllocatePD(ev *event) {
	group := resourceGroupFromRoute(ev.route)
	if !s.hasResourceGroupRuntimeOrInstances(group) {
		ev.pdResultCh <- pdAllocResult{err: fmt.Errorf("%w: %s", ErrUnknownResourceGroup, group)}
		return
	}
	g := s.getOrCreateGroup(group)
	s.withGroup(g, func() {
		s.handleAllocatePDCurrent(ev)
	})
}

func (s *Server) handleAllocatePDCurrent(ev *event) {
	now := time.Now()
	if !ev.enqueueTime.IsZero() {
		metrics.AllocQueueWaitMs.Observe(float64(now.Sub(ev.enqueueTime).Microseconds()) / 1000.0)
	}

	// Phase guard: only allocate during SERVING.
	phase := domain.StepPhase(s.stepPhase.Load())
	if phase != domain.StepServing {
		stepID := s.stepID.Load()
		ev.pdResultCh <- pdAllocResult{
			err: fmt.Errorf("scheduler: cannot allocate PD in phase %s (step_id=%d), only SERVING phase accepts requests",
				phase, stepID),
		}
		return
	}

	// Pause guard: reject immediately if allocation is paused.
	if s.paused {
		ev.pdResultCh <- pdAllocResult{err: ErrPaused}
		return
	}

	// Dedup: same request_id returns cached result (checked before rate limit, same as batchAllocate).
	reqID := ev.route.RequestID
	if reqID != "" {
		if cached, ok := s.pdAllocDedup[reqID]; ok {
			cached.claimed = true
			s.pdAllocDedup[reqID] = cached
			metrics.AllocDedupHits.Inc()
			s.accessLog.Debug("pd allocate dedup hit",
				zap.String("request_id", reqID))
			ev.pdResultCh <- pdAllocResult{
				prefill:        cached.prefill,
				decode:         cached.decode,
				prefillAllocID: cached.prefillAllocID,
				decodeAllocID:  cached.decodeAllocID,
			}
			return
		}
	}

	// Skip if caller already gave up.
	select {
	case <-ev.ctx.Done():
		ev.pdResultCh <- pdAllocResult{err: ev.ctx.Err()}
		metrics.AllocCallerGoneSkips.Inc()
		return
	default:
	}

	// FIFO preservation: if the PD waiting queue is non-empty, enqueue so that
	// earlier requests are not bypassed by later arrivals.
	if s.queueEnabled && len(s.pdWaitingQueue) > 0 {
		s.enqueuePDOrReject(ev)
		return
	}

	// Global rate limit — consider both normal and PD in-flight.
	if s.globalMaxInflight > 0 {
		totalActive := s.activeCount + s.pdActiveCount
		if totalActive+2 > s.globalMaxInflight { // +2 for prefill + decode
			if s.queueEnabled {
				s.enqueuePDOrReject(ev)
				return
			}
			ev.pdResultCh <- pdAllocResult{
				err: fmt.Errorf("%w (active=%d, pd_active=%d, limit=%d)",
					ErrGlobalRateLimitExceeded, s.activeCount, s.pdActiveCount, s.globalMaxInflight),
			}
			metrics.GlobalRateLimitRejects.Add(1)
			return
		}
	}
	if s.activeGroup != nil && s.activeGroup.maxInflight > 0 && s.currentGroupActiveCount()+2 > s.activeGroup.maxInflight {
		if s.queueEnabled {
			s.enqueuePDOrReject(ev)
			return
		}
		ev.pdResultCh <- pdAllocResult{
			err: fmt.Errorf("%w (resource_group=%s, active=%d, limit=%d)",
				ErrResourceGroupRateLimitExceeded, s.currentResourceGroup(), s.currentGroupActiveCount(), s.activeGroup.maxInflight),
		}
		return
	}

	// Get nodes by role.
	prefillNodes := s.currentNodesByRole("prefill")
	decodeNodes := s.currentNodesByRole("decode")

	if len(prefillNodes) == 0 || len(decodeNodes) == 0 {
		ev.pdResultCh <- pdAllocResult{
			err: fmt.Errorf("no available PD workers: prefill=%d, decode=%d",
				len(prefillNodes), len(decodeNodes)),
		}
		return
	}

	// Validate PD policies are configured.
	if s.pdPrefillPolicy == nil {
		ev.pdResultCh <- pdAllocResult{
			err: fmt.Errorf("pd_prefill_policy not configured, call StartStep with PDPrefillPolicy first"),
		}
		return
	}
	if s.pdDecodePolicy == nil {
		ev.pdResultCh <- pdAllocResult{
			err: fmt.Errorf("pd_decode_policy not configured, call StartStep with PDDecodePolicy first"),
		}
		return
	}

	// Select prefill instance via centralized policy.
	prefillInst, err := s.pdPrefillPolicy.Select(ev.ctx, ev.route, prefillNodes)
	if err != nil {
		s.logger.Warn("prefill policy select failed",
			zap.Error(err),
			zap.String("session_id", ev.route.SessionID),
			zap.String("request_id", ev.route.RequestID),
			zap.Bool("queue_enabled", s.queueEnabled))
		if (errors.Is(err, policy.ErrAllOverloaded) || errors.Is(err, policy.ErrNoInstances)) && s.queueEnabled {
			s.enqueuePDOrReject(ev)
			return
		}
		ev.pdResultCh <- pdAllocResult{err: fmt.Errorf("prefill policy select: %w", err)}
		return
	}

	// Select decode instance via centralized policy.
	// NOTE: state.Acquire is not called yet (that happens in recordPDAllocation), but
	// pdPrefillPolicy.Select has already incremented the policy's internal soft-counters
	// (counterMgr request/token counts, pendingTokens FIFO). If decode Select fails we
	// must call Feedback on the prefill instance to undo those increments, otherwise the
	// prefill policy will permanently over-count that instance's load.
	decodeInst, err := s.pdDecodePolicy.Select(ev.ctx, ev.route, decodeNodes)
	if err != nil {
		// Roll back prefill policy's internal soft-counters to avoid load-count leak.
		s.pdPrefillPolicy.Feedback(prefillInst.ID, nil)

		if (errors.Is(err, policy.ErrAllOverloaded) || errors.Is(err, policy.ErrNoInstances)) && s.queueEnabled {
			s.enqueuePDOrReject(ev)
			return
		}
		ev.pdResultCh <- pdAllocResult{err: fmt.Errorf("decode policy select: %w", err)}
		return
	}

	stepID := s.stepID.Load()
	s.recordPDAllocation(ev, prefillInst, decodeInst, stepID)
}

// recordPDAllocation acquires capacity on both instances, generates allocation IDs,
// updates gateway tracking and dedup cache, and sends the result to ev.pdResultCh.
// Called only from the event-loop (single-threaded).
func (s *Server) recordPDAllocation(ev *event, prefillInst, decodeInst *domain.Instance, stepID int64) {
	// Acquire: increment ActiveRequests on both instances.
	s.state.Acquire(prefillInst.ID)
	s.state.Acquire(decodeInst.ID)
	s.pdActiveCount += 2
	s.pdActiveAtomic.Store(s.pdActiveCount)

	// Generate allocation IDs.
	prefillAllocID := s.nextAllocationID(stepID)
	decodeAllocID := s.nextAllocationID(stepID)

	// Gateway tracking.
	reqID := ev.route.RequestID
	gatewayAddr := ev.route.GatewayID
	if gatewayAddr != "" {
		s.trackGatewayAllocation(gatewayAddr, gatewayAllocation{
			instanceID:    prefillInst.ID,
			allocationID:  prefillAllocID,
			resourceGroup: s.currentResourceGroup(),
			kind:          allocationKindPrefill,
		})
		s.trackGatewayAllocation(gatewayAddr, gatewayAllocation{
			instanceID:    decodeInst.ID,
			allocationID:  decodeAllocID,
			resourceGroup: s.currentResourceGroup(),
			kind:          allocationKindDecode,
		})
	}

	// Dedup cache.
	if reqID != "" {
		s.pdAllocDedup[reqID] = pdAllocCacheEntry{
			prefill:        prefillInst,
			decode:         decodeInst,
			prefillAllocID: prefillAllocID,
			decodeAllocID:  decodeAllocID,
		}
	}

	if ce := s.accessLog.Check(zap.DebugLevel, "pd allocated"); ce != nil {
		ce.Write(
			logger.Event(logger.EventAllocate),
			zap.String("resource_group", s.currentResourceGroup()),
			zap.String("request_id", reqID),
			zap.String("session_id", ev.route.SessionID),
			zap.String("prefill", prefillInst.ID),
			zap.String("decode", decodeInst.ID),
			zap.String("prefill_alloc_id", prefillAllocID),
			zap.String("decode_alloc_id", decodeAllocID),
			zap.String("gateway_addr", gatewayAddr),
			zap.Bool("gateway_tracked", gatewayAddr != ""),
			zap.Int64("pd_active_count", s.pdActiveCount),
			zap.Int64("total_active_count", s.totalActiveCount()),
		)
	}

	ev.pdResultCh <- pdAllocResult{
		prefill:        prefillInst,
		decode:         decodeInst,
		prefillAllocID: prefillAllocID,
		decodeAllocID:  decodeAllocID,
		newAllocation:  true,
	}
}

// handleReleasePD releases a prefill or decode allocation slot.
// Called only from the event-loop (single-threaded).
func (s *Server) handleReleasePD(ev *event) {
	group := s.resourceGroupForRelease(ev)
	g := s.getOrCreateGroup(group)
	s.withGroup(g, func() {
		s.handleReleasePDCurrent(ev)
	})
	s.pruneEmptyResourceGroups()
}

func (s *Server) handleReleasePDCurrent(ev *event) {
	defer func() {
		if ev.costMetrics != nil {
			domain.ReleaseCostMetrics(ev.costMetrics)
			ev.costMetrics = nil
		}
	}()
	resourceGroup := s.currentResourceGroup()
	wasTracked := s.hasTrackedGatewayAllocation(ev.gatewayAddr, ev.allocationID)

	if ev.instanceID == "" || ev.allocationID == "" || (ev.releaseRole != "prefill" && ev.releaseRole != "decode") {
		s.accessLog.Warn("invalid pd release event ignored",
			zap.String("resource_group", resourceGroup),
			zap.String("instance", ev.instanceID),
			zap.String("allocation_id", ev.allocationID),
			zap.String("role", ev.releaseRole))
		return
	}

	// Dedup: same allocation_id is a no-op.
	if ev.allocationID != "" {
		if _, ok := s.pdReleaseDedup[ev.allocationID]; ok {
			s.deleteCompensatedPDAllocDedup(ev.compensateRequestID, ev.allocationID)
			metrics.ReleaseDedupHits.Inc()
			if ce := s.accessLog.Check(zap.DebugLevel, "pd release dedup hit"); ce != nil {
				ce.Write(
					zap.String("resource_group", resourceGroup),
					zap.String("allocation_id", ev.allocationID),
					zap.String("instance", ev.instanceID),
					zap.String("role", ev.releaseRole),
					zap.String("gateway_addr", ev.gatewayAddr),
					zap.Bool("gateway_tracked", wasTracked),
					zap.Int64("pd_active_count", s.pdActiveCount),
					zap.Int64("total_active_count", s.totalActiveCount()),
				)
			}
			return
		}
		s.pdReleaseDedup[ev.allocationID] = struct{}{}
	}

	// Release: decrement ActiveRequests.
	s.state.Release(ev.instanceID)

	// Feed metrics back for internal load tracking.
	switch ev.releaseRole {
	case "prefill":
		if s.pdPrefillPolicy != nil {
			s.pdPrefillPolicy.Feedback(ev.instanceID, ev.costMetrics)
		}
	case "decode":
		if s.pdDecodePolicy != nil {
			s.pdDecodePolicy.Feedback(ev.instanceID, ev.costMetrics)
		}
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

	if s.pdActiveCount > 0 {
		s.pdActiveCount--
		s.pdActiveAtomic.Store(s.pdActiveCount)
	}

	// Signal that capacity was freed so drainPDWaitingQueue gets a retry.
	s.lastDrainHadCapacityFree = true

	// Remove from per-gateway tracking.
	if ev.gatewayAddr != "" {
		s.untrackGatewayAllocation(ev.gatewayAddr, ev.allocationID, ev.instanceID, allocationKindFromRole(ev.releaseRole))
	}
	s.deleteCompensatedPDAllocDedup(ev.compensateRequestID, ev.allocationID)
	if ev.gatewayAddr != "" && ev.allocationID != "" && !wasTracked {
		if ce := s.accessLog.Check(zap.DebugLevel, "pd release had no gateway tracking entry"); ce != nil {
			ce.Write(
				logger.Event(logger.EventRelease),
				zap.String("resource_group", resourceGroup),
				zap.String("instance", ev.instanceID),
				zap.String("allocation_id", ev.allocationID),
				zap.String("role", ev.releaseRole),
				zap.String("gateway_addr", ev.gatewayAddr),
			)
		}
	}

	if ce := s.accessLog.Check(zap.DebugLevel, "pd released"); ce != nil {
		ce.Write(
			logger.Event(logger.EventRelease),
			zap.String("resource_group", resourceGroup),
			zap.String("instance", ev.instanceID),
			zap.String("allocation_id", ev.allocationID),
			zap.String("role", ev.releaseRole),
			zap.String("gateway_addr", ev.gatewayAddr),
			zap.Bool("gateway_tracked", wasTracked),
			zap.Int64("pd_active_count", s.pdActiveCount),
			zap.Int64("total_active_count", s.totalActiveCount()),
		)
	}

	// Auto-transition DRAINING → IDLE when all requests are done.
	s.finishDrainingIfIdle("pd release")
}

func (s *Server) handleCompensatePD(ev *event) {
	group := s.resourceGroupForGatewayAllocation(ev.gatewayAddr, ev.compensatePrefillAllocID)
	g := s.getOrCreateGroup(group)
	s.withGroup(g, func() {
		s.handleCompensatePDCurrent(ev)
	})
}

func (s *Server) handleCompensatePDCurrent(ev *event) {
	requestID := ev.compensateRequestID
	if requestID == "" || ev.compensatePrefillAllocID == "" || ev.compensateDecodeAllocID == "" {
		s.accessLog.Warn("invalid pd compensation event ignored",
			zap.String("request_id", requestID),
			zap.String("prefill_alloc_id", ev.compensatePrefillAllocID),
			zap.String("decode_alloc_id", ev.compensateDecodeAllocID))
		return
	}

	cached, ok := s.pdAllocDedup[requestID]
	if !ok {
		s.accessLog.Debug("pd compensation skipped, dedup entry already gone",
			zap.String("trace_id", ev.compensateAllocationTrace),
			zap.String("request_id", requestID))
		return
	}
	if cached.prefillAllocID != ev.compensatePrefillAllocID || cached.decodeAllocID != ev.compensateDecodeAllocID {
		s.accessLog.Debug("pd compensation skipped, dedup entry changed",
			zap.String("trace_id", ev.compensateAllocationTrace),
			zap.String("request_id", requestID),
			zap.String("prefill_alloc_id", ev.compensatePrefillAllocID),
			zap.String("decode_alloc_id", ev.compensateDecodeAllocID))
		return
	}
	if cached.claimed {
		s.accessLog.Debug("pd compensation skipped, allocation recovered by dedup retry",
			zap.String("trace_id", ev.compensateAllocationTrace),
			zap.String("request_id", requestID),
			zap.String("prefill_alloc_id", ev.compensatePrefillAllocID),
			zap.String("decode_alloc_id", ev.compensateDecodeAllocID))
		return
	}

	s.handleCompensatedPDRelease(ev.compensatePrefillID, ev.gatewayAddr, ev.compensatePrefillAllocID, "prefill", requestID)
	s.handleCompensatedPDRelease(ev.compensateDecodeID, ev.gatewayAddr, ev.compensateDecodeAllocID, "decode", requestID)
	metrics.PDAllocCompensationsTotal.Inc()
	s.accessLog.Warn("pd allocation abandoned by caller, compensated release completed",
		zap.String("trace_id", ev.compensateAllocationTrace),
		zap.String("request_id", requestID),
		zap.String("prefill", ev.compensatePrefillID),
		zap.String("decode", ev.compensateDecodeID),
		zap.String("prefill_alloc_id", ev.compensatePrefillAllocID),
		zap.String("decode_alloc_id", ev.compensateDecodeAllocID))
}

func (s *Server) handleCompensatedPDRelease(instanceID, gatewayAddr, allocationID, role, requestID string) {
	m := domain.AcquireCostMetrics()
	m.ErrorCode = pdAllocationCanceledCode
	s.handleReleasePDCurrent(&event{
		instanceID:          instanceID,
		gatewayAddr:         gatewayAddr,
		allocationID:        allocationID,
		releaseRole:         role,
		compensateRequestID: requestID,
		costMetrics:         m,
	})
}

func (s *Server) deleteCompensatedPDAllocDedup(requestID, allocationID string) {
	if requestID == "" || allocationID == "" {
		return
	}
	cached, ok := s.pdAllocDedup[requestID]
	if !ok {
		return
	}
	if cached.prefillAllocID != allocationID && cached.decodeAllocID != allocationID {
		return
	}
	if _, ok := s.pdReleaseDedup[cached.prefillAllocID]; !ok {
		return
	}
	if _, ok := s.pdReleaseDedup[cached.decodeAllocID]; !ok {
		return
	}
	delete(s.pdAllocDedup, requestID)
}

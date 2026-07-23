package scheduler

// server_step.go contains step lifecycle management: handleStartStep (with 3 helpers),
// handleEndStep, and handleCleanupGateway.
// All methods are called exclusively from the event-loop (single-threaded).

import (
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

func (s *Server) handleStartStep(ev *event) {
	group := ev.resourceGroup
	g := s.getOrCreateGroup(group)
	var result stepResult
	s.withGroup(g, func() {
		result = s.handleStartStepCurrent(ev)
	})
	ev.stepResult <- result
}

func (s *Server) handleStartStepCurrent(ev *event) stepResult {
	// Phase validation: must be IDLE (with idempotency for re-SERVING same step).
	phase := domain.StepPhase(s.stepPhase.Load())
	if phase != domain.StepIdle {
		if phase == domain.StepServing && ev.stepID == s.stepID.Load() {
			s.logger.Info("step already serving (idempotent hit)",
				logger.Event(logger.EventStepStart),
				zap.Int64("step_id", ev.stepID))
			return stepResult{}
		}
		return stepResult{
			err: fmt.Errorf("scheduler: cannot start step %d in phase %s (current_step=%d), must be IDLE",
				ev.stepID, phase, s.stepID.Load()),
		}
	}

	// Resolve policy: build new or reset existing.
	if err := s.resolveStepPolicy(ev.stepID, ev.policyName, ev.policyConfig); err != nil {
		return stepResult{err: err}
	}

	// Resolve PD separation policies (always built with defaults).
	if err := s.resolvePDPolicies(ev.stepID, ev.policyConfig); err != nil {
		return stepResult{err: err}
	}
	s.currentPolicyConfig = ev.policyConfig
	if s.activeGroup != nil {
		s.activeGroup.maxInflight = ev.policyConfig.MaxInflight
	}

	// Reset all per-step state and reject leftover queued requests.
	s.resetStepState(ev.stepID)

	// Apply waiting queue configuration from StartStep overrides + defaults.
	s.applyWaitingQueueConfig(ev.policyConfig)

	// Activate the step.
	s.stepID.Store(ev.stepID)
	s.stepPhase.Store(int32(domain.StepServing))
	metrics.StepPhaseGauge.Set(float64(domain.StepServing))
	metrics.StepIDGauge.Set(float64(ev.stepID))

	policyLog := ev.policyName
	if policyLog == "" {
		policyLog = "unchanged"
	}
	pdPrefillLog := "<none>"
	if s.pdPrefillPolicy != nil {
		pdPrefillLog = string(s.pdPrefillPolicy.Name())
	}
	pdDecodeLog := "<none>"
	if s.pdDecodePolicy != nil {
		pdDecodeLog = string(s.pdDecodePolicy.Name())
	}
	s.logger.Info("step started",
		logger.Event(logger.EventStepStart),
		zap.Int64("step_id", ev.stepID),
		zap.String("policy", policyLog),
		zap.String("pd_prefill_policy", pdPrefillLog),
		zap.String("pd_decode_policy", pdDecodeLog),
		zap.Bool("queue_enabled", s.queueEnabled),
		zap.Int64("queue_max_size", s.queueMaxSize),
		zap.Duration("queue_timeout", s.queueTimeout),
		zap.Int("candidates", len(s.currentNodes())))
	return stepResult{}
}

func (s *Server) mergePDPolicyConfig(override policy.PolicyConfig) policy.PolicyConfig {
	cfg := s.defaultPDPolicyConfig
	if override.CacheThreshold != 0 {
		cfg.CacheThreshold = override.CacheThreshold
	}
	if override.BalanceAbsThreshold != 0 {
		cfg.BalanceAbsThreshold = override.BalanceAbsThreshold
	}
	if override.BalanceRelThreshold != 0 {
		cfg.BalanceRelThreshold = override.BalanceRelThreshold
	}
	if override.CacheBlockSize != 0 {
		cfg.CacheBlockSize = override.CacheBlockSize
	}
	if override.HitRatioWeight != 0 {
		cfg.HitRatioWeight = override.HitRatioWeight
	}
	if override.LoadBalanceWeight != 0 {
		cfg.LoadBalanceWeight = override.LoadBalanceWeight
	}
	if override.MaxTreeSize != 0 {
		cfg.MaxTreeSize = override.MaxTreeSize
	}
	if override.EvictionIntervalSec != 0 {
		cfg.EvictionIntervalSec = override.EvictionIntervalSec
	}
	if override.MaxRequestLoad != 0 {
		cfg.MaxRequestLoad = override.MaxRequestLoad
	}
	if override.PDPrefillPolicy != "" {
		cfg.PDPrefillPolicy = override.PDPrefillPolicy
	}
	if override.PDDecodePolicy != "" {
		cfg.PDDecodePolicy = override.PDDecodePolicy
	}
	return cfg
}

// resolveStepPolicy builds a new policy from the requested name and config,
// or resets the current policy if no override is specified.
// Returns non-nil error only when policy.Build fails.
func (s *Server) resolveStepPolicy(stepID int64, policyName string, cfg policy.PolicyConfig) error {
	requestedPolicy := policy.Name(policyName)
	if requestedPolicy == "" {
		// No override: reset current policy if it supports Reset().
		if r, ok := s.policy.(policy.Resettable); ok {
			r.Reset()
		}
		return nil
	}

	// Explicit override: rebuild the policy.
	newPolicy, err := policy.Build(requestedPolicy, cfg)
	if err != nil {
		return fmt.Errorf("scheduler: cannot start step %d, invalid policy %q: %w",
			stepID, requestedPolicy, err)
	}
	s.policy = newPolicy
	s.policyName.Store(string(newPolicy.Name()))
	if bs, ok := newPolicy.(policy.BatchSelector); ok {
		s.batchSelector = bs
	} else {
		s.batchSelector = nil
	}
	if ga, ok := newPolicy.(policy.GenerationAware); ok {
		s.generationAware = ga
	} else {
		s.generationAware = nil
	}
	if dt, ok := newPolicy.(policy.DriftAware); ok {
		s.driftTracker = dt
	} else {
		s.driftTracker = nil
	}
	if ce, ok := newPolicy.(policy.CachedIDEnsurer); ok {
		s.cachedIDEnsurer = ce
	} else {
		s.cachedIDEnsurer = nil
	}
	return nil
}

// resolvePDPolicies builds PD (prefill/decode) separation policies.
// Only builds policies when PD-role instances are registered; otherwise clears them.
// Defaults when not specified:
//   - prefill default: pd_cache_aware (cache-aware token scheduling)
//   - decode  default: request_num (request-count scheduling)
func (s *Server) resolvePDPolicies(stepID int64, cfg policy.PolicyConfig) error {
	cfg = s.mergePDPolicyConfig(cfg)
	prefillNodes := s.currentNodesByRole("prefill")
	decodeNodes := s.currentNodesByRole("decode")
	if len(prefillNodes) == 0 && len(decodeNodes) == 0 {
		s.pdPrefillPolicy = nil
		s.pdDecodePolicy = nil
		return nil
	}

	var prefillPolicyName policy.Name
	if cfg.PDPrefillPolicy != "" {
		prefillPolicyName = policy.Name(cfg.PDPrefillPolicy)
	}
	if prefillPolicyName == "" {
		prefillPolicyName = policy.NamePDCacheAware // PD default
	}
	p, err := policy.Build(prefillPolicyName, cfg)
	if err != nil {
		return fmt.Errorf("scheduler: cannot start step %d, invalid pd_prefill_policy: %w", stepID, err)
	}
	s.pdPrefillPolicy = p

	var decodePolicyName policy.Name
	if cfg.PDDecodePolicy != "" {
		decodePolicyName = policy.Name(cfg.PDDecodePolicy)
	}
	if decodePolicyName == "" {
		decodePolicyName = policy.NameRequestNum // PD default
	}
	p, err = policy.Build(decodePolicyName, cfg)
	if err != nil {
		return fmt.Errorf("scheduler: cannot start step %d, invalid pd_decode_policy: %w", stepID, err)
	}
	s.pdDecodePolicy = p

	// Reset PD policies if they support it.
	if r, ok := s.pdPrefillPolicy.(policy.Resettable); ok {
		r.Reset()
	}
	if r, ok := s.pdDecodePolicy.(policy.Resettable); ok {
		r.Reset()
	}
	return nil
}

// resetStepState clears all per-step counters, dedup maps, allocation tracking,
// and rejects any leftover queued requests from the previous step.
func (s *Server) resetStepState(newStepID int64) {
	if s.activeGroup != nil {
		s.state.ResetResourceGroup(s.activeGroup.resourceGroup)
		s.removeGatewayAllocationsForResourceGroup(s.activeGroup.resourceGroup)
	} else {
		s.state.ResetAll()
		s.gatewayAllocs = make(map[string]map[string]gatewayAllocation)
	}
	if s.cbManager != nil {
		if s.activeGroup != nil {
			s.cbManager.ResetByInstances(instanceIDsFromNodes(s.currentNodes()))
		} else {
			s.cbManager.Reset()
		}
	}
	s.activeCount = 0
	s.activeCountAtomic.Store(0)
	s.lastEndedStepID = 0
	s.allocCounter = 0
	s.allocCounterAtomic.Store(0)
	clear(s.allocDedup)
	clear(s.releaseDedup)
	clear(s.pdAllocDedup)
	clear(s.pdReleaseDedup)
	s.pdActiveCount = 0
	s.pdActiveAtomic.Store(0)
	s.paused = false
	s.pausedAtomic.Store(false)
	s.stepDoneCh = make(chan struct{})
	s.rejectWaitingQueue(fmt.Errorf("step reset (new step %d)", newStepID))
	s.rejectPDWaitingQueue(fmt.Errorf("step reset (new step %d)", newStepID))

	// Reset all instance session metrics to zero.
	// Called after Policy.Reset() clears sessionLoad maps, so we need to
	// manually reset the Prometheus gauges to reflect the new state.
	for _, inst := range s.currentInstances() {
		metrics.InstanceSessions.WithLabelValues(inst.ID).Set(0)
	}
}

func instanceIDsFromNodes(nodes map[string]*domain.NodeState) []string {
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	return ids
}

// applyWaitingQueueConfig merges waiting queue config from StartStep overrides
// with startup defaults, clamping invalid values to safe minimums.
func (s *Server) applyWaitingQueueConfig(cfg policy.PolicyConfig) {
	s.queueEnabled = s.defaultQueueCfg.Enabled
	if cfg.WaitingQueueEnabled != nil {
		s.queueEnabled = *cfg.WaitingQueueEnabled
	}
	s.queueMaxSize = s.defaultQueueCfg.MaxSize
	if s.queueMaxSize <= 0 {
		s.queueMaxSize = 100000
	}
	if cfg.WaitingQueueMaxSize != nil {
		s.queueMaxSize = *cfg.WaitingQueueMaxSize
		if s.queueMaxSize <= 0 {
			s.queueMaxSize = 100000
		}
	}
	s.queueTimeout = s.defaultQueueCfg.Timeout.Duration
	if s.queueTimeout <= 0 {
		s.queueTimeout = 300 * time.Second
	}
	if cfg.WaitingQueueTimeoutSec != nil {
		s.queueTimeout = time.Duration(*cfg.WaitingQueueTimeoutSec) * time.Second
		if s.queueTimeout <= 0 {
			s.queueTimeout = 300 * time.Second
		}
	}
	// Sync atomic mirrors for external readers.
	s.queueEnabledAtomic.Store(s.queueEnabled)
	s.queueMaxSizeAtomic.Store(s.queueMaxSize)
	s.queueTimeoutAtomic.Store(int64(s.queueTimeout.Seconds()))
	s.drainCyclesWithoutRelease = 0
	s.lastDrainHadCapacityFree = false
}

func (s *Server) handleEndStep(ev *event) {
	g := s.getGroup(ev.resourceGroup)
	if g == nil {
		ev.stepResult <- stepResult{err: fmt.Errorf("%w: %s", ErrUnknownResourceGroup, ev.resourceGroup)}
		return
	}
	s.withGroup(g, func() {
		s.handleEndStepCurrent(ev)
	})
	s.pruneEmptyResourceGroups()
}

func (s *Server) handleEndStepCurrent(ev *event) {
	phase := domain.StepPhase(s.stepPhase.Load())
	resourceGroup := s.currentResourceGroup()
	if phase != domain.StepServing {
		// Idempotent: if this step was already ended, return success.
		if ev.stepID == s.lastEndedStepID && (phase == domain.StepDraining || phase == domain.StepIdle) {
			pendingRequests := s.currentGroupActiveCount()
			fields := []zap.Field{
				logger.Event(logger.EventStepEnd),
				zap.String("resource_group", resourceGroup),
				zap.Int64("step_id", ev.stepID),
				zap.String("phase", phase.String()),
				zap.Int64("pending_requests", pendingRequests),
				zap.Int64("normal_pending_requests", s.activeCount),
				zap.Int64("pd_pending_requests", s.pdActiveCount),
				zap.Int64("total_pending_requests", s.totalActiveCount()),
			}
			fields = append(fields, s.currentGroupTrackingFields()...)
			s.logger.Info("step already ended (idempotent hit)", fields...)
			ev.stepResult <- stepResult{pendingRequests: pendingRequests}
			return
		}
		ev.stepResult <- stepResult{
			err: fmt.Errorf("scheduler: cannot end step in phase %s, must be SERVING",
				phase),
		}
		return
	}
	currentStep := s.stepID.Load()
	if ev.stepID != currentStep {
		ev.stepResult <- stepResult{
			err: fmt.Errorf("scheduler: step_id mismatch, current=%d requested=%d",
				currentStep, ev.stepID),
		}
		return
	}

	// Record for idempotency before transitioning.
	s.lastEndedStepID = ev.stepID

	// Reject all queued requests — step is ending, no more allocations.
	s.rejectWaitingQueue(fmt.Errorf("step %d ended", ev.stepID))
	s.rejectPDWaitingQueue(fmt.Errorf("step %d ended", ev.stepID))
	s.queueEnabled = false
	s.queueEnabledAtomic.Store(false)

	pendingRequests := s.currentGroupActiveCount()
	if s.activeCount == 0 && s.pdActiveCount == 0 {
		// No in-flight requests, go directly to IDLE.
		s.stepPhase.Store(int32(domain.StepIdle))
		metrics.StepPhaseGauge.Set(float64(domain.StepIdle))
		s.closeStepDoneCh()
		fields := []zap.Field{
			logger.Event(logger.EventStepEnd),
			zap.String("resource_group", resourceGroup),
			zap.Int64("step_id", ev.stepID),
			zap.Int64("pending_requests", pendingRequests),
			zap.Int64("normal_pending_requests", s.activeCount),
			zap.Int64("pd_pending_requests", s.pdActiveCount),
			zap.Int64("total_pending_requests", s.totalActiveCount()),
			zap.Int("alloc_dedup_entries", len(s.allocDedup)),
			zap.Int("release_dedup_entries", len(s.releaseDedup)),
		}
		fields = append(fields, s.currentGroupTrackingFields()...)
		s.logger.Info("step ended immediately (no pending requests)", fields...)
	} else {
		s.stepPhase.Store(int32(domain.StepDraining))
		metrics.StepPhaseGauge.Set(float64(domain.StepDraining))
		fields := []zap.Field{
			logger.Event(logger.EventStepEnd),
			zap.String("resource_group", resourceGroup),
			zap.Int64("step_id", ev.stepID),
			zap.Int64("pending_requests", pendingRequests),
			zap.Int64("normal_pending_requests", s.activeCount),
			zap.Int64("pd_pending_requests", s.pdActiveCount),
			zap.Int64("total_pending_requests", s.totalActiveCount()),
		}
		fields = append(fields, s.currentGroupTrackingFields()...)
		s.logger.Info("step draining", fields...)
	}
	ev.stepResult <- stepResult{pendingRequests: pendingRequests}
}

func (s *Server) handleCleanupGateway(ev *event) {
	allocs := s.gatewayAllocs[ev.gatewayAddr]
	if len(allocs) == 0 {
		if ev.cleanupDoneCh != nil {
			close(ev.cleanupDoneCh)
		}
		return
	}

	byGroup := make(map[string][]gatewayAllocation, 4)
	for _, alloc := range allocs {
		group := alloc.resourceGroup
		if group == "" {
			group = s.resourceGroupForInstance(alloc.instanceID)
		}
		byGroup[group] = append(byGroup[group], alloc)
	}

	var totalReleased int64
	for group, groupAllocs := range byGroup {
		g := s.getGroup(group)
		if g == nil {
			continue
		}
		s.withGroup(g, func() {
			for _, alloc := range groupAllocs {
				s.releaseTrackedGatewayAllocation(alloc)
				totalReleased++
			}
			s.finishDrainingIfIdle("gateway cleanup")
		})
	}
	delete(s.gatewayAllocs, ev.gatewayAddr)
	s.pruneEmptyResourceGroups()

	s.logger.Warn("cleaned up ghost allocations for crashed gateway",
		zap.String("gateway_addr", ev.gatewayAddr),
		zap.Int64("released_count", totalReleased),
		zap.Int64("active_count", s.activeCount),
		zap.Int64("pd_active_count", s.pdActiveCount))
	metrics.GhostCleanupTotal.Inc()
	metrics.GhostCleanupReleasedTotal.Add(float64(totalReleased))

	if ev.cleanupDoneCh != nil {
		close(ev.cleanupDoneCh)
	}
}

func (s *Server) removeGatewayAllocationsForResourceGroup(group string) {
	if group == "" {
		group = domain.DefaultResourceGroup
	}
	for gatewayAddr, allocs := range s.gatewayAllocs {
		for allocID, alloc := range allocs {
			allocGroup := alloc.resourceGroup
			if allocGroup == "" {
				allocGroup = s.resourceGroupForInstance(alloc.instanceID)
			}
			if allocGroup == group {
				delete(allocs, allocID)
			}
		}
		if len(allocs) == 0 {
			delete(s.gatewayAllocs, gatewayAddr)
		}
	}
}

func (s *Server) finishDrainingIfIdle(reason string) {
	if domain.StepPhase(s.stepPhase.Load()) != domain.StepDraining || s.activeCount != 0 || s.pdActiveCount != 0 {
		return
	}
	s.stepPhase.Store(int32(domain.StepIdle))
	metrics.StepPhaseGauge.Set(float64(domain.StepIdle))
	s.closeStepDoneCh()
	fields := []zap.Field{
		zap.String("reason", reason),
		zap.String("resource_group", s.currentResourceGroup()),
		zap.Int64("step_id", s.stepID.Load()),
		zap.Int64("pending_requests", s.currentGroupActiveCount()),
		zap.Int64("normal_pending_requests", s.activeCount),
		zap.Int64("pd_pending_requests", s.pdActiveCount),
		zap.Int64("total_pending_requests", s.totalActiveCount()),
		zap.Int("alloc_dedup_entries", len(s.allocDedup)),
		zap.Int("release_dedup_entries", len(s.releaseDedup)),
	}
	fields = append(fields, s.currentGroupTrackingFields()...)
	s.logger.Info("step draining complete, transitioned to IDLE", fields...)
}

func (s *Server) closeStepDoneCh() {
	select {
	case <-s.stepDoneCh:
		return
	default:
		close(s.stepDoneCh)
	}
}

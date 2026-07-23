package scheduler

import (
	"time"

	"github.com/yzx/rl-router/internal/config"
	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
)

type groupRuntime struct {
	resourceGroup   string
	phase           domain.StepPhase
	stepID          int64
	stepDoneCh      chan struct{}
	lastEndedStepID int64
	paused          bool

	policyName          policy.Name
	policyConfig        policy.PolicyConfig
	currentPolicyConfig policy.PolicyConfig
	policy              policy.Policy

	batchSelector   policy.BatchSelector
	generationAware policy.GenerationAware
	driftTracker    policy.DriftAware
	cachedIDEnsurer policy.CachedIDEnsurer

	activeCount  int64
	maxInflight  int64
	allocCounter uint64

	queueEnabled bool
	queueMaxSize int64
	queueTimeout time.Duration
	waitingQueue []*event

	allocDedup   map[string]allocCacheEntry
	releaseDedup map[string]struct{}

	pdPrefillPolicy policy.Policy
	pdDecodePolicy  policy.Policy
	pdActiveCount   int64
	pdAllocDedup    map[string]pdAllocCacheEntry
	pdReleaseDedup  map[string]struct{}
	pdWaitingQueue  []*event

	drainCyclesWithoutRelease int
	lastDrainHadCapacityFree  bool
}

func newGroupRuntime(resourceGroup string, p policy.Policy, queueCfg config.WaitingQueueConfig) *groupRuntime {
	if resourceGroup == "" {
		resourceGroup = domain.DefaultResourceGroup
	}
	g := &groupRuntime{
		resourceGroup:  resourceGroup,
		phase:          domain.StepIdle,
		stepDoneCh:     make(chan struct{}),
		policyName:     p.Name(),
		policy:         p,
		allocDedup:     make(map[string]allocCacheEntry, defaultDedupCapacity/16),
		releaseDedup:   make(map[string]struct{}, defaultDedupCapacity/16),
		pdAllocDedup:   make(map[string]pdAllocCacheEntry, defaultDedupCapacity/16),
		pdReleaseDedup: make(map[string]struct{}, defaultDedupCapacity/16),
		waitingQueue:   make([]*event, 0),
		pdWaitingQueue: make([]*event, 0),
	}
	g.applyQueueConfig(queueCfg, policy.PolicyConfig{})
	g.bindPolicyInterfaces()
	return g
}

func (g *groupRuntime) bindPolicyInterfaces() {
	if bs, ok := g.policy.(policy.BatchSelector); ok {
		g.batchSelector = bs
	} else {
		g.batchSelector = nil
	}
	if ga, ok := g.policy.(policy.GenerationAware); ok {
		g.generationAware = ga
	} else {
		g.generationAware = nil
	}
	if dt, ok := g.policy.(policy.DriftAware); ok {
		g.driftTracker = dt
	} else {
		g.driftTracker = nil
	}
	if ce, ok := g.policy.(policy.CachedIDEnsurer); ok {
		g.cachedIDEnsurer = ce
	} else {
		g.cachedIDEnsurer = nil
	}
}

func (g *groupRuntime) applyQueueConfig(defaultCfg config.WaitingQueueConfig, cfg policy.PolicyConfig) {
	g.queueEnabled = defaultCfg.Enabled
	if cfg.WaitingQueueEnabled != nil {
		g.queueEnabled = *cfg.WaitingQueueEnabled
	}
	g.queueMaxSize = defaultCfg.MaxSize
	if g.queueMaxSize <= 0 {
		g.queueMaxSize = 100000
	}
	if cfg.WaitingQueueMaxSize != nil {
		g.queueMaxSize = *cfg.WaitingQueueMaxSize
		if g.queueMaxSize <= 0 {
			g.queueMaxSize = 100000
		}
	}
	g.queueTimeout = defaultCfg.Timeout.Duration
	if g.queueTimeout <= 0 {
		g.queueTimeout = 300 * time.Second
	}
	if cfg.WaitingQueueTimeoutSec != nil {
		g.queueTimeout = time.Duration(*cfg.WaitingQueueTimeoutSec) * time.Second
		if g.queueTimeout <= 0 {
			g.queueTimeout = 300 * time.Second
		}
	}
}

func (g *groupRuntime) totalActiveCount() int64 {
	return g.activeCount + g.pdActiveCount
}

func resourceGroupFromRoute(route *domain.RouteContext) string {
	if route == nil {
		return domain.DefaultResourceGroup
	}
	return domain.NormalizeResourceGroup(route.ResourceGroup)
}

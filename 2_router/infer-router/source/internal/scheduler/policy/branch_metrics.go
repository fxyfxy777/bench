package policy

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/yzx/rl-router/pkg/metrics"
)

// ---------- session_aware (V2) branches ----------

const (
	branchV2Stay             = "stay"
	branchV2NewSession       = "new_session"
	branchV2StaleReassign    = "stale_reassign"
	branchV2NoSessionFallback = "no_session_fallback"
)

// SessionAwareBranches holds pre-resolved counters for session_aware (V2) policy.
type SessionAwareBranches struct {
	Stay              prometheus.Counter
	NewSession        prometheus.Counter
	StaleReassign     prometheus.Counter
	NoSessionFallback prometheus.Counter
}

func newSessionAwareBranches() *SessionAwareBranches {
	return &SessionAwareBranches{
		Stay:              metrics.PolicyBranchTotal.WithLabelValues("session_aware", branchV2Stay),
		NewSession:        metrics.PolicyBranchTotal.WithLabelValues("session_aware", branchV2NewSession),
		StaleReassign:     metrics.PolicyBranchTotal.WithLabelValues("session_aware", branchV2StaleReassign),
		NoSessionFallback: metrics.PolicyBranchTotal.WithLabelValues("session_aware", branchV2NoSessionFallback),
	}
}

// ---------- session_aware_v3 branches ----------

const (
	branchV3Stay             = "stay"
	branchV3MigrateOverload  = "migrate_overload"
	branchV3MigrateLoadDiff  = "migrate_load_diff"
	branchV3NewSession       = "new_session"
	branchV3StaleReassign    = "stale_reassign"
	branchV3NoSessionFallback = "no_session_fallback"
)

// SessionAwareV3Branches holds pre-resolved counters for session_aware_v3 policy.
type SessionAwareV3Branches struct {
	Stay              prometheus.Counter
	MigrateOverload   prometheus.Counter
	MigrateLoadDiff   prometheus.Counter
	NewSession        prometheus.Counter
	StaleReassign     prometheus.Counter
	NoSessionFallback prometheus.Counter
}

func newSessionAwareV3Branches() *SessionAwareV3Branches {
	return &SessionAwareV3Branches{
		Stay:              metrics.PolicyBranchTotal.WithLabelValues("session_aware_v3", branchV3Stay),
		MigrateOverload:   metrics.PolicyBranchTotal.WithLabelValues("session_aware_v3", branchV3MigrateOverload),
		MigrateLoadDiff:   metrics.PolicyBranchTotal.WithLabelValues("session_aware_v3", branchV3MigrateLoadDiff),
		NewSession:        metrics.PolicyBranchTotal.WithLabelValues("session_aware_v3", branchV3NewSession),
		StaleReassign:     metrics.PolicyBranchTotal.WithLabelValues("session_aware_v3", branchV3StaleReassign),
		NoSessionFallback: metrics.PolicyBranchTotal.WithLabelValues("session_aware_v3", branchV3NoSessionFallback),
	}
}

// ---------- session_aware_v4 branches ----------

const (
	branchV4Stay              = "stay"
	branchV4MigrateOverload   = "migrate_overload"
	branchV4MigrateLoadDiff   = "migrate_load_diff"
	branchV4ImbalancedMigrate = "imbalanced_migrate"
	branchV4NewSession        = "new_session"
	branchV4StaleReassign     = "stale_reassign"
	branchV4NoSessionFallback = "no_session_fallback"
)

// SessionAwareV4Branches holds pre-resolved counters for session_aware_v4 policy.
type SessionAwareV4Branches struct {
	Stay              prometheus.Counter
	MigrateOverload   prometheus.Counter
	MigrateLoadDiff   prometheus.Counter
	ImbalancedMigrate prometheus.Counter
	NewSession        prometheus.Counter
	StaleReassign     prometheus.Counter
	NoSessionFallback prometheus.Counter
}

func newSessionAwareV4Branches() *SessionAwareV4Branches {
	return &SessionAwareV4Branches{
		Stay:              metrics.PolicyBranchTotal.WithLabelValues("session_aware_v4", branchV4Stay),
		MigrateOverload:   metrics.PolicyBranchTotal.WithLabelValues("session_aware_v4", branchV4MigrateOverload),
		MigrateLoadDiff:   metrics.PolicyBranchTotal.WithLabelValues("session_aware_v4", branchV4MigrateLoadDiff),
		ImbalancedMigrate: metrics.PolicyBranchTotal.WithLabelValues("session_aware_v4", branchV4ImbalancedMigrate),
		NewSession:        metrics.PolicyBranchTotal.WithLabelValues("session_aware_v4", branchV4NewSession),
		StaleReassign:     metrics.PolicyBranchTotal.WithLabelValues("session_aware_v4", branchV4StaleReassign),
		NoSessionFallback: metrics.PolicyBranchTotal.WithLabelValues("session_aware_v4", branchV4NoSessionFallback),
	}
}

// ---------- cache_aware branches ----------

const (
	branchCacheHit      = "cache_hit"
	branchTenantEvict   = "tenant_evict"
	branchCacheMiss     = "cache_miss"
	branchImbalanced    = "imbalanced"
	branchEmptyText     = "empty_text"
)

// CacheAwareBranches holds pre-resolved counters for cache_aware policy.
type CacheAwareBranches struct {
	CacheHit    prometheus.Counter
	TenantEvict prometheus.Counter
	CacheMiss   prometheus.Counter
	Imbalanced  prometheus.Counter
	EmptyText   prometheus.Counter
}

func newCacheAwareBranches() *CacheAwareBranches {
	return &CacheAwareBranches{
		CacheHit:    metrics.PolicyBranchTotal.WithLabelValues("cache_aware", branchCacheHit),
		TenantEvict: metrics.PolicyBranchTotal.WithLabelValues("cache_aware", branchTenantEvict),
		CacheMiss:   metrics.PolicyBranchTotal.WithLabelValues("cache_aware", branchCacheMiss),
		Imbalanced:  metrics.PolicyBranchTotal.WithLabelValues("cache_aware", branchImbalanced),
		EmptyText:   metrics.PolicyBranchTotal.WithLabelValues("cache_aware", branchEmptyText),
	}
}

// incByName increments the counter matching the given branch label by 1.
func (b *CacheAwareBranches) incByName(branch string) {
	switch branch {
	case branchCacheHit:
		b.CacheHit.Inc()
	case branchTenantEvict:
		b.TenantEvict.Inc()
	case branchCacheMiss:
		b.CacheMiss.Inc()
	case branchImbalanced:
		b.Imbalanced.Inc()
	case branchEmptyText:
		b.EmptyText.Inc()
	}
}

// addByName increments the counter matching the given branch label by delta.
func (b *CacheAwareBranches) addByName(branch string, delta int) {
	if delta <= 0 {
		return
	}
	v := float64(delta)
	switch branch {
	case branchCacheHit:
		b.CacheHit.Add(v)
	case branchTenantEvict:
		b.TenantEvict.Add(v)
	case branchCacheMiss:
		b.CacheMiss.Add(v)
	case branchImbalanced:
		b.Imbalanced.Add(v)
	case branchEmptyText:
		b.EmptyText.Add(v)
	}
}

// ---------- session_aware_v5 branches ----------

const (
	branchV5Stay             = "stay"
	branchV5MigrateOverload  = "migrate_overload"
	branchV5MigrateScoreDiff = "migrate_score_diff"
	branchV5MigrateEmergency = "migrate_emergency"
	branchV5Imbalanced        = "imbalanced"
	branchV5NewSession       = "new_session"
	branchV5StaleReassign    = "stale_reassign"
	branchV5NoSessionFallback = "no_session_fallback"
	branchV5BlockGated        = "block_gated"
)

// SessionAwareV5Branches holds pre-resolved counters for session_aware_v5 policy.
type SessionAwareV5Branches struct {
	Stay               prometheus.Counter
	MigrateOverload    prometheus.Counter
	MigrateScoreDiff   prometheus.Counter
	MigrateEmergency   prometheus.Counter
	Imbalanced         prometheus.Counter
	NewSession         prometheus.Counter
	StaleReassign     prometheus.Counter
	NoSessionFallback  prometheus.Counter
	BlockGated         prometheus.Counter
}

func newSessionAwareV5Branches() *SessionAwareV5Branches {
	return &SessionAwareV5Branches{
		Stay:               metrics.PolicyBranchTotal.WithLabelValues("session_aware_v5", branchV5Stay),
		MigrateOverload:   metrics.PolicyBranchTotal.WithLabelValues("session_aware_v5", branchV5MigrateOverload),
		MigrateScoreDiff:   metrics.PolicyBranchTotal.WithLabelValues("session_aware_v5", branchV5MigrateScoreDiff),
		MigrateEmergency: metrics.PolicyBranchTotal.WithLabelValues("session_aware_v5", branchV5MigrateEmergency),
		Imbalanced:         metrics.PolicyBranchTotal.WithLabelValues("session_aware_v5", branchV5Imbalanced),
		NewSession:         metrics.PolicyBranchTotal.WithLabelValues("session_aware_v5", branchV5NewSession),
		StaleReassign:     metrics.PolicyBranchTotal.WithLabelValues("session_aware_v5", branchV5StaleReassign),
		NoSessionFallback:  metrics.PolicyBranchTotal.WithLabelValues("session_aware_v5", branchV5NoSessionFallback),
		BlockGated:         metrics.PolicyBranchTotal.WithLabelValues("session_aware_v5", branchV5BlockGated),
	}
}

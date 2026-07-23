package policy

import (
	"fmt"
	"sort"
	"strings"
)

// Name identifies a scheduling policy.
type Name string

const DefaultName = NameMinLoad

// ──── registry: add new policies here ────

const (
	NameRoundRobin     Name = "round_robin"
	NameMinLoad        Name = "min_load"
	NameMinRequest     Name = "min_request"
	NameProcessTokens  Name = "process_tokens"
	NameRequestNum     Name = "request_num"
	NameSessionAware   Name = "session_aware"
	NameSessionAwareV3 Name = "session_aware_v3"
	NameSessionAwareV4 Name = "session_aware_v4"
	NameSessionAwareV5 Name = "session_aware_v5"
	NameCacheAware     Name = "cache_aware"
	NamePDCacheAware   Name = "pd_cache_aware"
)

// PolicyConfig carries optional per-policy parameters for Build.
// Fields that are zero-valued are treated as "use default".
type PolicyConfig struct {
	// MaxSessionLoad is the maximum number of sessions a single instance can hold.
	// Used by session_aware, session_aware_v3, and cache_aware policies. Default: 100.
	MaxSessionLoad int64 `json:"max_session_load,omitempty"`

	// LoadDiffThreshold controls migration sensitivity for session_aware_v3.
	// If (lastInstance.load - minLoad) > LoadDiffThreshold, migrate to min-load instance.
	// Default: 1.
	LoadDiffThreshold int64 `json:"load_diff_threshold,omitempty"`

	// CacheThreshold is the minimum prefix match ratio for cache-hit routing.
	// Below this, route to the instance with the most free cache. Default: 0.5.
	CacheThreshold float64 `json:"cache_threshold,omitempty"`

	// BalanceAbsThreshold is the absolute load diff to trigger imbalanced mode.
	// Defaults: 32 for cache_aware, 30 for pd_cache_aware.
	BalanceAbsThreshold int64 `json:"balance_abs_threshold,omitempty"`

	// BalanceRelThreshold is the relative load ratio to trigger imbalanced mode.
	// Defaults: 1.1 for cache_aware, 0.01 for pd_cache_aware.
	BalanceRelThreshold float64 `json:"balance_rel_threshold,omitempty"`

	// CacheBlockSize is the token block size used by pd_cache_aware. Default: 64.
	CacheBlockSize int `json:"cache_block_size,omitempty"`

	// HitRatioWeight is the cache-hit score weight used by pd_cache_aware. Default: 1.0.
	HitRatioWeight float64 `json:"hit_ratio_weight,omitempty"`

	// LoadBalanceWeight is the load score weight used by pd_cache_aware. Default: 0.5.
	LoadBalanceWeight float64 `json:"load_balance_weight,omitempty"`

	// MaxTreeSize is the max bytes per tenant before LRU eviction. Default: 10000.
	MaxTreeSize int64 `json:"max_tree_size,omitempty"`

	// EvictionIntervalSec is seconds between inline eviction sweeps. Default: 60.
	EvictionIntervalSec int64 `json:"eviction_interval_sec,omitempty"`

	// SessionEvictEpochs is the epoch interval for session LRU eviction.
	// Used by session_aware_v4. Default: 6000 (~60s at 100 req/s).
	SessionEvictEpochs uint64 `json:"session_evict_epochs,omitempty"`

	// SessionAffinityBoost is the maximum extra LoadDiffThreshold for warm sessions.
	// Used by session_aware_v4. Warm sessions (high requestCount) tolerate more
	// load imbalance before migrating, inspired by sglang cache-aware routing.
	// Default: 8.
	SessionAffinityBoost int64 `json:"session_affinity_boost,omitempty"`

	// Block-aware scheduling thresholds (session_aware_v5 policy).
	BlockMinThreshold    int64 `json:"block_min_threshold,omitempty"`    // default 256: exclude nodes below this from new sessions
	BlockEmergencyThresh int64 `json:"block_emergency_thresh,omitempty"` // default 128: force migration when blocks fall below this

	// Composite score weights (session_aware_v5 policy).
	WeightRequestLoad   float64 `json:"weight_request_load,omitempty"`   // default 0.3
	WeightWaitingCount  float64 `json:"weight_waiting_count,omitempty"`  // default 0.3
	WeightBlockPressure float64 `json:"weight_block_pressure,omitempty"` // default 0.3
	WeightDrift         float64 `json:"weight_drift,omitempty"`          // default 0.1

	// MaxInflight is the resource-group in-flight cap. Zero means no group cap.
	// It is consumed by the scheduler GroupRuntime, not by individual policies.
	MaxInflight int64 `json:"max_inflight,omitempty"`

	// MaxRequestLoad is the per-instance active-request / load-score cap.
	// An instance whose active requests (or composite score) exceeds this value
	// is skipped during scheduling.
	// Used by round_robin, min_load, min_request, and cache_aware policies.
	// Zero means "use policy default" (128 for rr/min_load/min_request, 100 for cache_aware).
	MaxRequestLoad int64 `json:"max_request_load,omitempty"`

	// Waiting queue overrides (per-step via StartStep API).
	// Pointer types to distinguish "not set" (nil → use startup default) from "set to false/0".
	WaitingQueueEnabled    *bool  `json:"waiting_queue_enabled,omitempty"`
	WaitingQueueMaxSize    *int64 `json:"waiting_queue_max_size,omitempty"`
	WaitingQueueTimeoutSec *int64 `json:"waiting_queue_timeout_sec,omitempty"`
	// PD separation (prefill/decode disaggregation) policies.
	// Passed through to the scheduler event-loop for centralized PD allocation.
	PDPrefillPolicy string `json:"pd_prefill_policy,omitempty"` // "process_tokens", "request_num", "pd_cache_aware"
	PDDecodePolicy  string `json:"pd_decode_policy,omitempty"`  // "process_tokens", "request_num"
}

var builders = map[Name]func(PolicyConfig) Policy{
	NameRoundRobin: func(cfg PolicyConfig) Policy {
		p := NewRoundRobinPolicy()
		if cfg.MaxRequestLoad > 0 {
			p.MaxRequestLoad = cfg.MaxRequestLoad
		}
		return p
	},
	NameMinLoad: func(cfg PolicyConfig) Policy {
		p := NewMinLoadPolicy()
		if cfg.MaxRequestLoad > 0 {
			p.MaxRequestLoad = cfg.MaxRequestLoad
		}
		return p
	},
	NameMinRequest: func(cfg PolicyConfig) Policy {
		p := NewMinRequestPolicy()
		if cfg.MaxRequestLoad > 0 {
			p.MaxRequestLoad = cfg.MaxRequestLoad
		}
		return p
	},
	NameSessionAware: func(cfg PolicyConfig) Policy {
		p := NewSessionAwarePolicy(cfg.MaxSessionLoad)
		if cfg.MaxRequestLoad > 0 {
			p.MaxRequestLoad = cfg.MaxRequestLoad
		}
		return p
	},
	NameSessionAwareV3: func(cfg PolicyConfig) Policy {
		p := NewSessionAwareV3Policy(cfg.MaxSessionLoad, cfg.LoadDiffThreshold)
		if cfg.MaxRequestLoad > 0 {
			p.MaxRequestLoad = cfg.MaxRequestLoad
		}
		return p
	},
	NameSessionAwareV4: func(cfg PolicyConfig) Policy {
		return NewSessionAwareV4Policy(SessionAwareV4Config{
			MaxSessionLoad:       cfg.MaxSessionLoad,
			MaxRequestLoad:       cfg.MaxRequestLoad,
			LoadDiffThreshold:    cfg.LoadDiffThreshold,
			BalanceAbsThreshold:  cfg.BalanceAbsThreshold,
			BalanceRelThreshold:  cfg.BalanceRelThreshold,
			SessionEvictEpochs:   cfg.SessionEvictEpochs,
			SessionAffinityBoost: cfg.SessionAffinityBoost,
		})
	},
	NameSessionAwareV5: func(cfg PolicyConfig) Policy {
		return NewSessionAwareV5Policy(SessionAwareV5Config{
			MaxSessionLoad:       cfg.MaxSessionLoad,
			MaxRequestLoad:       cfg.MaxRequestLoad,
			LoadDiffThreshold:    cfg.LoadDiffThreshold,
			BalanceAbsThreshold:  cfg.BalanceAbsThreshold,
			BalanceRelThreshold:  cfg.BalanceRelThreshold,
			SessionEvictEpochs:   cfg.SessionEvictEpochs,
			SessionAffinityBoost: cfg.SessionAffinityBoost,
			BlockMinThreshold:    cfg.BlockMinThreshold,
			BlockEmergencyThresh: cfg.BlockEmergencyThresh,
			WeightRequestLoad:    cfg.WeightRequestLoad,
			WeightWaitingCount:   cfg.WeightWaitingCount,
			WeightBlockPressure:  cfg.WeightBlockPressure,
			WeightDrift:          cfg.WeightDrift,
		})
	},
	NameCacheAware: func(cfg PolicyConfig) Policy {
		return NewCacheAwarePolicy(CacheAwarePolicyConfig{
			CacheThreshold:      cfg.CacheThreshold,
			BalanceAbsThreshold: cfg.BalanceAbsThreshold,
			BalanceRelThreshold: cfg.BalanceRelThreshold,
			MaxTreeSize:         cfg.MaxTreeSize,
			EvictionIntervalSec: cfg.EvictionIntervalSec,
			MaxRequestLoad:      cfg.MaxRequestLoad,
		})
	},
	NameProcessTokens: func(cfg PolicyConfig) Policy {
		p := NewProcessTokensPolicy()
		if cfg.MaxRequestLoad > 0 {
			p.MaxRequestLoad = cfg.MaxRequestLoad
		}
		return p
	},
	NameRequestNum: func(cfg PolicyConfig) Policy {
		p := NewRequestNumPolicy()
		if cfg.MaxRequestLoad > 0 {
			p.MaxRequestLoad = cfg.MaxRequestLoad
		}
		return p
	},
	NamePDCacheAware: func(cfg PolicyConfig) Policy {
		return NewPDCacheAwarePolicy(CacheAwarePolicyConfig{
			CacheThreshold:      cfg.CacheThreshold,
			BalanceAbsThreshold: cfg.BalanceAbsThreshold,
			BalanceRelThreshold: cfg.BalanceRelThreshold,
			MaxTreeSize:         cfg.MaxTreeSize,
			EvictionIntervalSec: cfg.EvictionIntervalSec,
			MaxRequestLoad:      cfg.MaxRequestLoad,
			CacheBlockSize:      cfg.CacheBlockSize,
			HitRatioWeight:      cfg.HitRatioWeight,
			LoadBalanceWeight:   cfg.LoadBalanceWeight,
		})
	},
}

// ──────────────────────────────────────────────────────────

// Build constructs a Policy by name with optional configuration.
// Accepts all registered policy names including PD variants
// ("process_tokens", "request_num", "pd_cache_aware").
// Returns an error if the name is not recognized.
func Build(name Name, cfg PolicyConfig) (Policy, error) {
	if name == "" {
		name = DefaultName
	}
	// Normalize to lowercase for case-insensitive lookup.
	normalized := Name(strings.ToLower(string(name)))
	if fn, ok := builders[normalized]; ok {
		return fn(cfg), nil
	}
	return nil, fmt.Errorf("unknown policy: %q, available: [%s]", name, availableNames())
}

func availableNames() string {
	names := make([]string, 0, len(builders))
	for n := range builders {
		names = append(names, string(n))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

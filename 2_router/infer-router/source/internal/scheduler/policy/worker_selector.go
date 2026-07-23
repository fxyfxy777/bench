package policy

import (
	"fmt"
	"math"
	"slices"

	"github.com/yzx/rl-router/internal/domain"
)

// Valid policy names for PD separation scheduling.
const (
	PolicyProcessTokens = "process_tokens"
	PolicyRequestNum    = "request_num"
	PolicyCacheAware    = "pd_cache_aware"
)

// ValidPrefillPolicies lists allowed values for prefill_policy.
var ValidPrefillPolicies = []string{PolicyProcessTokens, PolicyRequestNum, PolicyCacheAware}

// ValidDecodePolicies lists allowed values for decode_policy.
var ValidDecodePolicies = []string{PolicyProcessTokens, PolicyRequestNum}

// ValidatePrefillPolicy returns an error if the policy name is not in the allowed set.
func ValidatePrefillPolicy(name string) error {
	if slices.Contains(ValidPrefillPolicies, name) {
		return nil
	}
	return fmt.Errorf("invalid prefill_policy %q: must be one of %v", name, ValidPrefillPolicies)
}

// ValidateDecodePolicy returns an error if the policy name is not in the allowed set.
func ValidateDecodePolicy(name string) error {
	if slices.Contains(ValidDecodePolicies, name) {
		return nil
	}
	return fmt.Errorf("invalid decode_policy %q: must be one of %v", name, ValidDecodePolicies)
}

// processTokensSelect selects the instance with the fewest tokens being processed.
func processTokensSelect(instances []*domain.Instance, counterMgr *CounterManager) *domain.Instance {
	if len(instances) == 0 {
		return nil
	}
	var selected *domain.Instance
	var minTokens uint64 = math.MaxUint64
	for _, inst := range instances {
		tc := counterMgr.GetOrCreateTokenCounter(inst.ID)
		load := tc.Get()
		if load < minTokens {
			minTokens = load
			selected = inst
		}
	}
	return selected
}

// requestNumSelect selects the instance with the fewest concurrent requests.
func requestNumSelect(instances []*domain.Instance, counterMgr *CounterManager) *domain.Instance {
	if len(instances) == 0 {
		return nil
	}
	var selected *domain.Instance
	var minCount uint64 = math.MaxUint64
	for _, inst := range instances {
		c := counterMgr.GetOrCreateCounter(inst.ID)
		load := c.Get()
		if load < minCount {
			minCount = load
			selected = inst
		}
	}
	return selected
}

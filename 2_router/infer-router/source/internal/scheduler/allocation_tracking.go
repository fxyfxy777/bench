package scheduler

import "go.uber.org/zap"

func (s *Server) totalActiveCount() int64 {
	if len(s.groups) == 0 {
		return s.activeCount + s.pdActiveCount
	}
	var total int64
	for _, g := range s.groups {
		if g == s.activeGroup {
			total += s.activeCount + s.pdActiveCount
			continue
		}
		total += g.totalActiveCount()
	}
	return total
}

func allocationKindFromRole(role string) allocationKind {
	switch role {
	case "prefill":
		return allocationKindPrefill
	case "decode":
		return allocationKindDecode
	default:
		return allocationKindNormal
	}
}

type gatewayAllocationCounts struct {
	normal int
	pd     int
}

func (c gatewayAllocationCounts) total() int {
	return c.normal + c.pd
}

func activeTrackingGap(active int64, tracked int) int64 {
	gap := active - int64(tracked)
	if gap < 0 {
		return 0
	}
	return gap
}

func (s *Server) trackGatewayAllocation(gatewayAddr string, alloc gatewayAllocation) {
	if gatewayAddr == "" || alloc.allocationID == "" {
		return
	}
	if s.gatewayAllocs[gatewayAddr] == nil {
		s.gatewayAllocs[gatewayAddr] = make(map[string]gatewayAllocation, 128)
	}
	s.gatewayAllocs[gatewayAddr][alloc.allocationID] = alloc
}

func (s *Server) hasTrackedGatewayAllocation(gatewayAddr, allocationID string) bool {
	if gatewayAddr == "" || allocationID == "" {
		return false
	}
	_, ok := s.gatewayAllocs[gatewayAddr][allocationID]
	return ok
}

func (s *Server) gatewayAllocationTotal() int {
	total := 0
	for _, allocs := range s.gatewayAllocs {
		total += len(allocs)
	}
	return total
}

func (s *Server) gatewayAllocationCountsForResourceGroup(group string) gatewayAllocationCounts {
	var counts gatewayAllocationCounts
	for _, allocs := range s.gatewayAllocs {
		for _, alloc := range allocs {
			allocGroup := alloc.resourceGroup
			if allocGroup == "" {
				allocGroup = s.resourceGroupForInstance(alloc.instanceID)
			}
			if allocGroup != group {
				continue
			}
			switch alloc.kind {
			case allocationKindPrefill, allocationKindDecode:
				counts.pd++
			default:
				counts.normal++
			}
		}
	}
	return counts
}

func (s *Server) gatewayAllocationCountsAll() gatewayAllocationCounts {
	var counts gatewayAllocationCounts
	for _, allocs := range s.gatewayAllocs {
		for _, alloc := range allocs {
			switch alloc.kind {
			case allocationKindPrefill, allocationKindDecode:
				counts.pd++
			default:
				counts.normal++
			}
		}
	}
	return counts
}

func (s *Server) gatewayAllocationsByResourceGroup() map[string]int {
	if len(s.gatewayAllocs) == 0 {
		return nil
	}
	byGroup := make(map[string]int, len(s.groups))
	for _, allocs := range s.gatewayAllocs {
		for _, alloc := range allocs {
			group := alloc.resourceGroup
			if group == "" {
				group = s.resourceGroupForInstance(alloc.instanceID)
			}
			byGroup[group]++
		}
	}
	return byGroup
}

func (s *Server) activeCountsByResourceGroup() map[string]int64 {
	if len(s.groups) == 0 {
		if s.activeCount+s.pdActiveCount == 0 {
			return nil
		}
		return map[string]int64{s.currentResourceGroup(): s.activeCount + s.pdActiveCount}
	}
	byGroup := make(map[string]int64, len(s.groups))
	for group, g := range s.groups {
		active := g.totalActiveCount()
		if g == s.activeGroup {
			active = s.activeCount + s.pdActiveCount
		}
		if active > 0 {
			byGroup[group] = active
		}
	}
	if len(byGroup) == 0 {
		return nil
	}
	return byGroup
}

func (s *Server) currentGroupTrackingFields() []zap.Field {
	group := s.currentResourceGroup()
	counts := s.gatewayAllocationCountsForResourceGroup(group)
	return []zap.Field{
		zap.Int("gateway_allocs_entries", len(s.gatewayAllocs)),
		zap.Int("gateway_allocs_total", s.gatewayAllocationTotal()),
		zap.Int("group_gateway_allocs", counts.total()),
		zap.Int("group_gateway_normal_allocs", counts.normal),
		zap.Int("group_gateway_pd_allocs", counts.pd),
		zap.Int64("untracked_active_requests", activeTrackingGap(s.activeCount, counts.normal)),
		zap.Int64("untracked_pd_active_requests", activeTrackingGap(s.pdActiveCount, counts.pd)),
	}
}

func (s *Server) aggregateTrackingFields() []zap.Field {
	counts := s.gatewayAllocationCountsAll()
	return []zap.Field{
		zap.Int("resource_groups", len(s.groups)),
		zap.Int("gateway_allocs_entries", len(s.gatewayAllocs)),
		zap.Int("gateway_allocs_total", counts.total()),
		zap.Int("gateway_normal_allocs", counts.normal),
		zap.Int("gateway_pd_allocs", counts.pd),
		zap.Int64("untracked_active_requests", activeTrackingGap(s.activeCount, counts.normal)),
		zap.Int64("untracked_pd_active_requests", activeTrackingGap(s.pdActiveCount, counts.pd)),
		zap.Any("active_by_resource_group", s.activeCountsByResourceGroup()),
		zap.Any("tracked_allocs_by_resource_group", s.gatewayAllocationsByResourceGroup()),
	}
}

func (s *Server) untrackGatewayAllocation(gatewayAddr, allocationID, instanceID string, kind allocationKind) {
	allocs := s.gatewayAllocs[gatewayAddr]
	if len(allocs) == 0 {
		return
	}
	if allocationID != "" {
		delete(allocs, allocationID)
	} else {
		for key, alloc := range allocs {
			if alloc.instanceID == instanceID && alloc.kind == kind {
				delete(allocs, key)
				break
			}
		}
	}
	if len(allocs) == 0 {
		delete(s.gatewayAllocs, gatewayAddr)
	}
}

func (s *Server) releaseTrackedGatewayAllocation(alloc gatewayAllocation) {
	// Mark in dedup to prevent double-release from late-arriving Release events
	// (gateway may have sent Release RPC before crashing, still queued in eventCh).
	if alloc.allocationID != "" {
		switch alloc.kind {
		case allocationKindNormal:
			s.releaseDedup[alloc.allocationID] = struct{}{}
		case allocationKindPrefill, allocationKindDecode:
			s.pdReleaseDedup[alloc.allocationID] = struct{}{}
		}
	}

	s.state.Release(alloc.instanceID)
	freedCapacity := false
	switch alloc.kind {
	case allocationKindNormal:
		s.policy.Feedback(alloc.instanceID, nil)
		if s.driftTracker != nil {
			s.driftTracker.TrackRelease(alloc.instanceID)
		}
		if s.activeCount > 0 {
			s.activeCount--
			freedCapacity = true
		}
	case allocationKindPrefill:
		if s.pdPrefillPolicy != nil {
			s.pdPrefillPolicy.Feedback(alloc.instanceID, nil)
		}
		if s.pdActiveCount > 0 {
			s.pdActiveCount--
			freedCapacity = true
		}
	case allocationKindDecode:
		if s.pdDecodePolicy != nil {
			s.pdDecodePolicy.Feedback(alloc.instanceID, nil)
		}
		if s.pdActiveCount > 0 {
			s.pdActiveCount--
			freedCapacity = true
		}
	}
	if freedCapacity {
		s.lastDrainHadCapacityFree = true
	}
}

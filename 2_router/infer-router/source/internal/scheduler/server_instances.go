package scheduler

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/store"
	"github.com/yzx/rl-router/pkg/metrics"
)

// RegisterInstances adds or replaces backend instances through the event-loop.
func (s *Server) RegisterInstances(ctx context.Context, instances []*domain.Instance) error {
	return s.RegisterInstancesForResourceGroup(ctx, "", instances)
}

func (s *Server) RegisterInstancesForResourceGroup(ctx context.Context, resourceGroup string, instances []*domain.Instance) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ch := make(chan instanceMutationResult, 1)
	ev := getEvent()
	ev.typ = evRegisterInstances
	ev.resourceGroup = resourceGroup
	ev.instances = instances
	ev.instanceResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return err
	}
	select {
	case res := <-ch:
		return res.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UnregisterInstances removes backend instances through the event-loop.
func (s *Server) UnregisterInstances(ctx context.Context, ids []string) (int, error) {
	return s.UnregisterInstancesForResourceGroup(ctx, "", ids)
}

func (s *Server) UnregisterInstancesForResourceGroup(ctx context.Context, resourceGroup string, ids []string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ch := make(chan instanceMutationResult, 1)
	ev := getEvent()
	ev.typ = evUnregisterInstances
	ev.resourceGroup = resourceGroup
	ev.instanceIDs = ids
	ev.instanceResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return 0, err
	}
	select {
	case res := <-ch:
		return res.removed, res.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// SyncInstances atomically replaces the backend instance set through the event-loop.
func (s *Server) SyncInstances(ctx context.Context, instances []*domain.Instance) (store.SyncResult, error) {
	return s.SyncInstancesForResourceGroup(ctx, "", instances)
}

func (s *Server) SyncInstancesForResourceGroup(ctx context.Context, resourceGroup string, instances []*domain.Instance) (store.SyncResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ch := make(chan instanceMutationResult, 1)
	ev := getEvent()
	ev.typ = evSyncInstances
	ev.resourceGroup = resourceGroup
	ev.instances = instances
	ev.instanceResult = ch
	if err := s.submitEvent(ctx, ev); err != nil {
		putEvent(ev)
		return store.SyncResult{}, err
	}
	select {
	case res := <-ch:
		return res.syncResult, res.err
	case <-ctx.Done():
		return store.SyncResult{}, ctx.Err()
	}
}

func (s *Server) handleRegisterInstances(ev *event) {
	if ev.resourceGroup == "" {
		s.state.RegisterBatch(ev.instances)
	} else {
		if conflict, ok := s.state.FindResourceGroupConflict(ev.resourceGroup, ev.instances); ok {
			if ev.instanceResult != nil {
				ev.instanceResult <- instanceMutationResult{err: fmt.Errorf("%w: instance_id=%s existing_resource_group=%s requested_resource_group=%s",
					ErrResourceGroupInstanceConflict, conflict.ID, conflict.ExistingGroup, ev.resourceGroup)}
			}
			return
		}
		s.state.RegisterBatchForResourceGroup(ev.resourceGroup, ev.instances)
	}
	s.maybeBuildPDPoliciesAfterInstanceChange()
	if ev.instanceResult != nil {
		ev.instanceResult <- instanceMutationResult{}
	}
}

func (s *Server) handleUnregisterInstances(ev *event) {
	removedIDs := s.existingInstanceIDsForResourceGroup(ev.resourceGroup, ev.instanceIDs)
	removed := 0
	if ev.resourceGroup == "" {
		removed = s.state.UnregisterBatch(ev.instanceIDs)
	} else {
		removed = s.state.UnregisterResourceGroup(ev.resourceGroup, ev.instanceIDs)
	}
	s.purgeInstanceMetrics(removedIDs)
	s.pruneEmptyResourceGroups()
	if ev.instanceResult != nil {
		ev.instanceResult <- instanceMutationResult{removed: removed}
	}
}

func (s *Server) handleSyncInstances(ev *event) {
	removedIDs := s.removedIDsForResourceGroupSync(ev.resourceGroup, ev.instances)
	var result store.SyncResult
	if ev.resourceGroup == "" {
		result = s.state.Sync(ev.instances)
	} else {
		if conflict, ok := s.state.FindResourceGroupConflict(ev.resourceGroup, ev.instances); ok {
			if ev.instanceResult != nil {
				ev.instanceResult <- instanceMutationResult{err: fmt.Errorf("%w: instance_id=%s existing_resource_group=%s requested_resource_group=%s",
					ErrResourceGroupInstanceConflict, conflict.ID, conflict.ExistingGroup, ev.resourceGroup)}
			}
			return
		}
		result = s.state.SyncResourceGroup(ev.resourceGroup, ev.instances)
	}
	s.purgeInstanceMetrics(removedIDs)
	s.maybeBuildPDPoliciesAfterInstanceChange()
	s.pruneEmptyResourceGroups()
	if ev.instanceResult != nil {
		ev.instanceResult <- instanceMutationResult{syncResult: result}
	}
}

func (s *Server) existingInstanceIDs(ids []string) []string {
	return s.existingInstanceIDsForResourceGroup("", ids)
}

func (s *Server) existingInstanceIDsForResourceGroup(group string, ids []string) []string {
	nodes := s.state.GetNodes()
	removed := make([]string, 0, len(ids))
	for _, id := range ids {
		ns, ok := nodes[id]
		if !ok {
			continue
		}
		if group == "" || (ns != nil && ns.Instance != nil && domain.NormalizeResourceGroup(ns.Instance.ResourceGroup) == group) {
			removed = append(removed, id)
		}
	}
	return removed
}

func (s *Server) removedIDsForSync(instances []*domain.Instance) []string {
	return s.removedIDsForResourceGroupSync("", instances)
}

func (s *Server) removedIDsForResourceGroupSync(group string, instances []*domain.Instance) []string {
	incoming := make(map[string]struct{}, len(instances))
	for _, inst := range instances {
		if inst != nil {
			incoming[inst.ID] = struct{}{}
		}
	}
	nodes := s.state.GetNodes()
	removed := make([]string, 0)
	for id, ns := range nodes {
		if group != "" && (ns == nil || ns.Instance == nil || domain.NormalizeResourceGroup(ns.Instance.ResourceGroup) != group) {
			continue
		}
		if _, ok := incoming[id]; !ok {
			removed = append(removed, id)
		}
	}
	return removed
}

func (s *Server) purgeInstanceMetrics(ids []string) {
	for _, id := range ids {
		metrics.PurgeInstanceMetrics(id)
	}
}

func (s *Server) maybeBuildPDPoliciesAfterInstanceChange() {
	if len(s.groups) == 0 {
		s.getOrCreateGroup(domain.DefaultResourceGroup)
	}
	for _, g := range s.groups {
		if g.phase != domain.StepServing || (g.pdPrefillPolicy != nil && g.pdDecodePolicy != nil) {
			continue
		}
		s.withGroup(g, func() {
			if err := s.resolvePDPolicies(s.stepID.Load(), s.currentPolicyConfig); err != nil {
				s.logger.Warn("refresh pd policies after instance change failed",
					zap.String("resource_group", s.currentResourceGroup()),
					zap.Int64("step_id", s.stepID.Load()),
					zap.Error(err))
			}
		})
	}
}

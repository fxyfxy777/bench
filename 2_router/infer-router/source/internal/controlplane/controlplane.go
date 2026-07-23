package controlplane

import (
	"context"
	"fmt"
	"net/http"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	nodestore "github.com/yzx/rl-router/internal/scheduler/store"
)

const ResourceGroupHeader = "X-InferRouter-Resource-Group"

type ResourceGroupMetadata struct {
	ResourceGroup string `json:"resource_group,omitempty"`
}

type StepScheduler interface {
	StartResourceGroupStep(ctx context.Context, resourceGroup string, stepID int64, policyName string, policyCfg policy.PolicyConfig) error
	EndResourceGroupStep(ctx context.Context, resourceGroup string, stepID int64) (int64, error)
	ResourceGroupStepState(ctx context.Context, resourceGroup string) (domain.StepState, error)
}

type InstanceSyncer interface {
	SyncInstances(ctx context.Context, instances []*domain.Instance) (nodestore.SyncResult, error)
	SyncInstancesForResourceGroup(ctx context.Context, resourceGroup string, instances []*domain.Instance) (nodestore.SyncResult, error)
}

type StartStepCommand struct {
	ResourceGroup string
	StepID        int64
	PolicyName    string
	PolicyConfig  policy.PolicyConfig
}

type EndStepCommand struct {
	ResourceGroup string
	StepID        int64
}

type SyncInstancesCommand struct {
	ResourceGroup string
	Scoped        bool
	Instances     []*domain.Instance
}

func ResourceGroupFromRequest(r *http.Request, bodyGroup, metadataGroup string) (string, bool) {
	if r != nil {
		if group := r.Header.Get(ResourceGroupHeader); group != "" {
			return domain.NormalizeResourceGroup(group), true
		}
		if r.URL != nil {
			if group := r.URL.Query().Get("resource_group"); group != "" {
				return domain.NormalizeResourceGroup(group), true
			}
		}
	}
	if bodyGroup != "" {
		return domain.NormalizeResourceGroup(bodyGroup), true
	}
	if metadataGroup != "" {
		return domain.NormalizeResourceGroup(metadataGroup), true
	}
	return domain.DefaultResourceGroup, false
}

func StartStep(ctx context.Context, scheduler StepScheduler, cmd StartStepCommand) (domain.StepState, error) {
	group := domain.NormalizeResourceGroup(cmd.ResourceGroup)
	if err := scheduler.StartResourceGroupStep(ctx, group, cmd.StepID, cmd.PolicyName, cmd.PolicyConfig); err != nil {
		return domain.StepState{}, err
	}
	return scheduler.ResourceGroupStepState(ctx, group)
}

func EndStep(ctx context.Context, scheduler StepScheduler, cmd EndStepCommand) (int64, domain.StepState, error) {
	group := domain.NormalizeResourceGroup(cmd.ResourceGroup)
	pending, err := scheduler.EndResourceGroupStep(ctx, group, cmd.StepID)
	if err != nil {
		return 0, domain.StepState{}, err
	}
	state, err := scheduler.ResourceGroupStepState(ctx, group)
	if err != nil {
		return pending, domain.StepState{}, err
	}
	return pending, state, nil
}

func SyncInstances(ctx context.Context, scheduler InstanceSyncer, cmd SyncInstancesCommand) (nodestore.SyncResult, error) {
	if cmd.Scoped {
		return scheduler.SyncInstancesForResourceGroup(ctx, domain.NormalizeResourceGroup(cmd.ResourceGroup), cmd.Instances)
	}
	return scheduler.SyncInstances(ctx, cmd.Instances)
}

func ValidateScopedInstanceResourceGroups(instances []*domain.Instance, group string, scoped bool) error {
	if !scoped {
		return nil
	}
	group = domain.NormalizeResourceGroup(group)
	for i, inst := range instances {
		if inst == nil {
			continue
		}
		if inst.ResourceGroup != "" && domain.NormalizeResourceGroup(inst.ResourceGroup) != group {
			return fmt.Errorf("instances[%d]: resource_group %q conflicts with request resource_group %q",
				i, inst.ResourceGroup, group)
		}
		inst.ResourceGroup = group
	}
	return nil
}

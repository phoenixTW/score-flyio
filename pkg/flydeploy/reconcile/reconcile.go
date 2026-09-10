// Package reconcile applies a validated MachinePlan to one Fly app.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/phoenixTW/score-flyio/pkg/flydeploy/deployer"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/machineconfig"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/planner"
	"github.com/phoenixTW/score-flyio/pkg/flymachines"
)

const (
	defaultStateTimeout  = 2 * time.Minute
	defaultHealthTimeout = 2 * time.Minute
	defaultHealthPoll    = 2 * time.Second
)

type Options struct {
	StateTimeout  time.Duration
	HealthTimeout time.Duration
	HealthPoll    time.Duration
	SkipRelease   bool
}

func (o Options) withDefaults() Options {
	if o.StateTimeout <= 0 {
		o.StateTimeout = defaultStateTimeout
	}
	if o.HealthTimeout <= 0 {
		o.HealthTimeout = defaultHealthTimeout
	}
	if o.HealthPoll <= 0 {
		o.HealthPoll = defaultHealthPoll
	}
	return o
}

type Result struct {
	Changes []planner.MachineChange `json:"changes"`
}

// Plan reads live state and computes the exact changes apply would execute.
func Plan(ctx context.Context, d *deployer.Deployer, desired *machineconfig.Plan) (*Result, error) {
	if d == nil {
		return nil, fmt.Errorf("deployer must not be nil")
	}
	machines, err := d.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	changes, err := planner.Diff(desired, machines)
	if err != nil {
		return nil, err
	}
	return &Result{Changes: changes}, nil
}

// Apply reconciles all managed groups. A failed rollout deletes machines
// created during this invocation, leaving existing capacity untouched.
func Apply(ctx context.Context, d *deployer.Deployer, desired *machineconfig.Plan, options Options) (*Result, error) {
	if d == nil {
		return nil, fmt.Errorf("deployer must not be nil")
	}
	if desired == nil {
		return nil, fmt.Errorf("desired plan must not be nil")
	}
	options = options.withDefaults()
	result, err := Plan(ctx, d, desired)
	if err != nil {
		return nil, err
	}
	if !options.SkipRelease {
		if err := runReleaseCommand(ctx, d, desired, options); err != nil {
			return nil, fmt.Errorf("apply failed: %w", err)
		}
	}
	created := make([]string, 0)
	rollback := func(cause error) (*Result, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var cleanupErrors []error
		for _, machineID := range created {
			if err := d.DeleteMachine(cleanupCtx, machineID, true); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup machine %s: %w", machineID, err))
			}
		}
		return nil, errors.Join(fmt.Errorf("apply failed: %w", cause), errors.Join(cleanupErrors...))
	}
	deferredDeletes := make([]string, 0)

	groups := make(map[string]machineconfig.Group, len(desired.Groups))
	for _, group := range desired.Groups {
		groups[group.Name] = group
	}
	for _, change := range result.Changes {
		group, ok := groups[change.Group]
		if !ok && change.Action != planner.ActionDelete {
			return rollback(fmt.Errorf("plan references unknown group %q", change.Group))
		}
		switch change.Action {
		case planner.ActionNoop:
			continue
		case planner.ActionDelete:
			deferredDeletes = append(deferredDeletes, change.MachineID)
		case planner.ActionCreate:
			machine, err := d.CreateMachine(ctx, "", group.Region, configWithHash(group, change.ConfigHash))
			if err != nil {
				return rollback(err)
			}
			if machine == nil || machine.Id == nil || *machine.Id == "" {
				return rollback(fmt.Errorf("machines create returned no machine id"))
			}
			machineID := *machine.Id
			created = append(created, machineID)
			if err := d.WaitForState(ctx, machineID, flymachines.MachinesWaitParamsStateStarted, options.StateTimeout); err != nil {
				return rollback(err)
			}
			if err := d.WaitForHealthy(ctx, machineID, options.HealthTimeout, options.HealthPoll); err != nil {
				return rollback(err)
			}
		case planner.ActionUpdate:
			machine, found, err := d.GetMachine(ctx, change.MachineID)
			if err != nil {
				return rollback(err)
			}
			if !found || machine == nil {
				return rollback(fmt.Errorf("managed machine %q disappeared before update", change.MachineID))
			}
			version := ""
			if machine.InstanceId != nil {
				version = *machine.InstanceId
			}
			if _, err := d.UpdateMachine(ctx, change.MachineID, version, change.MachineName, configWithHash(group, change.ConfigHash)); err != nil {
				return rollback(err)
			}
			if err := d.WaitForState(ctx, change.MachineID, flymachines.MachinesWaitParamsStateStarted, options.StateTimeout); err != nil {
				return rollback(err)
			}
			if err := d.WaitForHealthy(ctx, change.MachineID, options.HealthTimeout, options.HealthPoll); err != nil {
				return rollback(err)
			}
		case planner.ActionReplace:
			machine, err := d.CreateMachine(ctx, "", group.Region, configWithHash(group, change.ConfigHash))
			if err != nil {
				return rollback(err)
			}
			if machine == nil || machine.Id == nil || *machine.Id == "" {
				return rollback(fmt.Errorf("replacement create returned no machine id"))
			}
			newID := *machine.Id
			created = append(created, newID)
			if err := d.WaitForState(ctx, newID, flymachines.MachinesWaitParamsStateStarted, options.StateTimeout); err != nil {
				return rollback(err)
			}
			if err := d.WaitForHealthy(ctx, newID, options.HealthTimeout, options.HealthPoll); err != nil {
				return rollback(err)
			}
			deferredDeletes = append(deferredDeletes, change.MachineID)
		default:
			return rollback(fmt.Errorf("unsupported plan action %q", change.Action))
		}
	}
	for _, machineID := range deferredDeletes {
		if err := d.DeleteMachine(ctx, machineID, true); err != nil {
			return rollback(err)
		}
	}
	return result, nil
}

func configWithHash(group machineconfig.Group, hash string) flymachines.FlyMachineConfig {
	metadata := make(map[string]string, len(group.Metadata)+1)
	for key, value := range group.Metadata {
		metadata[key] = value
	}
	metadata[planner.MetadataGroup] = group.Name
	metadata[planner.MetadataHash] = hash
	group.Metadata = metadata
	return group.ToFlyMachineConfig()
}

const releaseGroupName = "release"

func runReleaseCommand(ctx context.Context, d *deployer.Deployer, desired *machineconfig.Plan, options Options) error {
	if len(desired.ReleaseCommand) == 0 || len(desired.Groups) == 0 {
		return nil
	}
	group := desired.Groups[0]
	machine, err := d.CreateMachine(ctx, desired.Workload+"-release", group.Region, releaseMachineConfig(group))
	if err != nil {
		return err
	}
	if machine == nil || machine.Id == nil || *machine.Id == "" {
		return fmt.Errorf("release machine create returned no machine id")
	}
	machineID := *machine.Id
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return d.DeleteMachine(cleanupCtx, machineID, true)
	}
	code, err := d.WaitExit(ctx, machineID, options.HealthTimeout, options.HealthPoll)
	if err != nil {
		return errors.Join(fmt.Errorf("release command: %w", err), cleanup())
	}
	if code != 0 {
		return errors.Join(fmt.Errorf("release command exited with code %d", code), cleanup())
	}
	return cleanup()
}

func releaseMachineConfig(group machineconfig.Group) flymachines.FlyMachineConfig {
	metadata := make(map[string]string, len(group.Metadata)+1)
	for key, value := range group.Metadata {
		metadata[key] = value
	}
	metadata[planner.MetadataGroup] = releaseGroupName
	release := group
	release.Restart = machineconfig.RestartPolicyNo
	release.Services = nil
	release.Checks = nil
	release.Metadata = metadata
	return release.ToFlyMachineConfig()
}

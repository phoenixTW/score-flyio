package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"

	scoreloader "github.com/score-spec/score-go/loader"
	scoreschema "github.com/score-spec/score-go/schema"
	scoretypes "github.com/score-spec/score-go/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/astromechza/score-flyio/internal/convert"
	"github.com/astromechza/score-flyio/internal/deployer"
	"github.com/astromechza/score-flyio/internal/flymachines"
	"github.com/astromechza/score-flyio/internal/machineconfig"
	"github.com/astromechza/score-flyio/internal/planner"
	"github.com/astromechza/score-flyio/internal/provisioners"
	"github.com/astromechza/score-flyio/internal/reconcile"
	"github.com/astromechza/score-flyio/internal/state"
)

type machineCommandOptions struct {
	overridesFile    string
	overrideProperty []string
	image            string
	environment      string
	dryRun           bool
	yes              bool
	planFile         string
	planOutput       string
}

type machineInput struct {
	state   *state.State
	plan    *machineconfig.Plan
	secrets map[string]string
}

var newMachinesClient = flymachines.NewFlyClient

type machineHookSet struct {
	apply     func(context.Context, *machineconfig.Plan, []planner.MachineChange, map[string]string) error
	status    func(context.Context, *machineconfig.Plan, io.Writer) error
	reconcile func(context.Context, *machineconfig.Plan, []planner.MachineChange, map[string]string) error
	destroy   func(context.Context, *machineconfig.Plan) error
}

var machineHooks machineHookSet

func loadMachineInput(cmd *cobra.Command, workloadFile string, options machineCommandOptions, persist bool) (*machineInput, error) {
	sd, ok, err := state.LoadStateDirectory(".")
	if err != nil {
		return nil, fmt.Errorf("failed to load existing state directory: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("state directory does not exist, please run \"init\" first")
	}
	raw, err := os.ReadFile(workloadFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read input score file: %s: %w", workloadFile, err)
	}
	var rawWorkload map[string]interface{}
	if err := yaml.Unmarshal(raw, &rawWorkload); err != nil {
		return nil, fmt.Errorf("failed to decode input score file: %s: %w", workloadFile, err)
	}
	if options.overridesFile != "" {
		if err := parseAndApplyOverrideFile(options.overridesFile, "overrides-file", rawWorkload); err != nil {
			return nil, err
		}
	}
	for _, entry := range options.overrideProperty {
		rawWorkload, err = parseAndApplyOverrideProperty(entry, "override-property", rawWorkload)
		if err != nil {
			return nil, err
		}
	}
	if changes, err := scoreschema.ApplyCommonUpgradeTransforms(rawWorkload); err != nil {
		return nil, fmt.Errorf("failed to upgrade spec: %w", err)
	} else if len(changes) > 0 {
		for _, change := range changes {
			cmd.PrintErrln("Applying backwards compatible upgrade", change)
		}
	}
	if err := scoreschema.Validate(rawWorkload); err != nil {
		return nil, fmt.Errorf("invalid score file: %s: %w", workloadFile, err)
	}
	var workload scoretypes.Workload
	if err := scoreloader.MapSpec(&workload, rawWorkload); err != nil {
		return nil, fmt.Errorf("failed to decode input score file: %s: %w", workloadFile, err)
	}
	workloadName, ok := workload.Metadata["name"].(string)
	if !ok || workloadName == "" {
		return nil, fmt.Errorf("score metadata.name must be set")
	}
	for name, container := range workload.Containers {
		if container.Image == "." && options.image != "" {
			container.Image = options.image
			workload.Containers[name] = container
		}
	}
	current := &sd.State
	if current, err = current.WithWorkload(&workload, &workloadFile, state.WorkloadExtras{}); err != nil {
		return nil, fmt.Errorf("failed to add score file to project: %w", err)
	}
	if current, err = current.WithPrimedResources(); err != nil {
		return nil, fmt.Errorf("failed to prime resources: %w", err)
	}
	if current, err = provisioners.ProvisionResources(current); err != nil {
		return nil, fmt.Errorf("failed to provision resources: %w", err)
	}
	if persist {
		sd.State = *current
		if err := sd.Persist(); err != nil {
			return nil, fmt.Errorf("failed to persist state file: %w", err)
		}
	}
	plan, secrets, err := convert.MachinePlanWithSecrets(current, workloadName, options.environment, rootCmd.Version)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, fmt.Errorf("workload %q has no metadata.progresify machine plan", workloadName)
	}
	if err := plan.ValidateImmutableImages(); err != nil {
		return nil, err
	}
	return &machineInput{state: current, plan: plan, secrets: secrets}, nil
}

type machinePlanOutput struct {
	AppName           string                  `json:"app_name"`
	Workload          string                  `json:"workload"`
	RendererVersion   string                  `json:"renderer_version,omitempty"`
	DeployEnvironment string                  `json:"deploy_environment,omitempty"`
	MachineGroups     []machineGroupOutput    `json:"machine_groups"`
	Changes           []planner.MachineChange `json:"changes"`
}

type machineGroupOutput struct {
	Name         string       `json:"name"`
	Region       string       `json:"region"`
	Scale        machineScale `json:"scale"`
	Cpus         int          `json:"cpus,omitempty"`
	MemoryMb     int          `json:"memory_mb,omitempty"`
	Containers   []string     `json:"containers"`
	ImageDigests []string     `json:"image_digests"`
	PublicPorts  []int        `json:"public_ports,omitempty"`
}

type machineScale struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type exactMachinePlanFile struct {
	Plan   machineconfig.Plan `json:"plan"`
	SHA256 string             `json:"sha256"`
}

func outputFor(input *machineInput, changes []planner.MachineChange) machinePlanOutput {
	out := machinePlanOutput{AppName: input.plan.AppName, Workload: input.plan.Workload, RendererVersion: input.plan.RendererVersion, DeployEnvironment: input.plan.Environment, Changes: changes}
	for _, group := range input.plan.Groups {
		item := machineGroupOutput{Name: group.Name, Region: group.Region, Scale: machineScale{Min: group.MinMachines, Max: group.MaxMachines}}
		if group.Guest != nil {
			item.Cpus, item.MemoryMb = group.Guest.Cpus, group.Guest.MemoryMb
		}
		for _, container := range group.Containers {
			item.Containers = append(item.Containers, container.Name)
			if container.ImageDigest != "" {
				item.ImageDigests = append(item.ImageDigests, container.ImageDigest)
			}
		}
		for _, service := range group.Services {
			for _, port := range service.Ports {
				item.PublicPorts = append(item.PublicPorts, port.Port)
			}
		}
		slices.Sort(item.Containers)
		slices.Sort(item.ImageDigests)
		slices.Sort(item.PublicPorts)
		out.MachineGroups = append(out.MachineGroups, item)
	}
	return out
}

func writeMachineJSON(cmd *cobra.Command, value any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func machineOptions(cmd *cobra.Command) machineCommandOptions {
	o := machineCommandOptions{}
	o.overridesFile, _ = cmd.Flags().GetString("overrides-file")
	o.overrideProperty, _ = cmd.Flags().GetStringArray("override-property")
	o.image, _ = cmd.Flags().GetString("image")
	o.environment, _ = cmd.Flags().GetString("environment")
	o.dryRun, _ = cmd.Flags().GetBool("dry-run")
	o.yes, _ = cmd.Flags().GetBool("yes")
	o.planFile, _ = cmd.Flags().GetString("plan-file")
	o.planOutput, _ = cmd.Flags().GetString("plan-output")
	return o
}

func setupMachineFlags(cmd *cobra.Command, includeYes bool) {
	cmd.Flags().String("overrides-file", "", "optional Score overrides file")
	cmd.Flags().StringArray("override-property", nil, "Score path=value override")
	cmd.Flags().String("image", "", "image to use for containers with image '.'")
	cmd.Flags().String("environment", "staging", "deployment environment")
	cmd.Flags().Bool("dry-run", false, "do not contact Fly")
	cmd.Flags().String("plan-file", "", "consume an exact machine plan JSON file")
	cmd.Flags().String("plan-output", "", "write the exact machine plan JSON to this file")
	if includeYes {
		cmd.Flags().Bool("yes", false, "confirm destructive actions")
	}
}

func runMachinePlan(cmd *cobra.Command, args []string) error {
	input, err := loadMachineInput(cmd, args[0], machineOptions(cmd), false)
	if err != nil {
		return err
	}
	var changes []planner.MachineChange
	changes, err = planner.Diff(input.plan, nil)
	if !machineOptions(cmd).dryRun {
		if client, clientErr := newMachinesClient(); clientErr == nil {
			if live, listErr := reconcile.Plan(cmd.Context(), deployer.New(client, input.plan.AppName), input.plan); listErr == nil {
				changes = live.Changes
			} else if os.Getenv("FLY_API_TOKEN") != "" {
				return listErr
			}
		} else if os.Getenv("FLY_API_TOKEN") != "" {
			return clientErr
		}
	}
	if err != nil {
		return err
	}
	options := machineOptions(cmd)
	if options.planOutput != "" {
		planRaw, marshalErr := json.Marshal(input.plan)
		if marshalErr != nil {
			return marshalErr
		}
		sum := sha256.Sum256(planRaw)
		artifact, marshalErr := json.MarshalIndent(exactMachinePlanFile{Plan: *input.plan, SHA256: hex.EncodeToString(sum[:])}, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		if writeErr := os.WriteFile(options.planOutput, append(artifact, '\n'), 0600); writeErr != nil {
			return fmt.Errorf("write plan: %w", writeErr)
		}
	}
	return writeMachineJSON(cmd, outputFor(input, changes))
}

func runMachineApply(cmd *cobra.Command, args []string) error {
	options := machineOptions(cmd)
	if options.dryRun {
		return fmt.Errorf("--dry-run is only supported by plan and validate")
	}
	input, err := loadMachineInput(cmd, args[0], options, true)
	if err != nil {
		return err
	}
	if options.planFile != "" {
		raw, readErr := os.ReadFile(options.planFile)
		if readErr != nil {
			return fmt.Errorf("read plan: %w", readErr)
		}
		var artifact exactMachinePlanFile
		if decodeErr := json.Unmarshal(raw, &artifact); decodeErr != nil {
			return fmt.Errorf("decode plan: %w", decodeErr)
		}
		planRaw, marshalErr := json.Marshal(artifact.Plan)
		if marshalErr != nil {
			return marshalErr
		}
		sum := sha256.Sum256(planRaw)
		if artifact.SHA256 != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("plan integrity check failed")
		}
		exact := artifact.Plan
		if exact.AppName != input.plan.AppName || exact.Workload != input.plan.Workload {
			return fmt.Errorf("plan file targets app %q workload %q but score file targets app %q workload %q", exact.AppName, exact.Workload, input.plan.AppName, input.plan.Workload)
		}
		if validateErr := exact.Validate(); validateErr != nil {
			return fmt.Errorf("invalid plan: %w", validateErr)
		}
		if validateErr := exact.ValidateImmutableImages(); validateErr != nil {
			return validateErr
		}
		input.plan = &exact
		input.secrets = nil
	}
	changes, err := planner.Diff(input.plan, nil)
	if err != nil {
		return err
	}
	if machineHooks.apply != nil {
		if err := machineHooks.apply(cmd.Context(), input.plan, changes, input.secrets); err != nil {
			return err
		}
		return writeMachineJSON(cmd, outputFor(input, changes))
	}
	client, err := newMachinesClient()
	if err != nil {
		return err
	}
	d := deployer.New(client, input.plan.AppName)
	if _, err := d.EnsureApp(cmd.Context(), flymachines.CreateAppRequest{AppName: &input.plan.AppName}); err != nil {
		return err
	}
	if err := setMachineSecrets(cmd, client.ApiToken, input.plan.AppName, input.secrets); err != nil {
		return err
	}
	result, err := reconcile.Apply(cmd.Context(), d, input.plan, reconcile.Options{})
	if err != nil {
		return err
	}
	return writeMachineJSON(cmd, outputFor(input, result.Changes))
}

func runMachineValidate(cmd *cobra.Command, args []string) error {
	input, err := loadMachineInput(cmd, args[0], machineOptions(cmd), false)
	if err != nil {
		return err
	}
	return writeMachineJSON(cmd, outputFor(input, nil))
}

func runMachineStatus(cmd *cobra.Command, args []string) error {
	if machineOptions(cmd).dryRun {
		return fmt.Errorf("--dry-run is only supported by plan and validate")
	}
	input, err := loadMachineInput(cmd, args[0], machineOptions(cmd), false)
	if err != nil {
		return err
	}
	if machineHooks.status != nil {
		return machineHooks.status(cmd.Context(), input.plan, cmd.OutOrStdout())
	}
	client, err := newMachinesClient()
	if err != nil {
		return fmt.Errorf("fly client is not configured for machine status")
	}
	machines, err := deployer.New(client, input.plan.AppName).ListMachines(cmd.Context())
	if err != nil {
		return err
	}
	type machineStatus struct {
		ID     string `json:"id"`
		Name   string `json:"name,omitempty"`
		Group  string `json:"group,omitempty"`
		State  string `json:"state,omitempty"`
		Region string `json:"region,omitempty"`
	}
	result := make([]machineStatus, 0, len(machines))
	for _, machine := range machines {
		group := ""
		if machine.Config != nil && machine.Config.Metadata != nil {
			group = (*machine.Config.Metadata)[planner.MetadataGroup]
		}
		result = append(result, machineStatus{ID: value(machine.Id), Name: value(machine.Name), Group: group, State: value(machine.State), Region: value(machine.Region)})
	}
	return writeMachineJSON(cmd, result)
}

func runMachineDestroy(cmd *cobra.Command, args []string) error {
	options := machineOptions(cmd)
	if options.dryRun {
		return fmt.Errorf("--dry-run is only supported by plan and validate")
	}
	if !options.yes {
		return fmt.Errorf("destroy requires --yes")
	}
	input, err := loadMachineInput(cmd, args[0], options, true)
	if err != nil {
		return err
	}
	if machineHooks.destroy != nil {
		return machineHooks.destroy(cmd.Context(), input.plan)
	}
	client, err := newMachinesClient()
	if err != nil {
		return err
	}
	d := deployer.New(client, input.plan.AppName)
	machines, err := d.ListMachines(cmd.Context())
	if err != nil {
		return err
	}
	deleted := 0
	for _, machine := range machines {
		if machine.Config == nil || machine.Config.Metadata == nil || (*machine.Config.Metadata)[planner.MetadataGroup] == "" {
			continue
		}
		if err := d.DeleteMachine(cmd.Context(), value(machine.Id), true); err != nil {
			return err
		}
		deleted++
	}
	appDeleted := false
	if remaining, listErr := d.ListMachines(cmd.Context()); listErr == nil && len(remaining) == 0 {
		if deleteErr := d.DeleteApp(cmd.Context(), input.plan.AppName); deleteErr != nil {
			return deleteErr
		}
		appDeleted = true
	}
	return writeMachineJSON(cmd, map[string]any{"app_name": input.plan.AppName, "deleted": deleted, "app_deleted": appDeleted})
}

func setMachineSecrets(cmd *cobra.Command, token, app string, secrets map[string]string) error {
	if len(secrets) == 0 {
		return nil
	}
	args := []string{"secrets", "set", "--access-token", token, "--app", app, "--stage"}
	for key, secret := range secrets {
		args = append(args, fmt.Sprintf("%s=%s", key, secret))
	}
	secretCommand := exec.Command("fly", args...)
	secretCommand.Stderr, secretCommand.Stdout = cmd.ErrOrStderr(), cmd.ErrOrStderr()
	if err := secretCommand.Run(); err != nil {
		return fmt.Errorf("failed to set machine secrets: %w", err)
	}
	return nil
}

func runMachineReconcile(cmd *cobra.Command, args []string) error {
	if machineOptions(cmd).dryRun {
		return fmt.Errorf("--dry-run is only supported by plan and validate")
	}
	input, err := loadMachineInput(cmd, args[0], machineOptions(cmd), true)
	if err != nil {
		return err
	}
	changes, err := planner.Diff(input.plan, nil)
	if err != nil {
		return err
	}
	if machineHooks.reconcile != nil {
		if err := machineHooks.reconcile(cmd.Context(), input.plan, changes, input.secrets); err != nil {
			return err
		}
		return writeMachineJSON(cmd, outputFor(input, changes))
	}
	client, err := newMachinesClient()
	if err != nil {
		return err
	}
	d := deployer.New(client, input.plan.AppName)
	if _, err := d.EnsureApp(cmd.Context(), flymachines.CreateAppRequest{AppName: &input.plan.AppName}); err != nil {
		return err
	}
	if err := setMachineSecrets(cmd, client.ApiToken, input.plan.AppName, input.secrets); err != nil {
		return err
	}
	result, err := reconcile.Apply(cmd.Context(), d, input.plan, reconcile.Options{})
	if err != nil {
		return err
	}
	return writeMachineJSON(cmd, outputFor(input, result.Changes))
}

func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func newMachineCommand(use string, run func(*cobra.Command, []string) error, includeYes bool) *cobra.Command {
	cmd := &cobra.Command{Use: use, Args: cobra.ExactArgs(1), SilenceUsage: true, RunE: run}
	setupMachineFlags(cmd, includeYes)
	return cmd
}

func init() {
	validate := newMachineCommand("validate SCORE_FILE", runMachineValidate, false)
	plan := newMachineCommand("plan SCORE_FILE", runMachinePlan, false)
	apply := newMachineCommand("apply SCORE_FILE", runMachineApply, false)
	status := newMachineCommand("status SCORE_FILE", runMachineStatus, false)
	reconcileCmd := newMachineCommand("reconcile SCORE_FILE", runMachineReconcile, false)
	destroy := newMachineCommand("destroy SCORE_FILE", runMachineDestroy, true)
	rootCmd.AddCommand(validate, plan, apply, status, reconcileCmd, destroy)
}

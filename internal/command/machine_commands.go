package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"

	scoreloader "github.com/score-spec/score-go/loader"
	scoreschema "github.com/score-spec/score-go/schema"
	scoretypes "github.com/score-spec/score-go/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/phoenixTW/score-flyio/internal/convert"
	"github.com/phoenixTW/score-flyio/internal/provisioners"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/deployer"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/machineconfig"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/planner"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/reconcile"
	"github.com/phoenixTW/score-flyio/pkg/flymachines"
	"github.com/phoenixTW/score-flyio/pkg/state"
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
	secretsFile      string
}

type machineInput struct {
	state   *state.State
	plan    *machineconfig.Plan
	secrets map[string]string
}

var newMachinesClient = flymachines.NewFlyClient

var execFly = func(args []string, stdout, stderr io.Writer) error {
	flyCommand := exec.Command("fly", args...)
	flyCommand.Stdout, flyCommand.Stderr = stdout, stderr
	return flyCommand.Run()
}

var execFlyWithInput = func(args []string, token, stdin string, stdout, stderr io.Writer) error {
	flyCommand := exec.Command("fly", args...)
	flyCommand.Env = environmentWithFlyToken(flyCommand.Environ(), token)
	flyCommand.Stdin = strings.NewReader(stdin)
	flyCommand.Stdout, flyCommand.Stderr = stdout, stderr
	return flyCommand.Run()
}

func environmentWithFlyToken(environment []string, token string) []string {
	const tokenKey = "FLY_API_TOKEN="
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, tokenKey) {
			out = append(out, entry)
		}
	}
	return append(out, tokenKey+token)
}

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
	if current, err = current.WithWorkload(&workload, &workloadFile, sd.State.Workloads[workloadName].Extras); err != nil {
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
		return nil, fmt.Errorf("workload %q has no metadata.fly machine plan", workloadName)
	}
	for i := range plan.Groups {
		if override, ok := sd.State.Workloads[workloadName].Extras.ScaleOverrides[plan.Groups[i].Name]; ok {
			plan.Groups[i].MinMachines = override.Min
			plan.Groups[i].MaxMachines = override.Max
		}
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if err := plan.ValidateImmutableImages(); err != nil {
		return nil, err
	}
	return &machineInput{state: current, plan: plan, secrets: secrets}, nil
}

func releaseCommandHash(plan *machineconfig.Plan) string {
	if len(plan.ReleaseCommand) == 0 {
		return ""
	}
	image := ""
	if len(plan.Groups) > 0 && len(plan.Groups[0].Containers) > 0 {
		image = plan.Groups[0].Containers[0].Image + "@" + plan.Groups[0].Containers[0].ImageDigest
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%q\x00%s", plan.ReleaseCommand, image)))
	return hex.EncodeToString(sum[:])
}

func releaseOptions(input *machineInput) (bool, string) {
	hash := releaseCommandHash(input.plan)
	if hash == "" {
		return false, ""
	}
	skip := input.state.Workloads[input.plan.Workload].Extras.ReleaseCommandHash == hash
	return skip, hash
}

func persistReleaseHash(input *machineInput, hash string) error {
	if hash == "" {
		return nil
	}
	sd, ok, err := state.LoadStateDirectory(".")
	if err != nil {
		return fmt.Errorf("failed to load existing state directory: %w", err)
	}
	if !ok {
		return fmt.Errorf("state directory does not exist, please run \"init\" first")
	}
	sd.State = *input.state
	workloadState := sd.State.Workloads[input.plan.Workload]
	workloadState.Extras.ReleaseCommandHash = hash
	sd.State.Workloads[input.plan.Workload] = workloadState
	if err := sd.Persist(); err != nil {
		return fmt.Errorf("failed to persist state file: %w", err)
	}
	return nil
}

func applyMachinePlanLive(cmd *cobra.Command, input *machineInput) error {
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
	skip, releaseHash := releaseOptions(input)
	result, err := reconcile.Apply(cmd.Context(), d, input.plan, reconcile.Options{SkipRelease: skip})
	if err != nil {
		return err
	}
	if !skip {
		if err := persistReleaseHash(input, releaseHash); err != nil {
			return err
		}
	}
	return writeMachineJSON(cmd, outputFor(input, result.Changes))
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
	Version         int                `json:"version"`
	Plan            machineconfig.Plan `json:"plan"`
	RequiredSecrets []string           `json:"required_secrets,omitempty"`
	SHA256          string             `json:"sha256"`
}

const exactMachinePlanVersion = 1

var secretNameRegexp = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	o.secretsFile, _ = cmd.Flags().GetString("secrets-file")
	return o
}

func setupMachineFlags(cmd *cobra.Command, includeYes bool) {
	cmd.Flags().String("overrides-file", "", "optional Score overrides file")
	cmd.Flags().StringArray("override-property", nil, "Score path=value override")
	cmd.Flags().String("image", "", "image to use for containers with image '.'")
	cmd.Flags().String("environment", "", "optional caller-defined deployment environment label")
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
		requiredSecrets := slices.Sorted(maps.Keys(input.secrets))
		exactPlan := exactMachinePlanFile{Version: exactMachinePlanVersion, Plan: *input.plan, RequiredSecrets: requiredSecrets}
		exactPlan.SHA256, err = exactMachinePlanChecksum(exactPlan)
		if err != nil {
			return err
		}
		artifact, marshalErr := json.MarshalIndent(exactPlan, "", "  ")
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
	if options.planFile == "" && options.secretsFile != "" {
		return fmt.Errorf("--secrets-file requires --plan-file")
	}

	var exactPlan *exactMachinePlanFile
	var exactSecrets map[string]string
	if options.planFile != "" {
		var err error
		exactPlan, exactSecrets, err = loadExactMachinePlan(options.planFile, options.secretsFile)
		if err != nil {
			return err
		}
	}

	input, err := loadMachineInput(cmd, args[0], options, exactPlan == nil)
	if err != nil {
		return err
	}
	if exactPlan != nil {
		if exactPlan.Plan.AppName != input.plan.AppName || exactPlan.Plan.Workload != input.plan.Workload {
			return fmt.Errorf("plan file targets app %q workload %q but score file targets app %q workload %q", exactPlan.Plan.AppName, exactPlan.Plan.Workload, input.plan.AppName, input.plan.Workload)
		}
		input.plan = &exactPlan.Plan
		input.secrets = exactSecrets
		if err := persistMachineState(input.state); err != nil {
			return err
		}
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
	return applyMachinePlanLive(cmd, input)
}

func exactMachinePlanChecksum(artifact exactMachinePlanFile) (string, error) {
	payload := struct {
		Version         int                `json:"version"`
		Plan            machineconfig.Plan `json:"plan"`
		RequiredSecrets []string           `json:"required_secrets,omitempty"`
	}{Version: artifact.Version, Plan: artifact.Plan, RequiredSecrets: artifact.RequiredSecrets}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode plan checksum: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func loadExactMachinePlan(planFile, secretsFile string) (*exactMachinePlanFile, map[string]string, error) {
	raw, err := os.ReadFile(planFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read plan: %w", err)
	}
	var artifact exactMachinePlanFile
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return nil, nil, fmt.Errorf("decode plan: %w", err)
	}
	if artifact.Version != exactMachinePlanVersion {
		return nil, nil, fmt.Errorf("unsupported plan file version %d; regenerate the plan", artifact.Version)
	}
	wantChecksum, err := exactMachinePlanChecksum(artifact)
	if err != nil {
		return nil, nil, err
	}
	if artifact.SHA256 != wantChecksum {
		return nil, nil, fmt.Errorf("plan integrity check failed")
	}
	if err := validateRequiredSecretNames(artifact.RequiredSecrets); err != nil {
		return nil, nil, fmt.Errorf("invalid plan: %w", err)
	}
	if err := artifact.Plan.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid plan: %w", err)
	}
	if err := artifact.Plan.ValidateImmutableImages(); err != nil {
		return nil, nil, err
	}
	if len(artifact.RequiredSecrets) > 0 && secretsFile == "" {
		return nil, nil, fmt.Errorf("plan requires secrets %s; provide --secrets-file", strings.Join(artifact.RequiredSecrets, ", "))
	}
	secrets := map[string]string{}
	if secretsFile != "" {
		secrets, err = readMachineSecretsFile(secretsFile)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := validateExactPlanSecrets(artifact.RequiredSecrets, secrets); err != nil {
		return nil, nil, err
	}
	return &artifact, secrets, nil
}

func validateRequiredSecretNames(names []string) error {
	for i, name := range names {
		if !secretNameRegexp.MatchString(name) {
			return fmt.Errorf("required secret name %q is invalid", name)
		}
		if i > 0 && name <= names[i-1] {
			return fmt.Errorf("required secret names must be sorted and unique")
		}
	}
	return nil
}

func readMachineSecretsFile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secrets file must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("secrets file permissions must be 0600 or more restrictive")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	secrets := make(map[string]string)
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for lineNumber := 0; lineNumber < len(lines); lineNumber++ {
		line := lines[lineNumber]
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !secretNameRegexp.MatchString(key) {
			return nil, fmt.Errorf("secrets file line %d must be KEY=VALUE with a valid key", lineNumber+1)
		}
		if strings.HasPrefix(value, `"""`) {
			value = strings.TrimPrefix(value, `"""`)
			for !strings.HasSuffix(value, `"""`) {
				lineNumber++
				if lineNumber >= len(lines) {
					return nil, fmt.Errorf("secrets file value for %q has an unterminated triple quote", key)
				}
				value += "\n" + lines[lineNumber]
			}
			value = strings.TrimSuffix(value, `"""`)
		}
		if _, duplicate := secrets[key]; duplicate {
			return nil, fmt.Errorf("secrets file contains duplicate key %q", key)
		}
		secrets[key] = value
	}
	return secrets, nil
}

func validateExactPlanSecrets(required []string, supplied map[string]string) error {
	requiredSet := make(map[string]struct{}, len(required))
	for _, name := range required {
		requiredSet[name] = struct{}{}
	}
	missing := make([]string, 0)
	for _, name := range required {
		if _, ok := supplied[name]; !ok {
			missing = append(missing, name)
		}
	}
	unexpected := make([]string, 0)
	for name := range supplied {
		if _, ok := requiredSet[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	slices.Sort(unexpected)
	if len(missing) > 0 || len(unexpected) > 0 {
		return fmt.Errorf("secrets file keys do not match plan (missing: %v, unexpected: %v)", missing, unexpected)
	}
	return nil
}

func persistMachineState(current *state.State) error {
	sd, ok, err := state.LoadStateDirectory(".")
	if err != nil {
		return fmt.Errorf("failed to load existing state directory: %w", err)
	}
	if !ok {
		return fmt.Errorf("state directory does not exist, please run \"init\" first")
	}
	sd.State = *current
	if err := sd.Persist(); err != nil {
		return fmt.Errorf("failed to persist state file: %w", err)
	}
	return nil
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
	d := deployer.New(client, input.plan.AppName)
	machines, err := d.ListMachines(cmd.Context())
	if err != nil {
		return err
	}
	type machineEventOutput struct {
		ID        string `json:"id,omitempty"`
		Type      string `json:"type,omitempty"`
		Timestamp *int   `json:"timestamp,omitempty"`
	}
	type machineStatus struct {
		ID           string               `json:"id"`
		Name         string               `json:"name,omitempty"`
		Group        string               `json:"group,omitempty"`
		State        string               `json:"state,omitempty"`
		Region       string               `json:"region,omitempty"`
		LastEvents   []machineEventOutput `json:"last_events,omitempty"`
		FailedChecks int                  `json:"failed_checks,omitempty"`
		Exits        int                  `json:"exits,omitempty"`
	}
	result := make([]machineStatus, 0, len(machines))
	for _, machine := range machines {
		group := ""
		if machine.Config != nil && machine.Config.Metadata != nil {
			group = (*machine.Config.Metadata)[planner.MetadataGroup]
		}
		status := machineStatus{ID: value(machine.Id), Name: value(machine.Name), Group: group, State: value(machine.State), Region: value(machine.Region)}
		if group != "" {
			events, eventsErr := d.ListEvents(cmd.Context(), value(machine.Id))
			if eventsErr != nil {
				return eventsErr
			}
			sort.SliceStable(events, func(i, j int) bool {
				if events[i].Timestamp == nil {
					return false
				}
				if events[j].Timestamp == nil {
					return true
				}
				return *events[i].Timestamp > *events[j].Timestamp
			})
			for i := 0; i < len(events) && i < 3; i++ {
				status.LastEvents = append(status.LastEvents, machineEventOutput{ID: value(events[i].Id), Type: value(events[i].Type), Timestamp: events[i].Timestamp})
			}
			for _, event := range events {
				if value(event.Type) == "exit" {
					status.Exits++
				}
			}
		}
		if machine.Checks != nil {
			for _, check := range *machine.Checks {
				if check.Status != nil && *check.Status != "" && *check.Status != "passing" {
					status.FailedChecks++
				}
			}
		}
		result = append(result, status)
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
	keys := slices.Sorted(maps.Keys(secrets))
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s=%s", key, secrets[key]))
	}
	args := []string{"secrets", "import", "--app", app, "--stage"}
	if err := execFlyWithInput(args, token, strings.Join(lines, "\n")+"\n", cmd.ErrOrStderr(), cmd.ErrOrStderr()); err != nil {
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
	return applyMachinePlanLive(cmd, input)
}

func managedMachineTargets(machines []flymachines.Machine, group string, machineID string) ([]flymachines.Machine, error) {
	var targets []flymachines.Machine
	for _, machine := range machines {
		if machine.Config == nil || machine.Config.Metadata == nil || (*machine.Config.Metadata)[planner.MetadataGroup] == "" {
			continue
		}
		if group != "" && (*machine.Config.Metadata)[planner.MetadataGroup] != group {
			continue
		}
		if machineID != "" && value(machine.Id) != machineID {
			continue
		}
		targets = append(targets, machine)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no managed machines matched the given filters")
	}
	return targets, nil
}

func runMachineLogs(cmd *cobra.Command, args []string) error {
	if machineOptions(cmd).dryRun {
		return fmt.Errorf("--dry-run is only supported by plan and validate")
	}
	machineFlag, _ := cmd.Flags().GetString("machine")
	groupFlag, _ := cmd.Flags().GetString("group")
	input, err := loadMachineInput(cmd, args[0], machineOptions(cmd), false)
	if err != nil {
		return err
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
	targets, err := managedMachineTargets(machines, groupFlag, machineFlag)
	if err != nil {
		return err
	}
	for _, machine := range targets {
		flyArgs := []string{"logs", "--access-token", client.ApiToken, "--app", input.plan.AppName, "--machine", value(machine.Id)}
		if err := execFly(flyArgs, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
			return fmt.Errorf("failed to stream logs for machine %s: %w", value(machine.Id), err)
		}
	}
	return nil
}

func runMachineScale(cmd *cobra.Command, args []string) error {
	if machineOptions(cmd).dryRun {
		return fmt.Errorf("--dry-run is only supported by plan and validate")
	}
	group, _ := cmd.Flags().GetString("group")
	minimum, _ := cmd.Flags().GetInt("min")
	maximum, _ := cmd.Flags().GetInt("max")
	if minimum < 0 {
		return fmt.Errorf("scale min must not be negative")
	}
	if maximum < minimum {
		return fmt.Errorf("scale max %d must be at least min %d", maximum, minimum)
	}
	input, err := loadMachineInput(cmd, args[0], machineOptions(cmd), false)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(input.plan.Groups, func(candidate machineconfig.Group) bool { return candidate.Name == group }) {
		return fmt.Errorf("unknown machine group %q", group)
	}
	sd, ok, err := state.LoadStateDirectory(".")
	if err != nil {
		return fmt.Errorf("failed to load existing state directory: %w", err)
	}
	if !ok {
		return fmt.Errorf("state directory does not exist, please run \"init\" first")
	}
	sd.State = *input.state
	workloadState := sd.State.Workloads[input.plan.Workload]
	if workloadState.Extras.ScaleOverrides == nil {
		workloadState.Extras.ScaleOverrides = map[string]state.ScaleRange{}
	}
	workloadState.Extras.ScaleOverrides[group] = state.ScaleRange{Min: minimum, Max: maximum}
	sd.State.Workloads[input.plan.Workload] = workloadState
	if err := sd.Persist(); err != nil {
		return fmt.Errorf("failed to persist state file: %w", err)
	}
	if apply, _ := cmd.Flags().GetBool("apply"); apply {
		return runMachineReconcile(cmd, args)
	}
	return writeMachineJSON(cmd, map[string]any{"group": group, "min": minimum, "max": maximum, "message": "scale override saved; run apply to change live machines"})
}

func runMachineLifecycle(cmd *cobra.Command, args []string, label string, action func(*deployer.Deployer, context.Context, string) error) error {
	if machineOptions(cmd).dryRun {
		return fmt.Errorf("--dry-run is only supported by plan and validate")
	}
	groupFlag, _ := cmd.Flags().GetString("group")
	input, err := loadMachineInput(cmd, args[0], machineOptions(cmd), false)
	if err != nil {
		return err
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
	targets, err := managedMachineTargets(machines, groupFlag, "")
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(targets))
	for _, machine := range targets {
		if err := action(d, cmd.Context(), value(machine.Id)); err != nil {
			return err
		}
		ids = append(ids, value(machine.Id))
	}
	return writeMachineJSON(cmd, map[string]any{label: ids})
}

func runMachineSuspend(cmd *cobra.Command, args []string) error {
	return runMachineLifecycle(cmd, args, "suspended", (*deployer.Deployer).SuspendMachine)
}

func runMachineResume(cmd *cobra.Command, args []string) error {
	return runMachineLifecycle(cmd, args, "resumed", (*deployer.Deployer).ResumeMachine)
}

func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func newMachineCommand(use string, short string, run func(*cobra.Command, []string) error, includeYes bool) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(1), SilenceUsage: true, RunE: run}
	setupMachineFlags(cmd, includeYes)
	return cmd
}

func init() {
	validate := newMachineCommand("validate SCORE_FILE", "Validate a Score workload and its Machines plan offline", runMachineValidate, false)
	plan := newMachineCommand("plan SCORE_FILE", "Show the Machines plan and pending changes for a workload", runMachinePlan, false)
	apply := newMachineCommand("apply SCORE_FILE", "Deploy a workload plan via the Fly Machines API", runMachineApply, false)
	apply.Flags().String("secrets-file", "", "read exact-plan runtime secrets from a permission-restricted KEY=VALUE file")
	status := newMachineCommand("status SCORE_FILE", "Report live machines, events, checks, and exits", runMachineStatus, false)
	reconcileCmd := newMachineCommand("reconcile SCORE_FILE", "Re-apply the desired Machines state", runMachineReconcile, false)
	destroy := newMachineCommand("destroy SCORE_FILE", "Delete managed machines and the Fly app", runMachineDestroy, true)
	logs := newMachineCommand("logs SCORE_FILE", "Stream logs for managed machines", runMachineLogs, false)
	logs.Flags().String("machine", "", "target a single machine by id")
	logs.Flags().String("group", "", "target machines in one machine group")
	scale := newMachineCommand("scale SCORE_FILE", "Set machine group scale bounds", runMachineScale, false)
	scale.Flags().String("group", "", "machine group to scale")
	scale.Flags().Int("min", 0, "minimum machine count")
	scale.Flags().Int("max", 0, "maximum machine count")
	scale.Flags().Bool("apply", false, "immediately reconcile live machines")
	_ = scale.MarkFlagRequired("group")
	_ = scale.MarkFlagRequired("min")
	_ = scale.MarkFlagRequired("max")
	suspend := newMachineCommand("suspend SCORE_FILE", "Suspend managed machines (scale to zero)", runMachineSuspend, false)
	suspend.Flags().String("group", "", "target machines in one machine group")
	resume := newMachineCommand("resume SCORE_FILE", "Resume suspended managed machines", runMachineResume, false)
	resume.Flags().String("group", "", "target machines in one machine group")
	rootCmd.AddCommand(validate, plan, apply, status, reconcileCmd, destroy, logs, scale, suspend, resume)
}

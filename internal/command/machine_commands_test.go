package command

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/astromechza/score-flyio/internal/machineconfig"
	"github.com/astromechza/score-flyio/internal/planner"
	"github.com/astromechza/score-flyio/internal/state"
)

func TestMachinePlanIsDeterministicAndDoesNotExposeEnvironmentValues(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("a", 64))

	first, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"plan", workloadFile})

	assert.NoError(t, err)

	second, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"plan", workloadFile})

	assert.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Contains(t, first, `"workload": "gateway"`)
	assert.Contains(t, first, `"machine_groups"`)
	assert.Contains(t, first, `"image_digests"`)
	assert.Contains(t, first, `"scale"`)
	assert.Contains(t, first, `"requires_replacement": false`)
	assert.NotContains(t, first, "super-secret-runtime-value")
	assert.NotContains(t, first, "ordinary-environment-value")
	assert.NotContains(t, first, `"env"`)
	assert.NotContains(t, first, `"environment"`)
}

func TestMachineApplyValidatesImmutableImagesBeforeCallingHook(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, "not-a-digest")
	previousHooks := machineHooks
	machineHooks.apply = func(context.Context, *machineconfig.Plan, []planner.MachineChange, map[string]string) error {
		t.Fatal("apply hook must not run for a mutable image")
		return nil
	}
	t.Cleanup(func() { machineHooks = previousHooks })

	_, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile})

	assert.ErrorContains(t, err, "must be pinned to an immutable sha256 digest")
}

func TestMachineApplyPassesTheGeneratedPlanAndSecretsOnlyToHook(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("b", 64))
	previousHooks := machineHooks
	var received *machineconfig.Plan
	var receivedChanges []planner.MachineChange
	machineHooks.apply = func(_ context.Context, plan *machineconfig.Plan, changes []planner.MachineChange, secrets map[string]string) error {
		received = plan
		receivedChanges = changes
		assert.Empty(t, secrets)
		return nil
	}
	t.Cleanup(func() { machineHooks = previousHooks })

	_, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile})

	assert.NoError(t, err)
	if !assert.NotNil(t, received) {
		return
	}
	assert.Equal(t, "sha256:"+strings.Repeat("b", 64), received.Groups[0].Containers[0].ImageDigest)
	if !assert.Len(t, receivedChanges, 1) {
		return
	}
	assert.Equal(t, planner.ActionCreate, receivedChanges[0].Action)
}

func TestMachineLifecycleHooksAndMissingClientError(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("c", 64))

	_, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"status", workloadFile})

	assert.EqualError(t, err, "fly client is not configured for machine status")

	previousHooks := machineHooks
	var reconciled, destroyed bool
	machineHooks.status = func(_ context.Context, _ *machineconfig.Plan, out io.Writer) error {
		_, err := fmt.Fprintln(out, "configured")
		return err
	}
	machineHooks.reconcile = func(_ context.Context, _ *machineconfig.Plan, _ []planner.MachineChange, _ map[string]string) error {
		reconciled = true
		return nil
	}
	machineHooks.destroy = func(_ context.Context, _ *machineconfig.Plan) error {
		destroyed = true
		return nil
	}
	t.Cleanup(func() { machineHooks = previousHooks })

	stdout, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"status", workloadFile})

	assert.NoError(t, err)
	assert.Equal(t, "configured\n", stdout)

	_, _, err = executeAndResetCommand(context.Background(), rootCmd, []string{"reconcile", workloadFile})

	assert.NoError(t, err)

	_, _, err = executeAndResetCommand(context.Background(), rootCmd, []string{"destroy", "--yes", workloadFile})

	assert.NoError(t, err)
	assert.True(t, reconciled)
	assert.True(t, destroyed)
}

func TestLegacyAndMachineCommandsAreRegistered(t *testing.T) {
	for _, name := range []string{"init", "generate", "provisioners", "validate", "plan", "apply", "status", "reconcile", "destroy"} {
		cmd, _, err := rootCmd.Find([]string{name})

		assert.NoError(t, err)
		assert.Equal(t, name, cmd.Name())
	}
}

func writeMachineCommandFixture(t *testing.T, digest string) string {
	t.Helper()
	directory := t.TempDir()
	t.Chdir(directory)

	sd := &state.StateDirectory{
		Path: state.DefaultRelativeStateDirectory,
		State: state.State{
			Extras:      state.StateExtras{AppPrefix: "score-"},
			SharedState: map[string]interface{}{state.SharedStateAppPrefixKey: "score-"},
		},
	}
	if !assert.NoError(t, sd.Persist()) {
		t.FailNow()
	}

	workloadFile := "score.yaml"
	content := fmt.Sprintf(`apiVersion: score.dev/v1b1
metadata:
  name: gateway
  progresify:
    processes:
      api: {}
containers:
  api:
    image: registry.example/gateway@sha256:%s
    variables:
      RUNTIME_SECRET: super-secret-runtime-value
      PUBLIC_VALUE: ordinary-environment-value
`, digest)
	if !assert.NoError(t, os.WriteFile(workloadFile, []byte(content), 0600)) {
		t.FailNow()
	}
	return workloadFile
}

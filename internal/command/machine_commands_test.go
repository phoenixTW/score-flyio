package command

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"

	"github.com/astromechza/score-flyio/pkg/flydeploy/machineconfig"
	"github.com/astromechza/score-flyio/pkg/flydeploy/planner"
	"github.com/astromechza/score-flyio/pkg/flymachines"
	"github.com/astromechza/score-flyio/pkg/state"
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
	for _, name := range []string{"init", "generate", "provisioners", "validate", "plan", "apply", "status", "reconcile", "destroy", "logs", "scale", "suspend", "resume"} {
		cmd, _, err := rootCmd.Find([]string{name})

		assert.NoError(t, err)
		assert.Equal(t, name, cmd.Name())
	}
}

func useFakeMachinesClient(t *testing.T, server *httptest.Server) {
	t.Helper()
	client, err := flymachines.NewClientWithResponses(server.URL)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	flyClient := &flymachines.FlyClient{ClientWithResponsesInterface: client, ApiToken: "fake-token"}
	previousClient := newMachinesClient
	newMachinesClient = func() (*flymachines.FlyClient, error) { return flyClient, nil }
	t.Cleanup(func() { newMachinesClient = previousClient })
}

func TestMachineScalePersistsOverrideAndAffectsPlan(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("d", 64))

	stdout, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"scale", workloadFile, "--group", "api", "--min", "2", "--max", "3"})

	assert.NoError(t, err)
	assert.Contains(t, stdout, `"group": "api"`)
	assert.Contains(t, stdout, `"min": 2`)
	assert.Contains(t, stdout, `"max": 3`)
	assert.Contains(t, stdout, "run apply to change live machines")

	planOutput, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"plan", workloadFile})

	assert.NoError(t, err)
	assert.Contains(t, planOutput, `"min": 2`)
	assert.Contains(t, planOutput, `"max": 3`)

	_, _, err = executeAndResetCommand(context.Background(), rootCmd, []string{"scale", workloadFile, "--group", "nope", "--min", "1", "--max", "1"})

	assert.EqualError(t, err, `unknown machine group "nope"`)

	_, _, err = executeAndResetCommand(context.Background(), rootCmd, []string{"scale", workloadFile, "--group", "api", "--min", "3", "--max", "1"})

	assert.EqualError(t, err, "scale max 1 must be at least min 3")
}

func TestMachineScaleOverrideSurvivesSubsequentLoads(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("e", 64))

	_, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"scale", workloadFile, "--group", "api", "--min", "0", "--max", "1"})

	assert.NoError(t, err)

	previousHooks := machineHooks
	var received *machineconfig.Plan
	machineHooks.apply = func(_ context.Context, plan *machineconfig.Plan, _ []planner.MachineChange, _ map[string]string) error {
		received = plan
		return nil
	}
	t.Cleanup(func() { machineHooks = previousHooks })

	stdout, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile})

	assert.NoError(t, err)
	assert.Contains(t, stdout, `"min": 0`)
	assert.Contains(t, stdout, `"max": 1`)
	if assert.NotNil(t, received) {
		assert.Equal(t, 0, received.Groups[0].MinMachines)
		assert.Equal(t, 1, received.Groups[0].MaxMachines)
	}
}

func TestMachineSuspendAndResumeTargetManagedMachines(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("f", 64))
	var called []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apps/score-gateway/machines":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":"m1","name":"n1","state":"started","region":"ams","config":{"metadata":{"flydeploy.group":"api"}}},
				{"id":"m2","name":"n2","state":"started","region":"ams","config":{"metadata":{"flydeploy.group":"api"}}},
				{"id":"m3","name":"n3","state":"started","region":"ams","config":{"metadata":{}}}
			]`))
		case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/suspend") || strings.HasSuffix(r.URL.Path, "/start")):
			called = append(called, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	useFakeMachinesClient(t, server)

	stdout, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"suspend", workloadFile, "--group", "api"})

	assert.NoError(t, err)
	assert.Contains(t, stdout, `"suspended": [`)
	assert.Contains(t, stdout, `"m1"`)
	assert.Contains(t, stdout, `"m2"`)
	assert.NotContains(t, stdout, `"m3"`)
	assert.Equal(t, []string{"POST /apps/score-gateway/machines/m1/suspend", "POST /apps/score-gateway/machines/m2/suspend"}, called)

	called = nil
	stdout, _, err = executeAndResetCommand(context.Background(), rootCmd, []string{"resume", workloadFile})

	assert.NoError(t, err)
	assert.Contains(t, stdout, `"resumed": [`)
	assert.Equal(t, []string{"POST /apps/score-gateway/machines/m1/start", "POST /apps/score-gateway/machines/m2/start"}, called)

	_, _, err = executeAndResetCommand(context.Background(), rootCmd, []string{"suspend", workloadFile, "--group", "worker"})

	assert.EqualError(t, err, "no managed machines matched the given filters")
}

func TestMachineStatusSurfacesEventsExitsAndFailedChecks(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("1", 64))
	var eventsRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apps/score-gateway/machines":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":"m1","name":"n1","state":"started","region":"ams","checks":[{"name":"api-ready","status":"passing"},{"name":"api-live","status":"failing"}],"config":{"metadata":{"flydeploy.group":"api"}}},
				{"id":"m2","name":"n2","state":"started","region":"ams","config":{"metadata":{}}}
			]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			eventsRequests++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":"e4","type":"exit","timestamp":5},
				{"id":"e1","type":"exit","timestamp":30},
				{"id":"e2","type":"start","timestamp":20},
				{"id":"e3","type":"stop","timestamp":10}
			]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	useFakeMachinesClient(t, server)

	stdout, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"status", workloadFile})

	assert.NoError(t, err)
	assert.Equal(t, 1, eventsRequests)
	assert.Contains(t, stdout, `"failed_checks": 1`)
	assert.Contains(t, stdout, `"exits": 2`)
	assert.Contains(t, stdout, `"last_events"`)
	assert.Contains(t, stdout, `"id": "e1"`)
	assert.Contains(t, stdout, `"id": "e2"`)
	assert.Contains(t, stdout, `"id": "e3"`)
	assert.NotContains(t, stdout, `"id": "e4"`)
}

func TestMachineLogsExecsFlyPerTargetMachine(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("2", 64))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/apps/score-gateway/machines" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":"m1","state":"started","config":{"metadata":{"flydeploy.group":"api"}}},
				{"id":"m2","state":"started","config":{"metadata":{"flydeploy.group":"api"}}},
				{"id":"m3","state":"started","config":{"metadata":{}}}
			]`))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	useFakeMachinesClient(t, server)
	var calls [][]string
	previousExec := execFly
	execFly = func(args []string, stdout, stderr io.Writer) error {
		assert.NotNil(t, stdout)
		assert.NotNil(t, stderr)
		calls = append(calls, args)
		return nil
	}
	t.Cleanup(func() { execFly = previousExec })

	_, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"logs", workloadFile})

	assert.NoError(t, err)
	if assert.Len(t, calls, 2) {
		assert.Equal(t, []string{"logs", "--access-token", "fake-token", "--app", "score-gateway", "--machine", "m1"}, calls[0])
		assert.Equal(t, []string{"logs", "--access-token", "fake-token", "--app", "score-gateway", "--machine", "m2"}, calls[1])
	}

	calls = nil
	_, _, err = executeAndResetCommand(context.Background(), rootCmd, []string{"logs", workloadFile, "--machine", "m3"})

	assert.EqualError(t, err, "no managed machines matched the given filters")
	assert.Empty(t, calls)
}

func TestMachineNewCommandsRejectDryRun(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("3", 64))

	commands := [][]string{
		{"logs", workloadFile, "--dry-run"},
		{"scale", workloadFile, "--group", "api", "--min", "1", "--max", "1", "--dry-run"},
		{"suspend", workloadFile, "--dry-run"},
		{"resume", workloadFile, "--dry-run"},
	}
	for _, args := range commands {
		_, _, err := executeAndResetCommand(context.Background(), rootCmd, args)

		assert.EqualError(t, err, "--dry-run is only supported by plan and validate")
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

func TestSetMachineSecretsPipesSecretsViaStdin(t *testing.T) {
	var capturedArgs []string
	var capturedStdin string
	original := execFlyWithInput
	execFlyWithInput = func(args []string, stdin string, stdout, stderr io.Writer) error {
		capturedArgs = args
		capturedStdin = stdin
		return nil
	}
	t.Cleanup(func() { execFlyWithInput = original })

	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)

	err := setMachineSecrets(cmd, "token-value", "app-value", map[string]string{"B_PASSWORD": "hunter2", "A_PASSWORD": "s3cret"})

	assert.NoError(t, err)
	assert.Contains(t, capturedArgs, "-i")
	assert.NotContains(t, capturedArgs, "hunter2")
	assert.NotContains(t, capturedArgs, "s3cret")
	assert.Equal(t, "A_PASSWORD=s3cret\nB_PASSWORD=hunter2\n", capturedStdin)
}

func tunnelHealthPlan(environment string) *machineconfig.Plan {
	return &machineconfig.Plan{
		Environment: environment,
		Groups: []machineconfig.Group{{
			Name:       "app",
			Containers: []machineconfig.Container{{Name: "cloudflared"}},
			Metadata:   map[string]string{machineconfig.MetadataIngressHostname: "api.flowbit.work"},
		}},
	}
}

func TestRunTunnelHealthCheckSucceedsSilentlyWhenHostAnswers(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	tunnelHealthCheckURL = func(hostname string) (string, bool) { return server.URL + "/" + hostname, true }
	t.Cleanup(func() {
		tunnelHealthCheckURL = func(hostname string) (string, bool) { return "https://" + hostname, true }
	})

	cmd := &cobra.Command{}
	stdErr := &strings.Builder{}
	cmd.SetErr(stdErr)

	runTunnelHealthCheck(cmd, tunnelHealthPlan("staging"))

	assert.Equal(t, 1, requests)
	assert.Empty(t, stdErr.String())
}

func TestRunTunnelHealthCheckWarnsWhenHostIsUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()
	tunnelHealthCheckURL = func(hostname string) (string, bool) { return server.URL, true }
	t.Cleanup(func() {
		tunnelHealthCheckURL = func(hostname string) (string, bool) { return "https://" + hostname, true }
	})

	cmd := &cobra.Command{}
	stdErr := &strings.Builder{}
	cmd.SetErr(stdErr)

	runTunnelHealthCheck(cmd, tunnelHealthPlan("staging"))

	assert.Contains(t, stdErr.String(), "warning: tunnel health check failed")
}

func TestRunTunnelHealthCheckSkipsNonStagingAndMissingHostname(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	tunnelHealthCheckURL = func(hostname string) (string, bool) { return server.URL, true }
	t.Cleanup(func() {
		tunnelHealthCheckURL = func(hostname string) (string, bool) { return "https://" + hostname, true }
	})

	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)

	runTunnelHealthCheck(cmd, tunnelHealthPlan("production"))

	assert.Equal(t, 0, requests)

	noHostname := tunnelHealthPlan("staging")
	noHostname.Groups[0].Metadata = map[string]string{}

	runTunnelHealthCheck(cmd, noHostname)

	assert.Equal(t, 0, requests)
}

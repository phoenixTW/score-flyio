package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"

	"github.com/phoenixTW/score-flyio/pkg/flydeploy/deployer"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/machineconfig"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/planner"
	"github.com/phoenixTW/score-flyio/pkg/flymachines"
	"github.com/phoenixTW/score-flyio/pkg/state"
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
  fly:
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

func TestSetMachineSecretsUsesDeployerApi(t *testing.T) {
	type recordedSecretRequest struct {
		method        string
		path          string
		authorization string
		value         []int
	}
	newSecretsDeployer := func(t *testing.T, recorded *[]recordedSecretRequest, respond func(w http.ResponseWriter)) *deployer.Deployer {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body flymachines.CreateSecretRequest
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			value := []int{}
			if body.Value != nil {
				value = *body.Value
			}
			*recorded = append(*recorded, recordedSecretRequest{method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization"), value: value})
			respond(w)
		}))
		t.Cleanup(server.Close)
		client, err := flymachines.NewClientWithResponses(server.URL, flymachines.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer test-token")
			return nil
		}))
		if !assert.NoError(t, err) {
			t.FailNow()
		}
		return deployer.New(client, "score-gateway")
	}

	t.Run("posts one request per key in sorted order", func(t *testing.T) {
		var requests []recordedSecretRequest
		d := newSecretsDeployer(t, &requests, func(w http.ResponseWriter) { w.WriteHeader(http.StatusCreated) })

		err := setMachineSecrets(context.Background(), d, map[string]string{"B_PASSWORD": "hunter2", "A_PASSWORD": "s3cret"})

		assert.NoError(t, err)
		if assert.Len(t, requests, 2) {
			assert.Equal(t, "POST", requests[0].method)
			assert.Equal(t, "/apps/score-gateway/secrets/A_PASSWORD/type/string", requests[0].path)
			assert.Equal(t, "Bearer test-token", requests[0].authorization)
			assert.Equal(t, secretValueToInts("s3cret"), requests[0].value)
			assert.Equal(t, "/apps/score-gateway/secrets/B_PASSWORD/type/string", requests[1].path)
			assert.Equal(t, "Bearer test-token", requests[1].authorization)
			assert.Equal(t, secretValueToInts("hunter2"), requests[1].value)
		}
	})

	t.Run("empty map makes no requests", func(t *testing.T) {
		var requests []recordedSecretRequest
		d := newSecretsDeployer(t, &requests, func(w http.ResponseWriter) { w.WriteHeader(http.StatusCreated) })

		err := setMachineSecrets(context.Background(), d, map[string]string{})

		assert.NoError(t, err)
		assert.Empty(t, requests)
	})

	t.Run("failure names the secret and not the value", func(t *testing.T) {
		var requests []recordedSecretRequest
		d := newSecretsDeployer(t, &requests, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("rejected value hunter2"))
		})

		err := setMachineSecrets(context.Background(), d, map[string]string{"B_PASSWORD": "hunter2"})

		assert.ErrorContains(t, err, "B_PASSWORD")
		assert.ErrorContains(t, err, "500")
		assert.NotContains(t, err.Error(), "hunter2")
		assert.NotContains(t, err.Error(), "rejected value")
	})
}

func secretValueToInts(value string) []int {
	out := make([]int, len(value))
	for i := 0; i < len(value); i++ {
		out[i] = int(value[i])
	}
	return out
}

func TestMachinePlanArtifactRecordsSecretNamesWithoutValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secrets":{"token":"plan-secret-value"}}`))
	}))
	t.Cleanup(server.Close)
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("4", 64))
	raw, err := os.ReadFile(workloadFile)
	assert.NoError(t, err)
	raw = []byte(strings.Replace(string(raw), "super-secret-runtime-value", "${resources.auth.token}", 1) + "resources:\n  auth:\n    type: credentials\n")
	assert.NoError(t, os.WriteFile(workloadFile, raw, 0600))
	sd, ok, err := state.LoadStateDirectory(".")
	assert.NoError(t, err)
	assert.True(t, ok)
	sd.State.Extras.Provisioners = []state.Provisioner{{
		ProvisionerId: "test",
		ResourceType:  "credentials",
		Http:          &state.HttpProvisioner{Url: server.URL},
	}}
	assert.NoError(t, sd.Persist())

	stdout, stderr, err := executeAndResetCommand(context.Background(), rootCmd, []string{"plan", workloadFile, "--dry-run", "--plan-output", "exact-plan.json"})

	assert.NoError(t, err)
	artifact, readErr := os.ReadFile("exact-plan.json")
	assert.NoError(t, readErr)
	assert.Contains(t, string(artifact), "\"required_secrets\": [\n    \"RUNTIME_SECRET\"")
	for _, output := range []string{stdout, stderr, string(artifact)} {
		assert.NotContains(t, output, "plan-secret-value")
	}
}

func writeExactPlanFixture(t *testing.T, workloadFile string, requiredSecrets []string) string {
	t.Helper()
	input, err := loadMachineInput(&cobra.Command{}, workloadFile, machineCommandOptions{}, false)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	artifact := exactMachinePlanFile{
		Version:         exactMachinePlanVersion,
		Plan:            *input.plan,
		RequiredSecrets: requiredSecrets,
	}
	artifact.SHA256, err = exactMachinePlanChecksum(artifact)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	raw, err := json.Marshal(artifact)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	path := "machine-plan.json"
	if !assert.NoError(t, os.WriteFile(path, raw, 0600)) {
		t.FailNow()
	}
	return path
}

func TestExactPlanWithRequiredSecretsFailsBeforeMutationWithoutSecretsFile(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("1", 64))
	planFile := writeExactPlanFixture(t, workloadFile, []string{"API_TOKEN"})
	statePath := state.DefaultRelativeStateDirectory + "/" + state.FileName
	stateBefore, err := os.ReadFile(statePath)
	assert.NoError(t, err)
	previousHooks := machineHooks
	machineHooks.apply = func(context.Context, *machineconfig.Plan, []planner.MachineChange, map[string]string) error {
		t.Fatal("apply hook must not run when exact-plan secrets are missing")
		return nil
	}
	t.Cleanup(func() { machineHooks = previousHooks })

	stdout, stderr, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile, "--plan-file", planFile})

	assert.EqualError(t, err, "plan requires secrets API_TOKEN; provide --secrets-file")
	assert.NotContains(t, stdout, "super-secret-runtime-value")
	assert.NotContains(t, stderr, "super-secret-runtime-value")
	stateAfter, readErr := os.ReadFile(statePath)
	assert.NoError(t, readErr)
	assert.Equal(t, stateBefore, stateAfter)
}

func TestExactPlanLoadsOnlyRequiredSecretsFromRestrictedFile(t *testing.T) {
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("2", 64))
	planFile := writeExactPlanFixture(t, workloadFile, []string{"API_TOKEN"})
	secretsFile := "machine-secrets.env"
	assert.NoError(t, os.WriteFile(secretsFile, []byte("API_TOKEN=exact-secret-value\n"), 0600))
	previousHooks := machineHooks
	machineHooks.apply = func(_ context.Context, _ *machineconfig.Plan, _ []planner.MachineChange, secrets map[string]string) error {
		assert.Equal(t, map[string]string{"API_TOKEN": "exact-secret-value"}, secrets)
		return nil
	}
	t.Cleanup(func() { machineHooks = previousHooks })

	stdout, stderr, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile, "--plan-file", planFile, "--secrets-file", secretsFile})

	assert.NoError(t, err)
	assert.NotContains(t, stdout, "exact-secret-value")
	assert.NotContains(t, stderr, "exact-secret-value")
}

func TestExactPlanRejectsOverexposedSecretsFileBeforeMutation(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX file modes are not available")
	}
	workloadFile := writeMachineCommandFixture(t, strings.Repeat("3", 64))
	planFile := writeExactPlanFixture(t, workloadFile, []string{"API_TOKEN"})
	secretsFile := "machine-secrets.env"
	assert.NoError(t, os.WriteFile(secretsFile, []byte("API_TOKEN=exact-secret-value\n"), 0644))

	_, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile, "--plan-file", planFile, "--secrets-file", secretsFile})

	assert.EqualError(t, err, "secrets file permissions must be 0600 or more restrictive")
}

func TestReadMachineSecretsFileAcceptsGenerateMultilineFormat(t *testing.T) {
	path := t.TempDir() + "/machine-secrets.env"
	assert.NoError(t, writeSecretsFile(map[string]string{"TOKEN": "line one\nline two", "URL": "https://example.test?a=b"}, path))

	secrets, err := readMachineSecretsFile(path)

	assert.NoError(t, err)
	assert.Equal(t, map[string]string{"TOKEN": "line one\nline two", "URL": "https://example.test?a=b"}, secrets)
}

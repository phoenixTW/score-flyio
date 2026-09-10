package convert

import (
	"encoding/base64"
	"testing"

	"github.com/score-spec/score-go/framework"
	scoretypes "github.com/score-spec/score-go/types"
	"github.com/stretchr/testify/assert"

	"github.com/astromechza/score-flyio/internal"
	"github.com/astromechza/score-flyio/internal/machineconfig"
	"github.com/astromechza/score-flyio/internal/provisioners"
	"github.com/astromechza/score-flyio/internal/state"
)

func happyWorkloadSpec() scoretypes.Workload {
	progresifyMeta := map[string]any{
		"owner":            "platform",
		"secret_namespace": "ns",
		"region":           "ams",
		"ingress":          map[string]any{"type": "cloudflare", "hostname": "api.flowbit.work"},
		"variables": map[string]string{
			"LOG_LEVEL": "debug",
		},
		"processes": map[string]any{
			"api": map[string]any{
				"machine_group": "app",
				"vm":            map[string]any{"cpus": 1, "memory_mb": 512},
				"scale":         map[string]any{"min": 1, "max": 2},
				"restart":       "always",
				"concurrency":   map[string]any{"soft_limit": 10, "hard_limit": 20},
				"http_service": map[string]any{
					"internal_port":        8080,
					"auto_stop":            "stop",
					"min_machines_running": 1,
					"ports":                []any{map[string]any{"port": 443, "handlers": []any{"http"}}},
				},
				"checks": map[string]any{
					"ready": map[string]any{
						"type":             "http",
						"port":             8080,
						"method":           "get",
						"path":             "/ready",
						"interval_seconds": 10,
						"timeout_seconds":  5,
					},
				},
			},
			"cloudflared": map[string]any{
				"machine_group": "app",
				"vm":            map[string]any{"cpus": 1, "memory_mb": 512},
				"scale":         map[string]any{"min": 1, "max": 2},
				"restart":       "always",
			},
			"worker": map[string]any{
				"restart": "no",
			},
		},
	}
	return scoretypes.Workload{
		Metadata: scoretypes.WorkloadMetadata{
			"name":       "api",
			"progresify": progresifyMeta,
		},
		Containers: map[string]scoretypes.Container{
			"api": {
				Image:   "ghcr.io/progresify/api:1.2.3",
				Command: []string{"./api"},
				Args:    []string{"serve"},
				Variables: map[string]string{
					"DATABASE_URL": "${resources.db.host}",
					"API_TOKEN":    "${resources.auth.token}",
					"LOG_LEVEL":    "info",
				},
				Files: []scoretypes.ContainerFilesElem{
					{Target: "/etc/config.yaml", Content: internal.Ref("hello ${metadata.name}")},
				},
				Volumes: []scoretypes.ContainerVolumesElem{
					{Source: "data-vol", Target: "/data"},
				},
				LivenessProbe: &scoretypes.ContainerProbe{
					HttpGet: &scoretypes.HttpProbe{
						Path: "/live",
						Port: 8080,
						HttpHeaders: []scoretypes.HttpProbeHttpHeadersElem{
							{Name: "X-Probe", Value: "1"},
						},
					},
				},
				ReadinessProbe: &scoretypes.ContainerProbe{
					HttpGet: &scoretypes.HttpProbe{Path: "/healthz", Port: 8080},
				},
			},
			"cloudflared": {
				Image: "cloudflare/cloudflared:2024.1",
				Args:  []string{"tunnel", "run"},
			},
			"worker": {
				Image: "ghcr.io/progresify/worker:1.2.3",
				Variables: map[string]string{
					"QUEUE": "${resources.queue.name}",
				},
			},
		},
		Resources: map[string]scoretypes.Resource{
			"db":    {Type: "fake-db"},
			"auth":  {Type: "fake-auth"},
			"queue": {Type: "fake-queue"},
		},
	}
}

func happyState() *state.State {
	spec := happyWorkloadSpec()
	return &state.State{
		Extras: state.StateExtras{AppPrefix: "example-"},
		Workloads: map[string]framework.ScoreWorkloadState[state.WorkloadExtras]{
			"api": {Spec: spec},
		},
		Resources: map[framework.ResourceUid]framework.ScoreResourceState[state.ResourceExtras]{
			framework.NewResourceUid("api", "db", "fake-db", nil, nil): {
				Outputs: map[string]interface{}{"host": "db.internal"},
			},
			framework.NewResourceUid("api", "auth", "fake-auth", nil, nil): {
				OutputLookupFunc: func(keys ...string) (interface{}, error) {
					provisioners.MarkSecretAccessed()
					return "tok-123", nil
				},
			},
			framework.NewResourceUid("api", "queue", "fake-queue", nil, nil): {
				Outputs: map[string]interface{}{"name": "jobs"},
			},
		},
	}
}

func happyPlan() *machineconfig.Plan {
	return &machineconfig.Plan{
		AppName:         "example-api",
		RendererVersion: "0.1.0",
		Workload:        "api",
		Environment:     "staging",
		Groups: []machineconfig.Group{
			{
				Name:        "app",
				Region:      "ams",
				Guest:       &machineconfig.Guest{CpuKind: "shared", Cpus: 1, MemoryMb: 512},
				MinMachines: 1,
				MaxMachines: 2,
				Restart:     "always",
				Services: []machineconfig.Service{{
					Protocol:           "tcp",
					InternalPort:       8080,
					Ports:              []machineconfig.ServicePort{{Port: 443, Handlers: []string{"http"}}},
					AutoStop:           "stop",
					AutoStart:          true,
					MinMachinesRunning: 1,
					Concurrency:        map[string]any{"soft_limit": float64(10), "hard_limit": float64(20)},
					Checks: []machineconfig.ServiceHttpCheck{{
						Method:          "get",
						Path:            "/healthz",
						IntervalSeconds: 10,
						TimeoutSeconds:  5,
					}},
				}},
				Checks: map[string]machineconfig.Check{
					"api-ready": {
						Type:            "http",
						Port:            8080,
						Method:          "get",
						Path:            "/ready",
						IntervalSeconds: 10,
						TimeoutSeconds:  5,
					},
					"api-liveness_probe": {
						Type:    "http",
						Port:    8080,
						Method:  "get",
						Path:    "/live",
						Headers: map[string]string{"X-Probe": "1"},
					},
				},
				Volumes: []machineconfig.Volume{{Name: "data-vol"}},
				Metadata: map[string]string{
					"progresify.workload":         "api",
					"progresify.environment":      "staging",
					"progresify.group":            "app",
					"progresify.renderer-version": "0.1.0",
					"progresify.owner":            "platform",
					"progresify.secret-namespace": "ns",
				},
				Containers: []machineconfig.Container{
					{
						Name:    "api",
						Image:   "ghcr.io/progresify/api:1.2.3",
						Command: []string{"./api"},
						Args:    []string{"serve"},
						Restart: "always",
						Env: map[string]string{
							"LOG_LEVEL":    "info",
							"DATABASE_URL": "db.internal",
						},
						Files: []machineconfig.File{
							{GuestPath: "/etc/config.yaml", RawContent: base64.StdEncoding.EncodeToString([]byte("hello api"))},
						},
						Mounts: []machineconfig.Mount{{Volume: "data-vol", Path: "/data"}},
					},
					{
						Name:    "cloudflared",
						Image:   "cloudflare/cloudflared:2024.1",
						Args:    []string{"tunnel", "run"},
						Restart: "always",
						Env:     map[string]string{"LOG_LEVEL": "debug"},
					},
				},
			},
			{
				Name:        "worker",
				Region:      "ams",
				Guest:       &machineconfig.Guest{CpuKind: "shared", Cpus: 1, MemoryMb: 256},
				MinMachines: 1,
				MaxMachines: 1,
				Restart:     "no",
				Metadata: map[string]string{
					"progresify.workload":         "api",
					"progresify.environment":      "staging",
					"progresify.group":            "worker",
					"progresify.renderer-version": "0.1.0",
					"progresify.owner":            "platform",
					"progresify.secret-namespace": "ns",
				},
				Containers: []machineconfig.Container{
					{
						Name:    "worker",
						Image:   "ghcr.io/progresify/worker:1.2.3",
						Restart: "no",
						Env: map[string]string{
							"LOG_LEVEL": "debug",
							"QUEUE":     "jobs",
						},
					},
				},
			},
		},
	}
}

func happySecrets() map[string]string {
	return map[string]string{
		"API_TOKEN": "tok-123",
	}
}

func TestMachinePlanWithSecretsHappyPath(t *testing.T) {
	currentState := happyState()

	plan, secrets, err := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.NoError(t, err)
	assert.Equal(t, happyPlan(), plan)
	assert.Equal(t, happySecrets(), secrets)
}

func TestMachinePlanWithSecretsIsDeterministic(t *testing.T) {
	currentState := happyState()

	firstPlan, firstSecrets, firstErr := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")
	secondPlan, secondSecrets, secondErr := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.NoError(t, firstErr)
	assert.NoError(t, secondErr)
	assert.Equal(t, firstPlan, secondPlan)
	assert.Equal(t, firstSecrets, secondSecrets)
}

func TestMachinePlanWithSecretsWithoutProgresify(t *testing.T) {
	currentState := happyState()

	delete(currentState.Workloads["api"].Spec.Metadata, "progresify")

	plan, secrets, err := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.NoError(t, err)
	assert.Nil(t, plan)
	assert.Nil(t, secrets)
}

func TestMachinePlanWithSecretsWithNonObjectProgresify(t *testing.T) {
	currentState := happyState()

	currentState.Workloads["api"].Spec.Metadata["progresify"] = "nope"

	plan, secrets, err := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.EqualError(t, err, "metadata.progresify: must be an object")
	assert.Nil(t, plan)
	assert.Nil(t, secrets)
}

func TestMachinePlanWithSecretsWithUnknownWorkload(t *testing.T) {
	currentState := happyState()

	plan, secrets, err := MachinePlanWithSecrets(currentState, "ghost", "staging", "0.1.0")

	assert.EqualError(t, err, "workload 'ghost': does not exist")
	assert.Nil(t, plan)
	assert.Nil(t, secrets)
}

func TestMachinePlanWithSecretsRejectsProcessWithoutContainer(t *testing.T) {
	currentState := happyState()
	processes := currentState.Workloads["api"].Spec.Metadata["progresify"].(map[string]any)["processes"].(map[string]any)

	processes["ghost"] = map[string]any{"restart": "always"}

	plan, secrets, err := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.ErrorContains(t, err, "process 'ghost' has no matching Score container")
	assert.Nil(t, plan)
	assert.Nil(t, secrets)
}

func TestMachinePlanWithSecretsRejectsContainerWithoutProcess(t *testing.T) {
	currentState := happyState()
	processes := currentState.Workloads["api"].Spec.Metadata["progresify"].(map[string]any)["processes"].(map[string]any)

	delete(processes, "cloudflared")

	plan, secrets, err := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.ErrorContains(t, err, "container 'cloudflared' has no configuration in metadata.progresify.processes")
	assert.Nil(t, plan)
	assert.Nil(t, secrets)
}

func TestMachinePlanWithSecretsRejectsSecretBackedFiles(t *testing.T) {
	currentState := happyState()
	api := currentState.Workloads["api"].Spec.Containers["api"]
	api.Files = append(api.Files, scoretypes.ContainerFilesElem{
		Target:  "/run/secrets/token",
		Content: internal.Ref("${resources.auth.token}"),
	})
	currentState.Workloads["api"].Spec.Containers["api"] = api

	plan, secrets, err := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.ErrorContains(t, err, "runtime secret-backed files are not supported by machine plans")
	assert.Nil(t, plan)
	assert.Nil(t, secrets)
}

func TestMachinePlanWithSecretsRejectsLocalBuildImage(t *testing.T) {
	currentState := happyState()

	currentState.Workloads["api"].Spec.Containers["api"] = scoretypes.Container{Image: "."}

	plan, secrets, err := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.ErrorContains(t, err, "machine deployment requires a prebuilt image")
	assert.Nil(t, plan)
	assert.Nil(t, secrets)
}

func TestMachinePlanWithSecretsRejectsColocationMismatch(t *testing.T) {
	currentState := happyState()
	processes := currentState.Workloads["api"].Spec.Metadata["progresify"].(map[string]any)["processes"].(map[string]any)

	processes["cloudflared"].(map[string]any)["vm"] = map[string]any{"cpus": 2, "memory_mb": 512}

	plan, secrets, err := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.ErrorContains(t, err, "machine group 'app': process 'cloudflared' must share vm, scale, and restart with process 'api'")
	assert.Nil(t, plan)
	assert.Nil(t, secrets)
}

func TestMachinePlanWithSecretsOverridesSecretVariableWithContainerValue(t *testing.T) {
	currentState := happyState()
	progresifyMeta := currentState.Workloads["api"].Spec.Metadata["progresify"].(map[string]any)

	progresifyMeta["variables"] = map[string]string{
		"LOG_LEVEL": "debug",
		"API_TOKEN": "${resources.auth.token}",
	}
	api := currentState.Workloads["api"].Spec.Containers["api"]
	api.Variables["API_TOKEN"] = "container-value"
	currentState.Workloads["api"].Spec.Containers["api"] = api

	plan, secrets, err := MachinePlanWithSecrets(currentState, "api", "staging", "0.1.0")

	assert.NoError(t, err)
	assert.Equal(t, "container-value", plan.Groups[0].Containers[0].Env["API_TOKEN"])
	assert.Equal(t, "tok-123", secrets["API_TOKEN"])
	assert.NotContains(t, plan.Groups[0].Containers[1].Env, "API_TOKEN")
}

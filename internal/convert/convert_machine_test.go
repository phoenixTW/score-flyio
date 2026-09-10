package convert

import (
	"encoding/base64"
	"testing"

	"github.com/score-spec/score-go/framework"
	scoretypes "github.com/score-spec/score-go/types"
	"github.com/stretchr/testify/assert"

	"github.com/phoenixTW/score-flyio/pkg/flydeploy/machineconfig"
	"github.com/phoenixTW/score-flyio/pkg/state"
)

func TestMachinePlanMapsScoreAndProgresifyFields(t *testing.T) {
	currentState := machineTestState(scoretypes.Workload{
		Metadata: scoretypes.WorkloadMetadata{
			"name": "gateway",
			"progresify": map[string]any{
				"owner":            "platform",
				"slack_channel":    "#platform",
				"secret_namespace": "gateway-staging",
				"region":           "ord",
				"ingress":          map[string]any{"type": "cloudflare", "hostname": "gateway.flowbit.work"},
				"variables":        map[string]string{"LOG_LEVEL": "info"},
				"processes": map[string]any{
					"api": map[string]any{
						"machine_group": "web",
						"vm":            map[string]any{"cpus": 2, "memory_mb": 512},
						"scale":         map[string]any{"min": 1, "max": 3},
						"restart":       "always",
						"http_service": map[string]any{
							"internal_port":        8080,
							"protocol":             "tcp",
							"auto_stop":            "suspend",
							"min_machines_running": 1,
							"ports":                []any{map[string]any{"port": 443, "handlers": []any{"http", "tls"}}},
						},
						"concurrency": map[string]any{"type": "requests", "hard_limit": 25},
						"checks": map[string]any{
							"live": map[string]any{
								"type": "http", "port": 8080, "method": "get", "path": "/healthz",
								"headers": map[string]string{"X-Probe": "gateway"}, "interval_seconds": 10, "timeout_seconds": 2,
							},
						},
					},
					"worker": map[string]any{
						"vm":      map[string]any{"cpus": 1, "memory_mb": 256},
						"scale":   map[string]any{"min": 0, "max": 1},
						"restart": "on-failure",
						"checks": map[string]any{
							"tcp": map[string]any{"type": "tcp", "port": 9000, "interval_seconds": 10, "timeout_seconds": 2},
						},
					},
				},
			},
		},
		Containers: scoretypes.WorkloadContainers{
			"api": {
				Image:     "ghcr.io/example/api:1",
				Command:   []string{"./api"},
				Args:      []string{"serve"},
				Variables: scoretypes.ContainerVariables{"LOG_LEVEL": "debug", "PORT": "8080"},
				Files:     []scoretypes.ContainerFilesElem{{Target: "/etc/api/config", Content: stringRef("hello")}},
				Volumes:   []scoretypes.ContainerVolumesElem{{Source: "data", Target: "/data"}},
			},
			"worker": {Image: "ghcr.io/example/worker:2"},
		},
	})

	plan, secrets, err := MachinePlanWithSecrets(currentState, "gateway", "staging", "test")

	assert.NoError(t, err)
	assert.NotNil(t, plan)
	assert.Empty(t, secrets)
	if !assert.NoError(t, plan.Validate()) {
		return
	}

	assert.Equal(t, "score-gateway", plan.AppName)
	if !assert.Len(t, plan.Groups, 2) {
		return
	}
	assert.Equal(t, []string{"web", "worker"}, []string{plan.Groups[0].Name, plan.Groups[1].Name})

	web := plan.Groups[0]
	assert.Equal(t, "ord", web.Region)
	assert.Equal(t, &machineconfig.Guest{CpuKind: "shared", Cpus: 2, MemoryMb: 512}, web.Guest)
	assert.Equal(t, 1, web.MinMachines)
	assert.Equal(t, 3, web.MaxMachines)
	assert.Equal(t, machineconfig.RestartPolicyAlways, web.Restart)
	assert.Equal(t, "web", web.Metadata["flydeploy.group"])
	assert.Equal(t, "platform", web.Metadata["progresify.owner"])
	assert.Equal(t, "gateway-staging", web.Metadata["progresify.secret-namespace"])
	if !assert.Len(t, web.Containers, 1) {
		return
	}
	assert.Equal(t, machineconfig.Container{
		Name:    "api",
		Image:   "ghcr.io/example/api:1",
		Command: []string{"./api"},
		Args:    []string{"serve"},
		Env:     map[string]string{"LOG_LEVEL": "debug", "PORT": "8080"},
		Files:   []machineconfig.File{{GuestPath: "/etc/api/config", RawContent: base64.StdEncoding.EncodeToString([]byte("hello"))}},
		Mounts:  []machineconfig.Mount{{Volume: "data", Path: "/data"}},
		Restart: machineconfig.RestartPolicyAlways,
	}, web.Containers[0])
	assert.Equal(t, []machineconfig.Volume{{Name: "data"}}, web.Volumes)
	if !assert.Len(t, web.Services, 1) {
		return
	}
	assert.Equal(t, machineconfig.Service{
		Protocol:           "tcp",
		InternalPort:       8080,
		Ports:              []machineconfig.ServicePort{{Port: 443, Handlers: []string{"http", "tls"}}},
		AutoStop:           machineconfig.AutoStopSuspend,
		AutoStart:          true,
		MinMachinesRunning: 1,
		Concurrency:        map[string]any{"type": "requests", "hard_limit": float64(25)},
	}, web.Services[0])
	assert.Equal(t, machineconfig.Check{
		Type: "http", Port: 8080, Method: "get", Path: "/healthz", Headers: map[string]string{"X-Probe": "gateway"}, IntervalSeconds: 10, TimeoutSeconds: 2,
	}, web.Checks["api-live"])

	worker := plan.Groups[1]
	assert.Equal(t, "ord", worker.Region)
	assert.Equal(t, machineconfig.RestartPolicyOnFailure, worker.Restart)
	assert.Equal(t, &machineconfig.Guest{CpuKind: "shared", Cpus: 1, MemoryMb: 256}, worker.Guest)
	assert.Equal(t, map[string]machineconfig.Check{"worker-tcp": {Type: "tcp", Port: 9000, IntervalSeconds: 10, TimeoutSeconds: 2}}, worker.Checks)
}

func TestMachinePlanColocatesContainersWithDistinctImages(t *testing.T) {
	currentState := machineTestState(scoretypes.Workload{
		Metadata: scoretypes.WorkloadMetadata{
			"name": "gateway",
			"progresify": map[string]any{"processes": map[string]any{
				"api": map[string]any{
					"machine_group": "app", "vm": map[string]any{"cpus": 1, "memory_mb": 512}, "scale": map[string]any{"min": 1, "max": 2}, "restart": "always",
				},
				"cloudflared": map[string]any{
					"machine_group": "app", "vm": map[string]any{"cpus": 1, "memory_mb": 512}, "scale": map[string]any{"min": 1, "max": 2}, "restart": "always",
				},
			}},
		},
		Containers: scoretypes.WorkloadContainers{
			"api":         {Image: "ghcr.io/example/api:abc"},
			"cloudflared": {Image: "cloudflare/cloudflared:2024.10.0"},
		},
	})

	plan, secrets, err := MachinePlanWithSecrets(currentState, "gateway", "staging", "test")

	assert.NoError(t, err)
	assert.Empty(t, secrets)
	if !assert.Len(t, plan.Groups, 1) {
		return
	}
	assert.Equal(t, "app", plan.Groups[0].Name)
	assert.Equal(t, []machineconfig.Container{
		{Name: "api", Image: "ghcr.io/example/api:abc", Env: map[string]string{}, Restart: machineconfig.RestartPolicyAlways},
		{Name: "cloudflared", Image: "cloudflare/cloudflared:2024.10.0", Env: map[string]string{}, Restart: machineconfig.RestartPolicyAlways},
	}, plan.Groups[0].Containers)
}

func TestMachinePlanRejectsInvalidMetadataContainerCombinations(t *testing.T) {
	tests := []struct {
		name       string
		containers scoretypes.WorkloadContainers
		processes  map[string]any
		want       string
	}{
		{
			name:       "unconfigured container",
			containers: scoretypes.WorkloadContainers{"api": {Image: "api:1"}, "sidecar": {Image: "sidecar:1"}},
			processes:  map[string]any{"api": map[string]any{}},
			want:       "container 'sidecar' has no configuration",
		},
		{
			name:       "process without container",
			containers: scoretypes.WorkloadContainers{"api": {Image: "api:1"}},
			processes:  map[string]any{"api": map[string]any{}, "ghost": map[string]any{}},
			want:       "process 'ghost' has no matching Score container",
		},
		{
			name:       "inconsistent colocated group",
			containers: scoretypes.WorkloadContainers{"api": {Image: "api:1"}, "sidecar": {Image: "sidecar:1"}},
			processes: map[string]any{
				"api":     map[string]any{"machine_group": "app", "vm": map[string]any{"cpus": 1, "memory_mb": 256}},
				"sidecar": map[string]any{"machine_group": "app", "vm": map[string]any{"cpus": 2, "memory_mb": 256}},
			},
			want: "machine group 'app': process 'sidecar' must share vm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			currentState := machineTestState(scoretypes.Workload{
				Metadata: scoretypes.WorkloadMetadata{
					"name":       "gateway",
					"progresify": map[string]any{"processes": tt.processes},
				},
				Containers: tt.containers,
			})
			plan, _, err := MachinePlanWithSecrets(currentState, "gateway", "staging", "test")

			assert.Nil(t, plan)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestMachinePlanIsAbsentWithoutProgresifyMetadataAndLegacyConversionStillWorks(t *testing.T) {
	currentState := machineTestState(scoretypes.Workload{
		Metadata:   scoretypes.WorkloadMetadata{"name": "gateway"},
		Containers: scoretypes.WorkloadContainers{"api": {Image: "ghcr.io/example/api:1"}},
	})

	plan, secrets, err := MachinePlanWithSecrets(currentState, "gateway", "staging", "test")

	assert.NoError(t, err)
	assert.Nil(t, plan)
	assert.Empty(t, secrets)

	legacy, secrets, err := Workload(currentState, "gateway")

	assert.NoError(t, err)
	assert.Empty(t, secrets)
	assert.Equal(t, "score-gateway", legacy.AppName)
	assert.Equal(t, "ghcr.io/example/api:1", legacy.Build.Image)
}

func machineTestState(workload scoretypes.Workload) *state.State {
	return &state.State{
		Extras: state.StateExtras{AppPrefix: "score-"},
		Workloads: map[string]framework.ScoreWorkloadState[state.WorkloadExtras]{
			"gateway": {Spec: workload},
		},
	}
}

func stringRef(value string) *string {
	return &value
}

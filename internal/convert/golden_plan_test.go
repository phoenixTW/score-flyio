package convert

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/score-spec/score-go/framework"
	scoretypes "github.com/score-spec/score-go/types"
	"github.com/stretchr/testify/assert"

	"github.com/phoenixTW/score-flyio/pkg/state"
)

var goldenUpdate = flag.Bool("update", false, "rewrite golden fixture files")

func colocatedServiceState() *state.State {
	flyMeta := map[string]any{
		"region":  "ams",
		"ingress": map[string]any{"type": "public", "hostname": "gateway.example.test"},
		"processes": map[string]any{
			"web": map[string]any{
				"machine_group": "frontend",
				"vm":            map[string]any{"cpus": 1, "memory_mb": 512},
				"scale":         map[string]any{"min": 1, "max": 3},
				"restart":       "always",
				"http_service": map[string]any{
					"internal_port": 8080,
					"ports":         []any{map[string]any{"port": 443, "handlers": []any{"http"}}},
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
			"sidecar": map[string]any{
				"machine_group": "frontend",
				"vm":            map[string]any{"cpus": 1, "memory_mb": 512},
				"scale":         map[string]any{"min": 1, "max": 3},
				"restart":       "always",
			},
		},
	}
	spec := scoretypes.Workload{
		Metadata: scoretypes.WorkloadMetadata{"name": "gateway", "fly": flyMeta},
		Containers: map[string]scoretypes.Container{
			"web": {
				Image: "registry.example/gateway/web@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			},
			"sidecar": {
				Image: "registry.example/gateway/sidecar@sha256:4444444444444444444444444444444444444444444444444444444444444444",
				Args:  []string{"tunnel", "run"},
			},
		},
	}
	return &state.State{
		Extras: state.StateExtras{AppPrefix: "example-"},
		Workloads: map[string]framework.ScoreWorkloadState[state.WorkloadExtras]{
			"gateway": {Spec: spec},
		},
	}
}

func workerOnlyState() *state.State {
	flyMeta := map[string]any{
		"region": "cdg",
		"processes": map[string]any{
			"worker": map[string]any{
				"vm":      map[string]any{"cpus": 2, "memory_mb": 1024},
				"scale":   map[string]any{"min": 3, "max": 8},
				"restart": "on-failure",
			},
		},
	}
	spec := scoretypes.Workload{
		Metadata: scoretypes.WorkloadMetadata{"name": "pipeline", "fly": flyMeta},
		Containers: map[string]scoretypes.Container{
			"worker": {
				Image:     "registry.example/pipeline/worker@sha256:9999999999999999999999999999999999999999999999999999999999999999",
				Variables: map[string]string{"QUEUE_URL": "amqp://queue.internal"},
			},
		},
	}
	return &state.State{
		Extras: state.StateExtras{AppPrefix: "example-"},
		Workloads: map[string]framework.ScoreWorkloadState[state.WorkloadExtras]{
			"pipeline": {Spec: spec},
		},
	}
}

func TestGoldenMachinePlanJson(t *testing.T) {
	cases := []struct {
		name           string
		state          func() *state.State
		workload       string
		wantSecretKeys []string
	}{
		{name: "plan", state: happyState, workload: "api", wantSecretKeys: []string{"API_TOKEN"}},
		{name: "colocated_service_plan", state: colocatedServiceState, workload: "gateway"},
		{name: "worker_plan", state: workerOnlyState, workload: "pipeline"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plan, secrets, err := MachinePlanWithSecrets(testCase.state(), testCase.workload, "staging", "0.1.0")

			assert.NoError(t, err)
			assert.NotNil(t, plan)
			for _, key := range testCase.wantSecretKeys {
				assert.Contains(t, secrets, key)
			}

			raw, err := json.MarshalIndent(plan, "", "  ")
			assert.NoError(t, err)
			raw = append(raw, '\n')

			for _, value := range secrets {
				assert.NotContains(t, string(raw), value)
			}

			goldenPath := filepath.Join("testdata", "golden", testCase.name+".json")

			if *goldenUpdate {
				assert.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0755))
				assert.NoError(t, os.WriteFile(goldenPath, raw, 0644))
				return
			}

			expected, readErr := os.ReadFile(goldenPath)
			assert.NoError(t, readErr)
			assert.Equal(t, string(expected), string(raw))
		})
	}
}

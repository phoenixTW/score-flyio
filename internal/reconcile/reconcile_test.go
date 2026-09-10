package reconcile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/astromechza/score-flyio/internal"
	"github.com/astromechza/score-flyio/internal/deployer"
	"github.com/astromechza/score-flyio/internal/flymachines"
	"github.com/astromechza/score-flyio/internal/machineconfig"
)

func TestPlanReadsLiveMachinesAndProducesNoop(t *testing.T) {
	group := machineconfig.Group{
		Name: "app", Region: "iad", MinMachines: 1, MaxMachines: 1,
		Guest:      &machineconfig.Guest{Cpus: 1, MemoryMb: 256},
		Containers: []machineconfig.Container{{Name: "api", Image: "example/api@sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("a", 64)}},
		Metadata:   map[string]string{"progresify.group": "app"},
	}
	hash, err := machineconfig.ConfigHash(&group)

	assert.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/apps/test-app/machines", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		metadata := map[string]string{"progresify.group": "app", "progresify.config-hash": hash}
		_ = json.NewEncoder(w).Encode([]flymachines.Machine{{Id: internal.Ref("m1"), Region: internal.Ref("iad"), Config: &flymachines.FlyMachineConfig{Metadata: &metadata}}})
	}))
	defer server.Close()
	client, err := flymachines.NewClientWithResponses(server.URL)

	assert.NoError(t, err)

	result, err := Plan(context.Background(), deployer.New(client, "test-app"), &machineconfig.Plan{AppName: "test-app", Workload: "api", Groups: []machineconfig.Group{group}})

	assert.NoError(t, err)
	assert.Len(t, result.Changes, 1)
	assert.Equal(t, "noop", string(result.Changes[0].Action))
}

func TestConfigWithHashDoesNotMutateGroup(t *testing.T) {
	group := machineconfig.Group{Name: "app", Metadata: map[string]string{"owner": "platform"}}

	config := configWithHash(group, "abc")

	assert.Equal(t, "platform", group.Metadata["owner"])
	assert.NotEqual(t, "abc", group.Metadata["progresify.config-hash"])
	assert.Equal(t, "abc", (*config.Metadata)["progresify.config-hash"])
}

func releaseTestGroup() machineconfig.Group {
	return machineconfig.Group{
		Name: "app", Region: "iad", MinMachines: 1, MaxMachines: 1,
		Guest:      &machineconfig.Guest{Cpus: 1, MemoryMb: 256},
		Containers: []machineconfig.Container{{Name: "api", Image: "example/api@sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("a", 64)}},
		Metadata:   map[string]string{"progresify.group": "app"},
	}
}

func releaseTestServer(t *testing.T, releaseExitCode *int) (*httptest.Server, *[]string, *int) {
	t.Helper()
	creates := make([]string, 0)
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apps/test-app/machines":
			_ = json.NewEncoder(w).Encode([]flymachines.Machine{})
		case r.Method == http.MethodPost && r.URL.Path == "/apps/test-app/machines":
			var request struct {
				Name   *string                       `json:"name"`
				Config *flymachines.FlyMachineConfig `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			isRelease := request.Config != nil && request.Config.Metadata != nil && (*request.Config.Metadata)["progresify.group"] == "release"
			if isRelease {
				creates = append(creates, "release")
				_ = json.NewEncoder(w).Encode(flymachines.Machine{Id: internal.Ref("rel-1"), Name: request.Name})
				return
			}
			creates = append(creates, "group")
			_ = json.NewEncoder(w).Encode(flymachines.Machine{Id: internal.Ref("m1"), Name: request.Name, State: internal.Ref("started")})
		case r.Method == http.MethodGet && r.URL.Path == "/apps/test-app/machines/rel-1/events":
			_ = json.NewEncoder(w).Encode([]flymachines.MachineEvent{{Type: internal.Ref("exit"), Timestamp: internal.Ref(2), Request: &map[string]any{"exit_code": float64(*releaseExitCode)}}})
		case r.Method == http.MethodGet && r.URL.Path == "/apps/test-app/machines/rel-1":
			_ = json.NewEncoder(w).Encode(flymachines.Machine{Id: internal.Ref("rel-1"), State: internal.Ref("stopped")})
		case r.Method == http.MethodDelete && r.URL.Path == "/apps/test-app/machines/rel-1":
			deletes++
			_ = json.NewEncoder(w).Encode(struct{}{})
		case r.Method == http.MethodGet && r.URL.Path == "/apps/test-app/machines/m1/wait":
			_ = json.NewEncoder(w).Encode(struct{}{})
		case r.Method == http.MethodGet && r.URL.Path == "/apps/test-app/machines/m1":
			_ = json.NewEncoder(w).Encode(flymachines.Machine{Id: internal.Ref("m1"), State: internal.Ref("started"), Config: &flymachines.FlyMachineConfig{Metadata: &map[string]string{"progresify.group": "app"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &creates, &deletes
}

func releaseTestOptions() Options {
	return Options{StateTimeout: 2 * time.Second, HealthTimeout: 2 * time.Second, HealthPoll: time.Millisecond}
}

func TestApplyRunsReleaseCommandBeforeGroupMachines(t *testing.T) {
	exitCode := 0
	server, creates, deletes := releaseTestServer(t, &exitCode)
	client, err := flymachines.NewClientWithResponses(server.URL)

	assert.NoError(t, err)

	plan := &machineconfig.Plan{AppName: "test-app", Workload: "api", ReleaseCommand: []string{"bin/migrate"}, Groups: []machineconfig.Group{releaseTestGroup()}}
	result, err := Apply(context.Background(), deployer.New(client, "test-app"), plan, releaseTestOptions())

	assert.NoError(t, err)
	assert.Equal(t, []string{"release", "group"}, *creates)
	assert.Equal(t, 1, *deletes)
	assert.Len(t, result.Changes, 1)
	assert.Equal(t, "create", string(result.Changes[0].Action))
}

func TestApplyReleaseCommandFailureAbortsBeforeGroupMachines(t *testing.T) {
	exitCode := 1
	server, creates, deletes := releaseTestServer(t, &exitCode)
	client, err := flymachines.NewClientWithResponses(server.URL)

	assert.NoError(t, err)

	plan := &machineconfig.Plan{AppName: "test-app", Workload: "api", ReleaseCommand: []string{"bin/migrate"}, Groups: []machineconfig.Group{releaseTestGroup()}}
	result, err := Apply(context.Background(), deployer.New(client, "test-app"), plan, releaseTestOptions())

	assert.Nil(t, result)
	assert.ErrorContains(t, err, "release command exited with code 1")
	assert.Equal(t, []string{"release"}, *creates)
	assert.Equal(t, 1, *deletes)
}

func TestApplySkipReleaseSkipsOneOffMachine(t *testing.T) {
	exitCode := 0
	server, creates, deletes := releaseTestServer(t, &exitCode)
	client, err := flymachines.NewClientWithResponses(server.URL)

	assert.NoError(t, err)

	options := releaseTestOptions()
	options.SkipRelease = true
	plan := &machineconfig.Plan{AppName: "test-app", Workload: "api", ReleaseCommand: []string{"bin/migrate"}, Groups: []machineconfig.Group{releaseTestGroup()}}
	result, err := Apply(context.Background(), deployer.New(client, "test-app"), plan, options)

	assert.NoError(t, err)
	assert.Equal(t, []string{"group"}, *creates)
	assert.Equal(t, 0, *deletes)
	assert.Len(t, result.Changes, 1)
}

func TestReleaseMachineConfigStripsServicesAndChecks(t *testing.T) {
	group := releaseTestGroup()
	group.Services = []machineconfig.Service{{Protocol: "tcp", InternalPort: 8080}}
	group.Checks = map[string]machineconfig.Check{"api-ready": {Type: "http", Port: 8080}}
	group.Restart = machineconfig.RestartPolicyAlways

	config := releaseMachineConfig(group)

	assert.Nil(t, config.Services)
	assert.Equal(t, "release", (*config.Metadata)["progresify.group"])
	assert.Equal(t, "no", string(internal.DerefOr(config.Restart.Policy, "")))
}

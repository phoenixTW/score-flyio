package reconcile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

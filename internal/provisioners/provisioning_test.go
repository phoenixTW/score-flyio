package provisioners

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/score-spec/score-go/framework"
	scoretypes "github.com/score-spec/score-go/types"
	"github.com/stretchr/testify/assert"

	"github.com/phoenixTW/score-flyio/pkg/state"
)

const provisioningSentinelSecret = "sentinel-secret-value"

var provisioningResourceUid = framework.ResourceUid("credentials.default#default")

func newProvisioningHttpServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func newProvisioningState(t *testing.T, serverUrl string) *state.State {
	t.Helper()
	resourceId := "default"
	return &state.State{
		Workloads: map[string]framework.ScoreWorkloadState[state.WorkloadExtras]{
			"gateway": {
				Spec: scoretypes.Workload{
					ApiVersion: "score.dev/v1b1",
					Metadata:   scoretypes.WorkloadMetadata{"name": "gateway"},
					Containers: scoretypes.WorkloadContainers{
						"api": {Image: "registry.example/app@sha256:" + strings.Repeat("a", 64)},
					},
					Resources: scoretypes.WorkloadResources{
						"auth": {Type: "credentials", Id: &resourceId},
					},
				},
			},
		},
		Resources: map[framework.ResourceUid]framework.ScoreResourceState[state.ResourceExtras]{
			provisioningResourceUid: {Guid: "guid-1", Type: "credentials", Class: "default", Id: "default", SourceWorkload: "gateway", ProvisionerUri: "test"},
		},
		SharedState: map[string]interface{}{},
		Extras: state.StateExtras{
			Provisioners: []state.Provisioner{{
				ProvisionerId: "test",
				ResourceType:  "credentials",
				Http:          &state.HttpProvisioner{Url: serverUrl},
			}},
		},
	}
}

func captureProvisionerLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := new(bytes.Buffer)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func TestProvisionResourcesDecodeFailureDoesNotLeakProvisionerOutput(t *testing.T) {
	server := newProvisioningHttpServer(t, http.StatusOK, `{"secrets":{"token":"`+provisioningSentinelSecret)
	logs := captureProvisionerLogs(t)
	input := newProvisioningState(t, server.URL)

	_, err := ProvisionResources(input)

	assert.ErrorContains(t, err, string(provisioningResourceUid))
	assert.ErrorContains(t, err, "failed to decode response from provisioner")
	assert.NotContains(t, err.Error(), provisioningSentinelSecret)
	assert.NotContains(t, logs.String(), provisioningSentinelSecret)
}

func TestDeProvisionResourceDecodeFailureDoesNotLeakProvisionerOutput(t *testing.T) {
	server := newProvisioningHttpServer(t, http.StatusOK, `{"secrets":{"token":"`+provisioningSentinelSecret)
	logs := captureProvisionerLogs(t)
	input := newProvisioningState(t, server.URL)

	_, err := DeProvisionResource(input, provisioningResourceUid)

	assert.ErrorContains(t, err, string(provisioningResourceUid))
	assert.ErrorContains(t, err, "failed to decode response from provisioner")
	assert.NotContains(t, err.Error(), provisioningSentinelSecret)
	assert.NotContains(t, logs.String(), provisioningSentinelSecret)
}

func TestProvisionResourcesHttpFailureDoesNotLeakProvisionerOutput(t *testing.T) {
	server := newProvisioningHttpServer(t, http.StatusInternalServerError, `{"values":{"token":"`+provisioningSentinelSecret+`"}}`)
	logs := captureProvisionerLogs(t)
	input := newProvisioningState(t, server.URL)

	_, err := ProvisionResources(input)

	assert.ErrorContains(t, err, "failed to provision")
	assert.ErrorContains(t, err, "500")
	assert.NotContains(t, err.Error(), provisioningSentinelSecret)
	assert.NotContains(t, logs.String(), provisioningSentinelSecret)
}

func TestProvisionResourcesResolvesResourceSecretsThroughOutputs(t *testing.T) {
	server := newProvisioningHttpServer(t, http.StatusOK, `{"secrets":{"token":"`+provisioningSentinelSecret+`"}}`)
	input := newProvisioningState(t, server.URL)

	out, err := ProvisionResources(input)

	assert.NoError(t, err)
	res := out.Resources[provisioningResourceUid]
	resolved, lookupErr := res.OutputLookupFunc("token")
	assert.NoError(t, lookupErr)
	assert.Equal(t, provisioningSentinelSecret, resolved)
}

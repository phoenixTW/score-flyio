package deployer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astromechza/score-flyio/internal"
	"github.com/astromechza/score-flyio/pkg/flymachines"
	"github.com/stretchr/testify/assert"
)

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	assert.NoError(t, json.NewEncoder(w).Encode(v))
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	assert.NoError(t, err)
	return string(raw)
}

func newTestDeployer(t *testing.T, handler http.HandlerFunc) *Deployer {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := flymachines.NewClientWithResponses(server.URL)
	assert.NoError(t, err)
	return New(client, "test-app")
}

func machineJSON(id string, state string, checks []flymachines.CheckStatus) flymachines.Machine {
	return flymachines.Machine{
		Id:     internal.Ref(id),
		Name:   internal.Ref(id + "-name"),
		State:  internal.Ref(state),
		Checks: &checks,
	}
}

func TestCreateMachineSendsExpectedPayloadAndReturnsMachine(t *testing.T) {
	var captured recordedRequest
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apps/test-app/machines" {
			http.NotFound(w, r)
			return
		}
		captured = recordedRequest{Method: r.Method, Path: r.URL.Path, Body: readBody(t, r)}
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "starting", nil))
	})
	config := flymachines.FlyMachineConfig{Image: internal.Ref("registry.example/app:v1")}
	created, err := d.CreateMachine(context.Background(), "web-1", "iad", config)
	assert.NoError(t, err)
	assert.NotNil(t, created)
	assert.Equal(t, "m1", internal.DerefOr(created.Id, ""))
	assert.Equal(t, "m1-name", internal.DerefOr(created.Name, ""))
	var body flymachines.CreateMachineRequest
	assert.NoError(t, json.Unmarshal([]byte(captured.Body), &body))
	assert.Equal(t, "web-1", internal.DerefOr(body.Name, ""))
	assert.Equal(t, "iad", internal.DerefOr(body.Region, ""))
	assert.NotNil(t, body.Config)
	assert.Equal(t, "registry.example/app:v1", internal.DerefOr(body.Config.Image, ""))
}

func TestCreateMachineWrapsServerError(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := d.CreateMachine(context.Background(), "web-1", "iad", flymachines.FlyMachineConfig{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestListMachinesReturnsAll(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apps/test-app/machines" {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, http.StatusOK, []flymachines.Machine{
			machineJSON("m1", "started", nil),
			machineJSON("m2", "stopped", nil),
		})
	})
	machines, err := d.ListMachines(context.Background())
	assert.NoError(t, err)
	assert.Len(t, machines, 2)
	assert.Equal(t, "m1", internal.DerefOr(machines[0].Id, ""))
	assert.Equal(t, "m2", internal.DerefOr(machines[1].Id, ""))
}

func TestGetMachineFoundNotFoundAndError(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apps/test-app/machines/m1":
			writeJSON(t, w, http.StatusOK, machineJSON("m1", "started", nil))
		case r.Method == http.MethodGet && r.URL.Path == "/apps/test-app/machines/gone":
			w.WriteHeader(http.StatusNotFound)
		default:
			http.Error(w, "nope", http.StatusInternalServerError)
		}
	})
	machine, found, err := d.GetMachine(context.Background(), "m1")
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "m1", internal.DerefOr(machine.Id, ""))
	machine, found, err = d.GetMachine(context.Background(), "gone")
	assert.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, machine)
	_, found, err = d.GetMachine(context.Background(), "broken")
	assert.Error(t, err)
	assert.False(t, found)
	assert.Contains(t, err.Error(), "500")
}

func TestUpdateMachineSendsCurrentVersionAndConfig(t *testing.T) {
	var captured recordedRequest
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apps/test-app/machines/m1" {
			http.NotFound(w, r)
			return
		}
		captured = recordedRequest{Method: r.Method, Path: r.URL.Path, Body: readBody(t, r)}
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "starting", nil))
	})
	config := flymachines.FlyMachineConfig{Image: internal.Ref("registry.example/app:v2")}
	updated, err := d.UpdateMachine(context.Background(), "m1", "v5", "web-1", config)
	assert.NoError(t, err)
	assert.Equal(t, "m1", internal.DerefOr(updated.Id, ""))
	var body flymachines.UpdateMachineRequest
	assert.NoError(t, json.Unmarshal([]byte(captured.Body), &body))
	assert.Equal(t, "v5", internal.DerefOr(body.CurrentVersion, ""))
	assert.Equal(t, "web-1", internal.DerefOr(body.Name, ""))
	assert.NotNil(t, body.Config)
	assert.Equal(t, "registry.example/app:v2", internal.DerefOr(body.Config.Image, ""))
}

func TestDeleteMachineRecordsForceAndHandlesErrors(t *testing.T) {
	var query atomic.Value
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/apps/test-app/machines/m1" {
			http.Error(w, "bad route", http.StatusTeapot)
			return
		}
		query.Store(r.URL.Query().Get("force"))
		w.WriteHeader(http.StatusAccepted)
	})
	assert.NoError(t, d.DeleteMachine(context.Background(), "m1", true))
	assert.Equal(t, "true", query.Load())
	assert.NoError(t, d.DeleteMachine(context.Background(), "m1", false))
	assert.Equal(t, "false", query.Load())
}

func TestDeleteMachineWrapsServerError(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	})
	err := d.DeleteMachine(context.Background(), "m1", false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestLifecycleActionsHitExpectedRoutes(t *testing.T) {
	var calls atomic.Int32
	var paths atomic.Value
	paths.Store([]string{})
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/apps/test-app/machines/m1/") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		current := paths.Load().([]string)
		paths.Store(append(current, r.URL.Path))
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "started", nil))
	})
	assert.NoError(t, d.StopMachine(context.Background(), "m1"))
	assert.NoError(t, d.SuspendMachine(context.Background(), "m1"))
	assert.NoError(t, d.RestartMachine(context.Background(), "m1"))
	assert.NoError(t, d.StartMachine(context.Background(), "m1"))
	assert.Equal(t, int32(4), calls.Load())
	assert.Equal(t, []string{
		"/apps/test-app/machines/m1/stop",
		"/apps/test-app/machines/m1/suspend",
		"/apps/test-app/machines/m1/restart",
		"/apps/test-app/machines/m1/start",
	}, paths.Load().([]string))
}

func TestLifecycleActionWrapsServerError(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "conflict", http.StatusConflict)
	})
	err := d.RestartMachine(context.Background(), "m1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "409")
}

func TestListEventsReturnsEvents(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apps/test-app/machines/m1/events" {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, http.StatusOK, []flymachines.MachineEvent{{Id: internal.Ref("e1"), Type: internal.Ref("start")}})
	})
	events, err := d.ListEvents(context.Background(), "m1")
	assert.NoError(t, err)
	assert.Len(t, events, 1)
	assert.Equal(t, "e1", internal.DerefOr(events[0].Id, ""))
}

func TestWaitExitReturnsExitCodeFromLatestExitEvent(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps/test-app/machines/m1/events" {
			writeJSON(t, w, http.StatusOK, []flymachines.MachineEvent{
				{Type: internal.Ref("exit"), Timestamp: internal.Ref(5), Request: &map[string]any{"exit_code": float64(3)}},
				{Type: internal.Ref("exit"), Timestamp: internal.Ref(9), Request: &map[string]any{"exit_code": float64(0)}},
			})
			return
		}
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "stopped", nil))
	})
	code, err := d.WaitExit(context.Background(), "m1", time.Second, time.Millisecond)
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
}

func TestWaitExitReturnsNonZeroExitCode(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps/test-app/machines/m1/events" {
			writeJSON(t, w, http.StatusOK, []flymachines.MachineEvent{{Type: internal.Ref("exit"), Request: &map[string]any{"exit_code": float64(7)}}})
			return
		}
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "destroyed", nil))
	})
	code, err := d.WaitExit(context.Background(), "m1", time.Second, time.Millisecond)
	assert.NoError(t, err)
	assert.Equal(t, 7, code)
}

func TestWaitExitPollsUntilMachineStops(t *testing.T) {
	var calls atomic.Int32
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps/test-app/machines/m1/events" {
			writeJSON(t, w, http.StatusOK, []flymachines.MachineEvent{{Type: internal.Ref("exit"), Request: &map[string]any{"exit_code": float64(0)}}})
			return
		}
		state := "starting"
		if calls.Add(1) >= 3 {
			state = "stopped"
		}
		writeJSON(t, w, http.StatusOK, machineJSON("m1", state, nil))
	})
	code, err := d.WaitExit(context.Background(), "m1", 2*time.Second, time.Millisecond)
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.GreaterOrEqual(t, calls.Load(), int32(3))
}

func TestWaitExitErrorsWithoutExitEvent(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps/test-app/machines/m1/events" {
			writeJSON(t, w, http.StatusOK, []flymachines.MachineEvent{{Type: internal.Ref("start")}})
			return
		}
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "stopped", nil))
	})
	code, err := d.WaitExit(context.Background(), "m1", time.Second, time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no exit event")
	assert.Equal(t, -1, code)
}

func TestWaitExitTimesOutWhileMachineRuns(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "started", nil))
	})
	code, err := d.WaitExit(context.Background(), "m1", 150*time.Millisecond, 10*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Equal(t, -1, code)
}

func TestWaitForStateReturnsWhenStateMatchesImmediately(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || (r.URL.Path != "/apps/test-app/machines/m1/wait" && r.URL.Path != "/apps/test-app/machines/m1") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/apps/test-app/machines/m1/wait" {
			assert.Equal(t, "started", r.URL.Query().Get("state"))
		}
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "started", nil))
	})
	assert.NoError(t, d.WaitForState(context.Background(), "m1", flymachines.MachinesWaitParamsStateStarted, time.Second))
}

func TestWaitForStateRetriesUntilStateMatches(t *testing.T) {
	var calls atomic.Int32
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		state := "starting"
		if calls.Load() >= 3 {
			state = "started"
		}
		writeJSON(t, w, http.StatusOK, machineJSON("m1", state, nil))
	})
	assert.NoError(t, d.WaitForState(context.Background(), "m1", flymachines.MachinesWaitParamsStateStarted, 2*time.Second))
	assert.GreaterOrEqual(t, calls.Load(), int32(3))
}

func TestWaitForStateTimesOutWhenStateNeverMatches(t *testing.T) {
	var calls atomic.Int32
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "starting", nil))
	})
	err := d.WaitForState(context.Background(), "m1", flymachines.MachinesWaitParamsStateStarted, 200*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Contains(t, err.Error(), "started")
	assert.GreaterOrEqual(t, calls.Load(), int32(1))
}

func TestWaitForHealthyReturnsWhenStartedAndAllChecksPassing(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "started", []flymachines.CheckStatus{
			{Name: internal.Ref("http"), Status: internal.Ref("passing")},
		}))
	})
	assert.NoError(t, d.WaitForHealthy(context.Background(), "m1", time.Second, 10*time.Millisecond))
}

func TestWaitForHealthyWithNilChecksIsHealthy(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "started", nil))
	})
	assert.NoError(t, d.WaitForHealthy(context.Background(), "m1", time.Second, 10*time.Millisecond))
}

func TestWaitForHealthyRetriesUntilChecksPass(t *testing.T) {
	var calls atomic.Int32
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		checks := []flymachines.CheckStatus{{Name: internal.Ref("http"), Status: internal.Ref("failing"), Output: internal.Ref("conn refused")}}
		if calls.Load() >= 2 {
			checks = []flymachines.CheckStatus{{Name: internal.Ref("http"), Status: internal.Ref("passing")}}
		}
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "started", checks))
	})
	assert.NoError(t, d.WaitForHealthy(context.Background(), "m1", 2*time.Second, 10*time.Millisecond))
	assert.GreaterOrEqual(t, calls.Load(), int32(2))
}

func TestWaitForHealthyTimesOutNamingFailingCheck(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "started", []flymachines.CheckStatus{
			{Name: internal.Ref("http"), Status: internal.Ref("failing"), Output: internal.Ref("conn refused")},
		}))
	})
	err := d.WaitForHealthy(context.Background(), "m1", 150*time.Millisecond, 10*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestWaitForHealthyTimesOutWhenNotStarted(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, machineJSON("m1", "starting", nil))
	})
	err := d.WaitForHealthy(context.Background(), "m1", 150*time.Millisecond, 10*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestMachinesByGroupFiltersOnMetadata(t *testing.T) {
	withGroup := flymachines.Machine{
		Id:     internal.Ref("m1"),
		Config: &flymachines.FlyMachineConfig{Metadata: internal.Ref(map[string]string{"flydeploy.group": "app"})},
	}
	nilConfig := flymachines.Machine{Id: internal.Ref("m2")}
	nilMetadata := flymachines.Machine{Id: internal.Ref("m3"), Config: &flymachines.FlyMachineConfig{}}
	otherGroup := flymachines.Machine{
		Id:     internal.Ref("m4"),
		Config: &flymachines.FlyMachineConfig{Metadata: internal.Ref(map[string]string{"flydeploy.group": "worker"})},
	}
	out := MachinesByGroup([]flymachines.Machine{withGroup, nilConfig, nilMetadata, otherGroup}, "flydeploy.group", "app")
	assert.Len(t, out, 1)
	assert.Equal(t, "m1", internal.DerefOr(out[0].Id, ""))
	assert.Empty(t, MachinesByGroup(nil, "flydeploy.group", "app"))
}

func TestAppLookupCreateAndEnsureAreIdempotent(t *testing.T) {
	var creates atomic.Int32
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apps/missing":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/apps":
			creates.Add(1)
			var body flymachines.CreateAppRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "missing", internal.DerefOr(body.AppName, ""))
			assert.Equal(t, "personal", internal.DerefOr(body.OrgSlug, ""))
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/apps/existing":
			writeJSON(t, w, http.StatusOK, flymachines.App{Name: internal.Ref("existing")})
		default:
			http.NotFound(w, r)
		}
	})

	_, found, err := d.LookupApp(context.Background(), "missing")
	assert.NoError(t, err)
	assert.False(t, found)
	assert.NoError(t, d.CreateApp(context.Background(), flymachines.CreateAppRequest{
		AppName: internal.Ref("missing"),
		OrgSlug: internal.Ref("personal"),
	}))
	app, err := d.EnsureApp(context.Background(), flymachines.CreateAppRequest{AppName: internal.Ref("existing")})
	assert.NoError(t, err)
	assert.Equal(t, "existing", internal.DerefOr(app.Name, ""))
	assert.EqualValues(t, 1, creates.Load())
}

func TestDeleteMachineMissingIsSafeToRepeat(t *testing.T) {
	d := newTestDeployer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		w.WriteHeader(http.StatusNotFound)
	})
	assert.NoError(t, d.DeleteMachine(context.Background(), "already-gone", false))
}

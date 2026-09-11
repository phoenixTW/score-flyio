package command

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/phoenixTW/score-flyio/internal"
	"github.com/phoenixTW/score-flyio/pkg/flymachines"
)

const e2eAppName = "e2e-gateway"

const e2eSecretValue = "e2e-secret-runtime-value"

const leakedSecretBodyMarker = "leaked-secret-body-marker"

type fakeSecretCreate struct {
	Label         string
	Value         string
	Authorization string
}

type fakeMachineCreate struct {
	ID                  string
	Group               string
	Hash                string
	ContainerEnvs       map[string]map[string]string
	Checks              []string
	ServiceInternalPort int
}

type fakeMachinesAPI struct {
	server           *httptest.Server
	mu               sync.Mutex
	apps             map[string]bool
	machines         []flymachines.Machine
	operations       []string
	creates          []fakeMachineCreate
	secretCreates    []fakeSecretCreate
	deletes          []string
	nextID           int
	failCreateOn     int
	failSecretCreate bool
}

func newFakeMachinesAPI(t *testing.T, failCreateOn int) *fakeMachinesAPI {
	t.Helper()
	fake := &fakeMachinesAPI{apps: make(map[string]bool), nextID: 1, failCreateOn: failCreateOn}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.operations = append(fake.operations, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		encoder := json.NewEncoder(w)
		switch {
		case r.Method == http.MethodGet && len(segments) == 2 && segments[0] == "apps":
			if !fake.apps[segments[1]] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = encoder.Encode(flymachines.App{Id: internal.Ref("app-" + segments[1]), Name: internal.Ref(segments[1]), Status: internal.Ref("deployed")})
		case r.Method == http.MethodPost && len(segments) == 1 && segments[0] == "apps":
			var request flymachines.CreateAppRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			fake.apps[*request.AppName] = true
			w.WriteHeader(http.StatusCreated)
			_ = encoder.Encode(flymachines.App{Id: internal.Ref("app-" + *request.AppName), Name: request.AppName, Status: internal.Ref("deployed")})
		case r.Method == http.MethodDelete && len(segments) == 2 && segments[0] == "apps":
			delete(fake.apps, segments[1])
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPost && len(segments) == 6 && segments[2] == "secrets" && segments[4] == "type":
			if fake.failSecretCreate {
				w.WriteHeader(http.StatusInternalServerError)
				_ = encoder.Encode(map[string]string{"error": leakedSecretBodyMarker, "value": e2eSecretValue})
				return
			}
			var request struct {
				Value []int `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			secretValue := make([]byte, len(request.Value))
			for i, byteValue := range request.Value {
				secretValue[i] = byte(byteValue)
			}
			fake.secretCreates = append(fake.secretCreates, fakeSecretCreate{Label: segments[3], Value: string(secretValue), Authorization: r.Header.Get("Authorization")})
			w.WriteHeader(http.StatusCreated)
			_ = encoder.Encode(struct{}{})
		case r.Method == http.MethodGet && len(segments) == 3 && segments[2] == "machines":
			_ = encoder.Encode(fake.machineSnapshotLocked())
		case r.Method == http.MethodPost && len(segments) == 3 && segments[2] == "machines":
			if fake.failCreateOn > 0 && len(fake.creates)+1 >= fake.failCreateOn {
				w.WriteHeader(http.StatusInternalServerError)
				_ = encoder.Encode(map[string]string{"error": "simulated create failure"})
				return
			}
			var request struct {
				Name   *string                       `json:"name"`
				Region *string                       `json:"region"`
				Config *flymachines.FlyMachineConfig `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			id := fmt.Sprintf("m%d", fake.nextID)
			fake.nextID++
			machine := flymachines.Machine{Id: internal.Ref(id), Name: request.Name, Region: request.Region, State: internal.Ref("started"), InstanceId: internal.Ref(fmt.Sprintf("v%d", fake.nextID)), Config: request.Config}
			fake.machines = append(fake.machines, machine)
			fake.creates = append(fake.creates, recordMachineCreate(id, request.Config))
			_ = encoder.Encode(machine)
		case r.Method == http.MethodGet && len(segments) == 5 && segments[4] == "wait":
			_ = encoder.Encode(struct{}{})
		case r.Method == http.MethodGet && len(segments) == 5 && segments[4] == "events":
			_ = encoder.Encode([]flymachines.MachineEvent{})
		case r.Method == http.MethodPost && len(segments) == 5 && segments[4] == "suspend":
			fake.setMachineStateLocked(segments[3], "suspended")
			_ = encoder.Encode(struct{}{})
		case r.Method == http.MethodPost && len(segments) == 5 && segments[4] == "start":
			fake.setMachineStateLocked(segments[3], "started")
			_ = encoder.Encode(struct{}{})
		case r.Method == http.MethodGet && len(segments) == 4 && segments[2] == "machines":
			machine, found := fake.findMachineLocked(segments[3])
			if !found {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = encoder.Encode(machine)
		case r.Method == http.MethodDelete && len(segments) == 4 && segments[2] == "machines":
			fake.machines = removeMachine(fake.machines, segments[3])
			fake.deletes = append(fake.deletes, segments[3])
			_ = encoder.Encode(struct{}{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func recordMachineCreate(id string, config *flymachines.FlyMachineConfig) fakeMachineCreate {
	record := fakeMachineCreate{ID: id, ContainerEnvs: make(map[string]map[string]string)}
	if config == nil {
		return record
	}
	if config.Metadata != nil {
		record.Group = (*config.Metadata)["flydeploy.group"]
		record.Hash = (*config.Metadata)["flydeploy.config-hash"]
	}
	if config.Checks != nil {
		for name := range *config.Checks {
			record.Checks = append(record.Checks, name)
		}
	}
	if config.Services != nil && len(*config.Services) > 0 {
		if (*config.Services)[0].InternalPort != nil {
			record.ServiceInternalPort = *((*config.Services)[0].InternalPort)
		}
	}
	if config.Containers != nil {
		for _, container := range *config.Containers {
			env := make(map[string]string)
			if container.Env != nil {
				for key, value := range *container.Env {
					env[key] = value
				}
			}
			if container.Name != nil {
				record.ContainerEnvs[*container.Name] = env
			}
		}
	}
	return record
}

func (f *fakeMachinesAPI) machineSnapshotLocked() []flymachines.Machine {
	out := make([]flymachines.Machine, 0, len(f.machines))
	out = append(out, f.machines...)
	return out
}

func (f *fakeMachinesAPI) findMachineLocked(id string) (flymachines.Machine, bool) {
	for _, machine := range f.machines {
		if machine.Id != nil && *machine.Id == id {
			return machine, true
		}
	}
	return flymachines.Machine{}, false
}

func (f *fakeMachinesAPI) setMachineStateLocked(id string, state string) {
	for i := range f.machines {
		if f.machines[i].Id != nil && *f.machines[i].Id == id {
			f.machines[i].State = internal.Ref(state)
		}
	}
}

func removeMachine(machines []flymachines.Machine, id string) []flymachines.Machine {
	out := make([]flymachines.Machine, 0, len(machines))
	for _, machine := range machines {
		if machine.Id == nil || *machine.Id != id {
			out = append(out, machine)
		}
	}
	return out
}

func (f *fakeMachinesAPI) countOperation(operation string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, entry := range f.operations {
		if entry == operation {
			count++
		}
	}
	return count
}

func (f *fakeMachinesAPI) machinesInGroup(group string) []flymachines.Machine {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]flymachines.Machine, 0)
	for _, machine := range f.machines {
		if machine.Config != nil && machine.Config.Metadata != nil && (*machine.Config.Metadata)["flydeploy.group"] == group {
			out = append(out, machine)
		}
	}
	return out
}

func (f *fakeMachinesAPI) machineCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.machines)
}

func (f *fakeMachinesAPI) machineStates() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.machines))
	for _, machine := range f.machines {
		if machine.Id != nil {
			out[*machine.Id] = value(machine.State)
		}
	}
	return out
}

func (f *fakeMachinesAPI) appExists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apps[name]
}

func (f *fakeMachinesAPI) createsInGroup(group string) []fakeMachineCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeMachineCreate, 0)
	for _, create := range f.creates {
		if create.Group == group {
			out = append(out, create)
		}
	}
	return out
}

func (f *fakeMachinesAPI) rollbackDeletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func (f *fakeMachinesAPI) setFailSecretCreate(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSecretCreate = fail
}

func (f *fakeMachinesAPI) recordedSecretCreates() []fakeSecretCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSecretCreate(nil), f.secretCreates...)
}

func (f *fakeMachinesAPI) seedUnmanagedMachine() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.machines = append(f.machines, flymachines.Machine{Id: internal.Ref("external-1"), Name: internal.Ref("external"), Region: internal.Ref("iad"), State: internal.Ref("started")})
}

func newCredentialsProvisionerServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"values":{"host":"auth.internal"},"secrets":{"token":"` + e2eSecretValue + `"}}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func writeMachineE2EFixture(t *testing.T, workerImage string) string {
	t.Helper()
	content := fmt.Sprintf(`apiVersion: score.dev/v1b1
metadata:
  name: gateway
  fly:
    ingress:
      type: public
      hostname: gateway.example.test
    region: iad
    processes:
      web:
        machine_group: app
        vm:
          cpus: 1
          memory_mb: 256
        scale:
          min: 2
          max: 4
        http_service:
          internal_port: 8080
          ports:
            - port: 443
              handlers:
                - http
        checks:
          ready:
            type: http
            port: 8080
            method: get
            path: /readyz
            interval_seconds: 10
            timeout_seconds: 5
      tunnel:
        machine_group: app
        vm:
          cpus: 1
          memory_mb: 256
        scale:
          min: 2
          max: 4
      worker:
        vm:
          cpus: 1
          memory_mb: 256
        scale:
          min: 1
          max: 1
containers:
  web:
    image: registry.example/gateway/web@sha256:%s
    variables:
      LOG_LEVEL: info
      HOST: ${resources.auth.host}
      API_TOKEN: ${resources.auth.token}
  tunnel:
    image: registry.example/gateway/tunnel@sha256:%s
  worker:
    image: %s
resources:
  auth:
    type: credentials
`, strings.Repeat("a", 64), strings.Repeat("b", 64), workerImage)
	workloadFile := "score.yaml"
	if !assert.NoError(t, os.WriteFile(workloadFile, []byte(content), 0600)) {
		t.FailNow()
	}
	return workloadFile
}

func initE2EProject(t *testing.T, credentials *httptest.Server) {
	t.Helper()
	_, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"init", "--fly-app-prefix=e2e-", "--file="})
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	_, _, err = executeAndResetCommand(context.Background(), rootCmd, []string{"provisioners", "add", "e2e-creds", "credentials", "--http-url", credentials.URL})
	if !assert.NoError(t, err) {
		t.FailNow()
	}
}

func setupMachineE2EEnvironment(t *testing.T, failCreateOn int, workerImage string) (*fakeMachinesAPI, string) {
	t.Helper()
	_ = changeToTempDir(t)
	t.Setenv("PATH", t.TempDir())
	fake := newFakeMachinesAPI(t, failCreateOn)
	credentials := newCredentialsProvisionerServer(t)
	t.Setenv("FLY_API_TOKEN", "FlyV1 e2e-token")
	t.Setenv("FLY_API_BASE_URL", fake.server.URL)
	workloadFile := writeMachineE2EFixture(t, workerImage)
	initE2EProject(t, credentials)
	return fake, workloadFile
}

func assertNoFlyTomlFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if !assert.NoError(t, err) {
		return
	}
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), "fly_") && strings.HasSuffix(entry.Name(), ".toml"), "unexpected fly.toml output %s", entry.Name())
	}
}

func (f *fakeMachinesAPI) secretOperationCount() int {
	return f.countOperation("POST /apps/" + e2eAppName + "/secrets/API_TOKEN/type/string")
}

func TestMachineCommandsEndToEndAgainstFakeApi(t *testing.T) {
	fake, workloadFile := setupMachineE2EEnvironment(t, 0, "registry.example/gateway/worker@sha256:"+strings.Repeat("c", 64))

	t.Run("plan dry run shows machine groups without fly toml", func(t *testing.T) {
		stdout, stderr, err := executeAndResetCommand(context.Background(), rootCmd, []string{"plan", workloadFile, "--dry-run"})

		assert.NoError(t, err)
		assert.Contains(t, stdout, "\"machine_groups\"")
		assert.Contains(t, stdout, "\"name\": \"app\"")
		assert.Contains(t, stdout, "\"name\": \"worker\"")
		assert.Contains(t, stdout, "\"action\": \"create\"")
		assert.NotContains(t, stdout, e2eSecretValue)
		assert.NotContains(t, stderr, e2eSecretValue)
		assertNoFlyTomlFiles(t, ".")
	})

	t.Run("apply creates machines for both groups and uploads secrets", func(t *testing.T) {
		stdout, stderr, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile})

		assert.NoError(t, err)
		assert.Equal(t, 1, fake.countOperation("POST /apps"))
		assert.Equal(t, 3, fake.countOperation("POST /apps/"+e2eAppName+"/machines"))
		assert.Len(t, fake.createsInGroup("app"), 2)
		assert.Len(t, fake.createsInGroup("worker"), 1)
		assert.Equal(t, 3, fake.machineCount())

		appCreates := fake.createsInGroup("app")
		webEnv := appCreates[0].ContainerEnvs["web"]
		assert.Equal(t, "info", webEnv["LOG_LEVEL"])
		assert.Equal(t, "auth.internal", webEnv["HOST"])
		for _, create := range append(fake.createsInGroup("app"), fake.createsInGroup("worker")...) {
			for _, env := range create.ContainerEnvs {
				assert.NotContains(t, env, "API_TOKEN")
			}
		}
		assert.Contains(t, appCreates[0].Checks, "web-ready")
		assert.Equal(t, 8080, appCreates[0].ServiceInternalPort)
		assert.Equal(t, 1, fake.secretOperationCount())
		secretCreates := fake.recordedSecretCreates()
		if assert.Len(t, secretCreates, 1) {
			assert.Equal(t, "API_TOKEN", secretCreates[0].Label)
			assert.Equal(t, e2eSecretValue, secretCreates[0].Value)
			assert.Equal(t, "Bearer e2e-token", secretCreates[0].Authorization)
		}

		assert.NotContains(t, stdout, e2eSecretValue)
		assert.NotContains(t, stderr, e2eSecretValue)
		assertNoFlyTomlFiles(t, ".")
	})

	t.Run("apply again is a noop", func(t *testing.T) {
		createsBefore := fake.countOperation("POST /apps/" + e2eAppName + "/machines")
		secretCreatesBefore := fake.secretOperationCount()

		stdout, stderr, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile})

		assert.NoError(t, err)
		assert.Equal(t, createsBefore, fake.countOperation("POST /apps/"+e2eAppName+"/machines"))
		assert.Equal(t, secretCreatesBefore+1, fake.secretOperationCount())
		assert.Equal(t, 3, fake.machineCount())
		assert.Contains(t, stdout, "\"action\": \"noop\"")
		assert.NotContains(t, stdout, "\"action\": \"create\"")
		assert.NotContains(t, stdout, "\"action\": \"update\"")
		assert.NotContains(t, stdout, "\"action\": \"delete\"")
		assert.NotContains(t, stdout, e2eSecretValue)
		assert.NotContains(t, stderr, e2eSecretValue)
	})

	t.Run("status lists machines by group", func(t *testing.T) {
		stdout, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"status", workloadFile})

		assert.NoError(t, err)
		var machines []struct {
			ID    string `json:"id"`
			Group string `json:"group"`
			State string `json:"state"`
		}
		if assert.NoError(t, json.Unmarshal([]byte(stdout), &machines)) {
			assert.Len(t, machines, 3)
			groups := make(map[string]int)
			for _, machine := range machines {
				groups[machine.Group]++
				assert.Equal(t, "started", machine.State)
			}
			assert.Equal(t, 2, groups["app"])
			assert.Equal(t, 1, groups["worker"])
		}
	})

	t.Run("scale worker group to zero removes its machines", func(t *testing.T) {
		stdout, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"scale", workloadFile, "--group", "worker", "--min", "0", "--max", "0", "--apply"})

		assert.NoError(t, err)
		assert.Empty(t, fake.machinesInGroup("worker"))
		assert.Len(t, fake.machinesInGroup("app"), 2)
		assert.Contains(t, stdout, "\"action\": \"delete\"")
		assert.Contains(t, stdout, "\"group\": \"worker\"")
	})

	t.Run("suspend and resume target one group", func(t *testing.T) {
		stdout, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"suspend", workloadFile, "--group", "app"})

		assert.NoError(t, err)
		var suspended struct {
			Suspended []string `json:"suspended"`
		}
		if assert.NoError(t, json.Unmarshal([]byte(stdout), &suspended)) {
			assert.Len(t, suspended.Suspended, 2)
		}
		for _, state := range fake.machineStates() {
			assert.Equal(t, "suspended", state)
		}

		stdout, _, err = executeAndResetCommand(context.Background(), rootCmd, []string{"resume", workloadFile, "--group", "app"})

		assert.NoError(t, err)
		var resumed struct {
			Resumed []string `json:"resumed"`
		}
		if assert.NoError(t, json.Unmarshal([]byte(stdout), &resumed)) {
			assert.Len(t, resumed.Resumed, 2)
		}
		for _, state := range fake.machineStates() {
			assert.Equal(t, "started", state)
		}
	})

	t.Run("destroy removes only managed machines", func(t *testing.T) {
		fake.seedUnmanagedMachine()

		stdout, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"destroy", workloadFile, "--yes"})

		assert.NoError(t, err)
		var destroyOutput struct {
			AppName    string `json:"app_name"`
			Deleted    int    `json:"deleted"`
			AppDeleted bool   `json:"app_deleted"`
		}
		if assert.NoError(t, json.Unmarshal([]byte(stdout), &destroyOutput)) {
			assert.Equal(t, e2eAppName, destroyOutput.AppName)
			assert.Equal(t, 2, destroyOutput.Deleted)
			assert.False(t, destroyOutput.AppDeleted)
		}
		assert.Empty(t, fake.machinesInGroup("app"))
		assert.Equal(t, 1, fake.machineCount())
		assert.True(t, fake.appExists(e2eAppName))
	})
}

func TestApplyFailureRollsBackCreatedMachines(t *testing.T) {
	fake, workloadFile := setupMachineE2EEnvironment(t, 3, "registry.example/gateway/worker@sha256:"+strings.Repeat("c", 64))

	stdout, stderr, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile})

	assert.ErrorContains(t, err, "apply failed")
	assert.ErrorContains(t, err, "machines create failed with status 500")
	assert.Equal(t, 0, fake.machineCount())
	assert.Len(t, fake.rollbackDeletes(), 2)
	assert.True(t, fake.appExists(e2eAppName))
	assert.NotContains(t, stdout, e2eSecretValue)
	assert.NotContains(t, stderr, e2eSecretValue)
}

func TestApplySecretUploadFailureRedactsValue(t *testing.T) {
	fake, workloadFile := setupMachineE2EEnvironment(t, 0, "registry.example/gateway/worker@sha256:"+strings.Repeat("c", 64))
	fake.setFailSecretCreate(true)

	stdout, stderr, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile})

	assert.ErrorContains(t, err, "API_TOKEN")
	assert.ErrorContains(t, err, "500")
	for _, leak := range []string{e2eSecretValue, leakedSecretBodyMarker} {
		assert.NotContains(t, err.Error(), leak)
		assert.NotContains(t, stdout, leak)
		assert.NotContains(t, stderr, leak)
	}
	assert.Equal(t, 0, fake.machineCount())
	assert.Equal(t, 0, fake.countOperation("POST /apps/"+e2eAppName+"/machines"))
	assert.True(t, fake.appExists(e2eAppName))
}

func TestApplyRejectsMutableImageBeforeAnyApiMutation(t *testing.T) {
	fake, workloadFile := setupMachineE2EEnvironment(t, 0, "registry.example/gateway/worker:latest")

	_, _, err := executeAndResetCommand(context.Background(), rootCmd, []string{"apply", workloadFile})

	assert.ErrorContains(t, err, "must be pinned to an immutable sha256 digest")
	assert.Equal(t, 0, fake.countOperation("POST /apps"))
	assert.Equal(t, 0, fake.countOperation("POST /apps/"+e2eAppName+"/machines"))
	assert.Equal(t, 0, fake.machineCount())
}

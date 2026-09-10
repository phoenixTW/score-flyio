package machineconfig

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/phoenixTW/score-flyio/internal"
	"github.com/phoenixTW/score-flyio/pkg/flymachines"
)

func happyGroup() Group {
	return Group{
		Name:               "app",
		Region:             "iad",
		Guest:              &Guest{CpuKind: "shared", Cpus: 1, MemoryMb: 512},
		MinMachines:        1,
		MaxMachines:        2,
		Restart:            RestartPolicyAlways,
		StopSignal:         "SIGTERM",
		StopTimeoutSeconds: 30,
		Volumes:            []Volume{{Name: "data", SizeGb: 3}},
		Services: []Service{
			{
				Protocol:           "tcp",
				InternalPort:       8080,
				Ports:              []ServicePort{{Port: 443, Handlers: []string{"http", "tls"}}},
				AutoStop:           AutoStopSuspend,
				AutoStart:          true,
				MinMachinesRunning: 1,
				Concurrency:        map[string]any{"type": "requests", "hard_limit": 25, "soft_limit": 20},
				Checks: []ServiceHttpCheck{
					{
						Path:            "/healthz",
						Protocol:        "http",
						Headers:         map[string]string{"X-Probe": "score-flyio"},
						IntervalSeconds: 10,
						TimeoutSeconds:  2,
					},
				},
			},
		},
		Checks: map[string]Check{
			"readiness": {
				Type:               CheckTypeHTTP,
				Port:               8080,
				Method:             "get",
				Path:               "/ready",
				IntervalSeconds:    10,
				TimeoutSeconds:     2,
				GracePeriodSeconds: 5,
			},
		},
		Metrics:  &Metrics{Https: true, Path: "/metrics", Port: 9090},
		Metadata: map[string]string{"flydeploy.group": "app"},
		Containers: []Container{
			{
				Name:    "api",
				Image:   "ghcr.io/example/api:sha-abc",
				Command: []string{"./api"},
				Args:    []string{"serve", "--port", "8080"},
				Env:     map[string]string{"NODE_ENV": "production"},
				Files:   []File{{GuestPath: "/etc/api/config.json", RawContent: "eyJhIjoxfQ=="}},
				Mounts:  []Mount{{Volume: "data", Path: "/data"}},
			},
			{
				Name:      "sidecar",
				Image:     "example/sidecar:2024.10.0",
				DependsOn: []Dependency{{Name: "api", Condition: DependencyConditionHealthy}},
			},
			{
				Name:    "worker",
				Image:   "ghcr.io/example/worker:sha-def",
				Restart: RestartPolicyNo,
			},
		},
	}
}

func happyPlan() *Plan {
	return &Plan{
		AppName:         "example-api",
		RendererVersion: "dev",
		Workload:        "api",
		Environment:     "staging",
		Groups:          []Group{happyGroup()},
	}
}

func TestValidateContainerNameAcceptsAndRejects(t *testing.T) {
	assert.NoError(t, ValidateContainerName("api"))
	assert.NoError(t, ValidateContainerName("sidecar"))
	assert.NoError(t, ValidateContainerName("a"))
	assert.NoError(t, ValidateContainerName("a-b-2"))
	assert.ErrorContains(t, ValidateContainerName(""), "container name must not be empty")
	assert.ErrorContains(t, ValidateContainerName("API"), "must match")
	assert.ErrorContains(t, ValidateContainerName("-api"), "must match")
	assert.ErrorContains(t, ValidateContainerName("api_1"), "must match")
	assert.ErrorContains(t, ValidateContainerName("a"+strings.Repeat("0", 63)), "must match")
}

func TestValidateAcceptsHappyPlan(t *testing.T) {
	assert.NoError(t, happyPlan().Validate())
}

func TestValidateAcceptsReleaseCommand(t *testing.T) {
	p := happyPlan()
	p.ReleaseCommand = []string{"bin/migrate", "up"}

	assert.NoError(t, p.Validate())
}

func TestValidateAllowsDifferentImagesInOneGroup(t *testing.T) {
	p := happyPlan()
	images := make([]string, 0)
	for _, c := range p.Groups[0].Containers {
		images = append(images, c.Image)
	}
	assert.Len(t, images, 3)
	assert.Len(t, unique(images), 3)
	assert.NoError(t, p.Validate())
}

func TestValidateImmutableImagesRequiresMatchingDigestReferences(t *testing.T) {
	p := happyPlan()
	p.Groups[0].Containers[0].Image = "ghcr.io/example/api@sha256:" + strings.Repeat("a", 64)
	p.Groups[0].Containers[0].ImageDigest = "sha256:" + strings.Repeat("a", 64)
	p.Groups[0].Containers[1].Image = "example/sidecar@sha256:" + strings.Repeat("b", 64)
	p.Groups[0].Containers[1].ImageDigest = "sha256:" + strings.Repeat("b", 64)
	p.Groups[0].Containers[2].Image = "ghcr.io/example/worker@sha256:" + strings.Repeat("c", 64)
	p.Groups[0].Containers[2].ImageDigest = "sha256:" + strings.Repeat("c", 64)
	assert.NoError(t, p.ValidateImmutableImages())
	p.Groups[0].Containers[0].ImageDigest = ""
	assert.ErrorContains(t, p.ValidateImmutableImages(), "must be pinned to an immutable sha256 digest")
}

func unique(in []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func TestValidateFailures(t *testing.T) {
	tests := []struct {
		name string
		give func(p *Plan)
		want string
	}{
		{
			name: "empty app name",
			give: func(p *Plan) { p.AppName = "" },
			want: "app_name must not be empty",
		},
		{
			name: "empty workload",
			give: func(p *Plan) { p.Workload = "" },
			want: "workload must not be empty",
		},
		{
			name: "no groups",
			give: func(p *Plan) { p.Groups = nil },
			want: "groups must not be empty",
		},
		{
			name: "unsorted groups",
			give: func(p *Plan) {
				p.Groups = []Group{happyGroup(), happyGroup()}
				p.Groups[0].Name = "zzz"
				p.Groups[1].Name = "aaa"
			},
			want: "must sort after",
		},
		{
			name: "duplicate group names",
			give: func(p *Plan) {
				p.Groups = []Group{happyGroup(), happyGroup()}
			},
			want: "must sort after",
		},
		{
			name: "invalid group name",
			give: func(p *Plan) { p.Groups[0].Name = "App_1" },
			want: "must match",
		},
		{
			name: "invalid region",
			give: func(p *Plan) { p.Groups[0].Region = "IAD!" },
			want: "region",
		},
		{
			name: "invalid group restart",
			give: func(p *Plan) { p.Groups[0].Restart = "sometimes" },
			want: "restart 'sometimes'",
		},
		{
			name: "negative stop timeout",
			give: func(p *Plan) { p.Groups[0].StopTimeoutSeconds = -1 },
			want: "stop_timeout_seconds must not be negative",
		},
		{
			name: "lowercase stop signal",
			give: func(p *Plan) { p.Groups[0].StopSignal = "sigterm" },
			want: "stop_signal",
		},
		{
			name: "guest cpu kind invalid",
			give: func(p *Plan) { p.Groups[0].Guest.CpuKind = "burst" },
			want: "guest cpu_kind 'burst' must be shared or performance",
		},
		{
			name: "guest cpu arch invalid",
			give: func(p *Plan) { p.Groups[0].Guest.CpuArch = "riscv" },
			want: "guest cpu_arch 'riscv' must be amd64 or arm64",
		},
		{
			name: "guest cpus too low",
			give: func(p *Plan) { p.Groups[0].Guest.Cpus = 0 },
			want: "guest cpus must be at least 1",
		},
		{
			name: "guest memory too low",
			give: func(p *Plan) { p.Groups[0].Guest.MemoryMb = 255 },
			want: "guest memory_mb must be at least 256",
		},
		{
			name: "guest memory not multiple",
			give: func(p *Plan) { p.Groups[0].Guest.MemoryMb = 300 },
			want: "must be a multiple of 256",
		},
		{
			name: "negative min machines",
			give: func(p *Plan) { p.Groups[0].MinMachines = -1 },
			want: "min_machines must not be negative",
		},
		{
			name: "max below min",
			give: func(p *Plan) {
				p.Groups[0].MinMachines = 2
				p.Groups[0].MaxMachines = 1
			},
			want: "max_machines 1 must be at least min_machines 2",
		},
		{
			name: "unsorted volumes",
			give: func(p *Plan) {
				p.Groups[0].Volumes = []Volume{{Name: "zeta"}, {Name: "alpha"}}
			},
			want: "volumes[1] name 'alpha' must sort after 'zeta'",
		},
		{
			name: "invalid volume name",
			give: func(p *Plan) {
				p.Groups[0].Volumes = []Volume{{Name: "data"}, {Name: "~bad"}}
			},
			want: "must match",
		},
		{
			name: "no containers",
			give: func(p *Plan) { p.Groups[0].Containers = nil },
			want: "containers must not be empty",
		},
		{
			name: "empty container name",
			give: func(p *Plan) { p.Groups[0].Containers[0].Name = "" },
			want: "container name must not be empty",
		},
		{
			name: "invalid container name",
			give: func(p *Plan) { p.Groups[0].Containers[0].Name = "API" },
			want: "must match",
		},
		{
			name: "unsorted containers",
			give: func(p *Plan) {
				g := &p.Groups[0]
				g.Containers = append(g.Containers, g.Containers[0])
				g.Containers[0].Name = "zzz"
				g.Containers[0].Image = "ghcr.io/x/zzz:1"
				g.Containers[len(g.Containers)-1].Name = "aaa"
				g.Containers[len(g.Containers)-1].Image = "ghcr.io/x/aaa:1"
			},
			want: "must sort after",
		},
		{
			name: "duplicate container names",
			give: func(p *Plan) {
				g := &p.Groups[0]
				g.Containers = append(g.Containers, g.Containers[0])
			},
			want: "must sort after",
		},
		{
			name: "empty image",
			give: func(p *Plan) { p.Groups[0].Containers[0].Image = "" },
			want: "image must not be empty",
		},
		{
			name: "invalid container restart",
			give: func(p *Plan) { p.Groups[0].Containers[2].Restart = "whenever" },
			want: "restart 'whenever'",
		},
		{
			name: "relative file guest path",
			give: func(p *Plan) { p.Groups[0].Containers[0].Files[0].GuestPath = "etc/api/config.json" },
			want: "guest_path 'etc/api/config.json' must be absolute",
		},
		{
			name: "mount references undeclared volume",
			give: func(p *Plan) { p.Groups[0].Containers[0].Mounts[0].Volume = "missing" },
			want: "references undeclared volume 'missing'",
		},
		{
			name: "relative mount path",
			give: func(p *Plan) { p.Groups[0].Containers[0].Mounts[0].Path = "data" },
			want: "path 'data' must be absolute",
		},
		{
			name: "depends on self",
			give: func(p *Plan) { p.Groups[0].Containers[1].DependsOn[0].Name = "sidecar" },
			want: "must not reference itself",
		},
		{
			name: "depends on unknown sibling",
			give: func(p *Plan) { p.Groups[0].Containers[1].DependsOn[0].Name = "ghost" },
			want: "references unknown sibling 'ghost'",
		},
		{
			name: "invalid dependency condition",
			give: func(p *Plan) { p.Groups[0].Containers[1].DependsOn[0].Condition = "soon" },
			want: "condition 'soon'",
		},
		{
			name: "unsorted services",
			give: func(p *Plan) {
				p.Groups[0].Services = append(p.Groups[0].Services, p.Groups[0].Services[0])
				p.Groups[0].Services[1].InternalPort = 80
			},
			want: "internal_port 80 must sort above 8080",
		},
		{
			name: "internal port zero",
			give: func(p *Plan) { p.Groups[0].Services[0].InternalPort = 0 },
			want: "internal_port 0 must be between 1 and 65535",
		},
		{
			name: "edge port too high",
			give: func(p *Plan) { p.Groups[0].Services[0].Ports[0].Port = 70000 },
			want: "port 70000 must be between 1 and 65535",
		},
		{
			name: "invalid service protocol",
			give: func(p *Plan) { p.Groups[0].Services[0].Protocol = "grpc" },
			want: "protocol 'grpc' must be tcp or udp",
		},
		{
			name: "invalid autostop",
			give: func(p *Plan) { p.Groups[0].Services[0].AutoStop = "maybe" },
			want: "auto_stop 'maybe'",
		},
		{
			name: "service check path not rooted",
			give: func(p *Plan) { p.Groups[0].Services[0].Checks[0].Path = "healthz" },
			want: "http check path 'healthz' must start with /",
		},
		{
			name: "service check bad protocol",
			give: func(p *Plan) { p.Groups[0].Services[0].Checks[0].Protocol = "ftp" },
			want: "http check protocol 'ftp'",
		},
		{
			name: "service check negative interval",
			give: func(p *Plan) { p.Groups[0].Services[0].Checks[0].IntervalSeconds = -1 },
			want: "intervals must not be negative",
		},
		{
			name: "invalid machine check type",
			give: func(p *Plan) {
				c := p.Groups[0].Checks["readiness"]
				c.Type = "grpc"
				p.Groups[0].Checks["readiness"] = c
			},
			want: "type 'grpc' must be http or tcp",
		},
		{
			name: "machine check port zero",
			give: func(p *Plan) {
				c := p.Groups[0].Checks["readiness"]
				c.Port = 0
				p.Groups[0].Checks["readiness"] = c
			},
			want: "port 0 must be between 1 and 65535",
		},
		{
			name: "machine check path not rooted",
			give: func(p *Plan) {
				c := p.Groups[0].Checks["readiness"]
				c.Path = "ready"
				p.Groups[0].Checks["readiness"] = c
			},
			want: "http path 'ready' must start with /",
		},
		{
			name: "empty metadata key",
			give: func(p *Plan) { p.Groups[0].Metadata[""] = "value" },
			want: "metadata keys must not be empty",
		},
		{
			name: "empty release command element",
			give: func(p *Plan) { p.ReleaseCommand = []string{"bin/migrate", ""} },
			want: "release_command must not contain empty elements",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := happyPlan()
			tt.give(p)
			err := p.Validate()
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tt.want)
			}
		})
	}
}

func TestValidateAllowsCallerDefinedScaleAboveTen(t *testing.T) {
	plan := happyPlan()
	plan.Groups[0].MinMachines = 20
	plan.Groups[0].MaxMachines = 25

	assert.NoError(t, plan.Validate())
}

func TestToFlyMachineConfigMapsAllFields(t *testing.T) {
	g := happyGroup()
	expected := flymachines.FlyMachineConfig{
		AutoDestroy: nil,
		Containers: &[]flymachines.FlyContainerConfig{
			{
				Name:       internal.Ref("api"),
				Image:      internal.Ref("ghcr.io/example/api:sha-abc"),
				Entrypoint: &[]string{"./api"},
				Cmd:        &[]string{"serve", "--port", "8080"},
				Env:        &map[string]string{"NODE_ENV": "production"},
				Files: &[]flymachines.FlyFile{
					{GuestPath: internal.Ref("/etc/api/config.json"), RawValue: internal.Ref("eyJhIjoxfQ==")},
				},
				Mounts: &[]flymachines.FlyContainerMount{
					{Name: internal.Ref("data"), Path: internal.Ref("/data")},
				},
			},
			{
				Name:  internal.Ref("sidecar"),
				Image: internal.Ref("example/sidecar:2024.10.0"),
				DependsOn: &[]flymachines.FlyContainerDependency{
					{Name: internal.Ref("api"), Condition: internal.Ref(flymachines.FlyContainerDependencyConditionHealthy)},
				},
			},
			{
				Name:    internal.Ref("worker"),
				Image:   internal.Ref("ghcr.io/example/worker:sha-def"),
				Restart: &flymachines.FlyMachineRestart{Policy: internal.Ref(flymachines.FlyMachineRestartPolicy(RestartPolicyNo))},
			},
		},
		Guest: &flymachines.FlyMachineGuest{
			CpuKind:  internal.Ref("shared"),
			Cpus:     internal.Ref(1),
			MemoryMb: internal.Ref(512),
		},
		Restart:  &flymachines.FlyMachineRestart{Policy: internal.Ref(flymachines.FlyMachineRestartPolicy(RestartPolicyAlways))},
		Schedule: nil,
		StopConfig: &flymachines.FlyStopConfig{
			Signal:  internal.Ref("SIGTERM"),
			Timeout: &flymachines.FlyDuration{TimeDuration: internal.Ref(30 * int(time.Second))},
		},
		Volumes: &[]flymachines.FlyVolumeConfig{{Name: internal.Ref("data")}},
		Services: &[]flymachines.FlyMachineService{
			{
				Protocol:           internal.Ref("tcp"),
				InternalPort:       internal.Ref(8080),
				Ports:              &[]flymachines.FlyMachinePort{{Port: internal.Ref(443), Handlers: &[]string{"http", "tls"}}},
				Autostop:           internal.Ref(flymachines.FlyMachineServiceAutostop(AutoStopSuspend)),
				Autostart:          internal.Ref(true),
				MinMachinesRunning: internal.Ref(1),
				Concurrency: &flymachines.FlyMachineServiceConcurrency{
					Type:      internal.Ref("requests"),
					HardLimit: internal.Ref(25),
					SoftLimit: internal.Ref(20),
				},
				Checks: &[]flymachines.FlyMachineCheck{
					{
						Type:     internal.Ref(CheckTypeHTTP),
						Method:   internal.Ref("get"),
						Path:     internal.Ref("/healthz"),
						Protocol: internal.Ref("http"),
						Headers: &[]flymachines.FlyMachineHTTPHeader{
							{Name: internal.Ref("X-Probe"), Values: &[]string{"score-flyio"}},
						},
						Interval: internal.Ref("10s"),
						Timeout:  internal.Ref("2s"),
					},
				},
			},
		},
		Checks: &map[string]flymachines.FlyMachineCheck{
			"readiness": {
				Type:        internal.Ref(CheckTypeHTTP),
				Port:        internal.Ref(8080),
				Method:      internal.Ref("get"),
				Path:        internal.Ref("/ready"),
				Interval:    internal.Ref("10s"),
				Timeout:     internal.Ref("2s"),
				GracePeriod: &flymachines.FlyDuration{TimeDuration: internal.Ref(5 * int(time.Second))},
			},
		},
		Metrics: &flymachines.FlyMachineMetrics{
			Https: internal.Ref(true),
			Path:  internal.Ref("/metrics"),
			Port:  internal.Ref(9090),
		},
		Metadata: &map[string]string{"flydeploy.group": "app"},
	}
	assert.Equal(t, expected, g.ToFlyMachineConfig())
}

func TestServiceExplicitAutoStartFalseSurvivesPlanJSONAndFlyConversion(t *testing.T) {
	plan := happyPlan()
	plan.Groups[0].Services[0].AutoStart = false
	plan.Groups[0].Services[0].AutoStartSet = true

	raw, err := json.Marshal(plan)
	assert.NoError(t, err)
	assert.Contains(t, string(raw), `"auto_start":false`)

	var decoded Plan
	assert.NoError(t, json.Unmarshal(raw, &decoded))
	service := decoded.Groups[0].Services[0]
	assert.False(t, service.AutoStart)
	assert.True(t, service.AutoStartSet)
	flyService := (*decoded.Groups[0].ToFlyMachineConfig().Services)[0]
	if assert.NotNil(t, flyService.Autostart) {
		assert.False(t, *flyService.Autostart)
	}
}

func TestConfigHashIsStableAndSensitive(t *testing.T) {
	a, err := ConfigHash(happyGroupPtr())
	assert.NoError(t, err)
	b, err := ConfigHash(happyGroupPtr())
	assert.NoError(t, err)
	assert.Equal(t, a, b)

	changed := happyGroup()
	changed.Containers[0].Env["NODE_ENV"] = "staging"
	c, err := ConfigHash(&changed)
	assert.NoError(t, err)
	assert.NotEqual(t, a, c)

	reordered := happyGroup()
	reordered.Services[0].Concurrency = map[string]any{"soft_limit": 20, "type": "requests", "hard_limit": 25}
	reordered.Services[0].Checks = append(reordered.Services[0].Checks, ServiceHttpCheck{Path: "/live"})
	reordered.Services[0].Checks = reordered.Services[0].Checks[:1]
	d, err := ConfigHash(&reordered)
	assert.NoError(t, err)
	assert.Equal(t, a, d)
}

func happyGroupPtr() *Group {
	g := happyGroup()
	return &g
}

func TestPlanJsonRoundTripKeepsShape(t *testing.T) {
	p := happyPlan()
	raw, err := json.Marshal(p)
	assert.NoError(t, err)
	assert.Contains(t, string(raw), `"app_name":"example-api"`)
	assert.Contains(t, string(raw), `"containers"`)
	var out Plan
	assert.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, p.AppName, out.AppName)
	assert.Equal(t, p.RendererVersion, out.RendererVersion)
	assert.Equal(t, p.Environment, out.Environment)
	assert.Len(t, out.Groups, len(p.Groups))
	assert.Equal(t, p.Groups[0].Name, out.Groups[0].Name)
	assert.Len(t, out.Groups[0].Containers, len(p.Groups[0].Containers))
	assert.Equal(t, p.Groups[0].ToFlyMachineConfig(), out.Groups[0].ToFlyMachineConfig())
}

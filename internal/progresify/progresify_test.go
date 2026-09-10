package progresify

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func happyMetadata() *Metadata {
	return &Metadata{
		Owner:           "team-a",
		SlackChannel:    "#flowbit",
		SecretNamespace: "flowbit-staging",
		Region:          "iad",
		Ingress: &Ingress{
			Type:     "cloudflare",
			Hostname: "api.flowbit.work",
			Tunnel:   "flowbit-staging",
		},
		ReleaseCommand: []string{"bin/migrate"},
		Variables:      map[string]string{"LOG_LEVEL": "debug"},
		Processes: map[string]Process{
			"app": {
				MachineGroup: "app",
				Vm:           &Vm{Cpus: 1, MemoryMb: 512},
				Scale:        &Scale{Min: 1, Max: 2},
				HttpService: &HttpService{
					InternalPort: 8080,
					Ports:        []ServicePort{{Port: 443, Handlers: []string{"http", "tls"}}},
				},
				Checks: map[string]Check{
					"readiness": {Type: "http", Port: 8080, Method: "get", Path: "/health", IntervalSeconds: 10, TimeoutSeconds: 2},
				},
			},
			"worker": {
				Vm:    &Vm{Cpus: 1, MemoryMb: 256},
				Scale: &Scale{Min: 0, Max: 2},
			},
			"cloudflared": {
				MachineGroup: "app",
				Vm:           &Vm{Cpus: 1, MemoryMb: 512},
				Scale:        &Scale{Min: 1, Max: 2},
			},
		},
	}
}

func happyContainers() []string {
	return []string{"app", "worker", "cloudflared"}
}

func mutateProcess(m *Metadata, name string, mutate func(p *Process)) {
	p := m.Processes[name]
	mutate(&p)
	m.Processes[name] = p
}

func mutateCheck(m *Metadata, processName, checkName string, mutate func(c *Check)) {
	p := m.Processes[processName]
	c := p.Checks[checkName]
	mutate(&c)
	p.Checks[checkName] = c
	m.Processes[processName] = p
}

func TestParseNilMap(t *testing.T) {
	metadata, err := Parse(nil)

	assert.NoError(t, err)
	assert.Nil(t, metadata)
}

func TestParseEmptyMap(t *testing.T) {
	metadata, err := Parse(map[string]any{})

	assert.NoError(t, err)
	assert.Nil(t, metadata)
}

func TestParseUnknownField(t *testing.T) {
	raw := map[string]any{"owner": "team-a", "nope": true}

	metadata, err := Parse(raw)

	assert.Error(t, err)
	assert.Nil(t, metadata)
	assert.Contains(t, err.Error(), `unknown field "nope"`)
}

func TestParseWrongType(t *testing.T) {
	raw := map[string]any{"owner": 123}

	metadata, err := Parse(raw)

	assert.Error(t, err)
	assert.Nil(t, metadata)
	assert.Contains(t, err.Error(), "cannot unmarshal")
}

func TestParseFullMetadata(t *testing.T) {
	raw := map[string]any{
		"owner":            "team-a",
		"slack_channel":    "#flowbit",
		"secret_namespace": "flowbit-staging",
		"region":           "iad",
		"ingress": map[string]any{
			"type":     "cloudflare",
			"hostname": "api.flowbit.work",
			"tunnel":   "flowbit-staging",
		},
		"release_command": []string{"bin/migrate"},
		"variables":       map[string]any{"LOG_LEVEL": "debug"},
		"processes": map[string]any{
			"app": map[string]any{
				"machine_group": "app",
				"vm":            map[string]any{"cpus": 1, "memory_mb": 512},
				"scale":         map[string]any{"min": 1, "max": 2},
				"http_service": map[string]any{
					"internal_port": 8080,
					"ports":         []any{map[string]any{"port": 443, "handlers": []any{"http", "tls"}}},
				},
				"concurrency": map[string]any{"soft_limit": 10, "hard_limit": 20},
				"checks": map[string]any{
					"readiness": map[string]any{
						"type":             "http",
						"port":             8080,
						"method":           "get",
						"path":             "/health",
						"headers":          map[string]any{"X-Probe": "score"},
						"interval_seconds": 10,
						"timeout_seconds":  2,
					},
				},
			},
			"worker": map[string]any{},
			"cloudflared": map[string]any{
				"machine_group": "app",
				"vm":            map[string]any{"cpus": 1, "memory_mb": 512},
				"scale":         map[string]any{"min": 1, "max": 2},
			},
		},
	}

	metadata, err := Parse(raw)

	assert.NoError(t, err)
	assert.Equal(t, &Metadata{
		Owner:           "team-a",
		SlackChannel:    "#flowbit",
		SecretNamespace: "flowbit-staging",
		Region:          "iad",
		Ingress: &Ingress{
			Type:     "cloudflare",
			Hostname: "api.flowbit.work",
			Tunnel:   "flowbit-staging",
		},
		ReleaseCommand: []string{"bin/migrate"},
		Variables:      map[string]string{"LOG_LEVEL": "debug"},
		Processes: map[string]Process{
			"app": {
				MachineGroup: "app",
				Vm:           &Vm{Cpus: 1, MemoryMb: 512},
				Scale:        &Scale{Min: 1, Max: 2},
				HttpService: &HttpService{
					InternalPort: 8080,
					Ports:        []ServicePort{{Port: 443, Handlers: []string{"http", "tls"}}},
				},
				Concurrency: map[string]any{"soft_limit": float64(10), "hard_limit": float64(20)},
				Checks: map[string]Check{
					"readiness": {Type: "http", Port: 8080, Method: "get", Path: "/health", Headers: map[string]string{"X-Probe": "score"}, IntervalSeconds: 10, TimeoutSeconds: 2},
				},
			},
			"worker": {},
			"cloudflared": {
				MachineGroup: "app",
				Vm:           &Vm{Cpus: 1, MemoryMb: 512},
				Scale:        &Scale{Min: 1, Max: 2},
			},
		},
	}, metadata)
}

func TestValidateNilMetadata(t *testing.T) {
	err := Validate(nil, happyContainers())

	assert.NoError(t, err)
}

func TestValidateHappyPath(t *testing.T) {
	metadata := happyMetadata()

	err := Validate(metadata, happyContainers())

	assert.NoError(t, err)
}

func TestValidateAcceptsFlyConcurrencyKeys(t *testing.T) {
	metadata := happyMetadata()
	mutateProcess(metadata, "app", func(p *Process) {
		p.Concurrency = map[string]any{"type": "requests", "hard_limit": 25, "soft_limit": 10}
	})

	err := Validate(metadata, happyContainers())

	assert.NoError(t, err)
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(m *Metadata)
		containers []string
		expected   string
	}{
		{
			name:     "process without matching container",
			mutate:   func(m *Metadata) { m.Processes["extra"] = Process{} },
			expected: "process 'extra' has no matching Score container",
		},
		{
			name:       "container without process",
			mutate:     func(m *Metadata) {},
			containers: []string{"app", "worker", "cloudflared", "sidecar"},
			expected:   "container 'sidecar' has no configuration in metadata.progresify.processes",
		},
		{
			name: "invalid process name",
			mutate: func(m *Metadata) {
				m.Processes["App"] = m.Processes["app"]
				delete(m.Processes, "app")
			},
			expected: "processes[App]: name must match",
		},
		{
			name: "colocated processes with different restart",
			mutate: func(m *Metadata) {
				mutateProcess(m, "app", func(p *Process) { p.Restart = "always" })
				mutateProcess(m, "cloudflared", func(p *Process) { p.Restart = "no" })
			},
			expected: "machine group 'app': process 'cloudflared' must share vm, scale, and restart with process 'app'",
		},
		{
			name: "colocated processes with different vm",
			mutate: func(m *Metadata) {
				p := m.Processes["cloudflared"]
				p.Vm = &Vm{Cpus: 1, MemoryMb: 1024}
				m.Processes["cloudflared"] = p
			},
			expected: "machine group 'app': process 'cloudflared' must share vm, scale, and restart with process 'app'",
		},
		{
			name: "colocated processes with nil vm",
			mutate: func(m *Metadata) {
				p := m.Processes["cloudflared"]
				p.Vm = nil
				m.Processes["cloudflared"] = p
			},
			expected: "machine group 'app': process 'cloudflared' must share vm, scale, and restart with process 'app'",
		},
		{
			name: "colocated processes with different scale",
			mutate: func(m *Metadata) {
				p := m.Processes["cloudflared"]
				p.Scale = &Scale{Min: 1, Max: 4}
				m.Processes["cloudflared"] = p
			},
			expected: "machine group 'app': process 'cloudflared' must share vm, scale, and restart with process 'app'",
		},
		{
			name: "concurrency with unsupported key",
			mutate: func(m *Metadata) {
				mutateProcess(m, "app", func(p *Process) { p.Concurrency = map[string]any{"soft": 10} })
			},
			expected: "processes[app].concurrency: key 'soft' must be one of type, hard_limit, soft_limit",
		},
		{
			name:     "ingress with invalid type",
			mutate:   func(m *Metadata) { m.Ingress.Type = "public" },
			expected: "ingress.type: must be one of",
		},
		{
			name:     "cloudflare ingress without hostname",
			mutate:   func(m *Metadata) { m.Ingress.Hostname = "" },
			expected: "ingress.type=cloudflare requires a hostname",
		},
		{
			name:     "private ingress with hostname",
			mutate:   func(m *Metadata) { m.Ingress.Type = "private" },
			expected: "ingress.type=private must not set a hostname",
		},
		{
			name:     "none ingress with tunnel",
			mutate:   func(m *Metadata) { m.Ingress.Type = "none" },
			expected: "ingress.type=none must not set a hostname or tunnel",
		},
		{
			name:     "scale min negative",
			mutate:   func(m *Metadata) { m.Processes["worker"].Scale.Min = -1 },
			expected: "processes[worker].scale.min: must be >= 0",
		},
		{
			name: "scale max below min",
			mutate: func(m *Metadata) {
				mutateProcess(m, "worker", func(p *Process) { p.Scale = &Scale{Min: 2, Max: 1} })
			},
			expected: "processes[worker].scale.max: must be >= min",
		},
		{
			name:     "scale max above limit",
			mutate:   func(m *Metadata) { m.Processes["worker"].Scale.Max = 11 },
			expected: "processes[worker].scale.max: must be <= 10",
		},
		{
			name:     "vm cpus below one",
			mutate:   func(m *Metadata) { m.Processes["worker"].Vm.Cpus = 0 },
			expected: "processes[worker].vm.cpus: must be >= 1",
		},
		{
			name:     "vm memory below minimum",
			mutate:   func(m *Metadata) { m.Processes["worker"].Vm.MemoryMb = 128 },
			expected: "processes[worker].vm.memory_mb: must be >= 256 and a multiple of 256",
		},
		{
			name:     "vm memory not multiple of 256",
			mutate:   func(m *Metadata) { m.Processes["worker"].Vm.MemoryMb = 384 },
			expected: "processes[worker].vm.memory_mb: must be >= 256 and a multiple of 256",
		},
		{
			name:     "http service internal port zero",
			mutate:   func(m *Metadata) { m.Processes["app"].HttpService.InternalPort = 0 },
			expected: "processes[app].http_service.internal_port: must be between 1 and 65535",
		},
		{
			name:     "http service internal port above range",
			mutate:   func(m *Metadata) { m.Processes["app"].HttpService.InternalPort = 70000 },
			expected: "processes[app].http_service.internal_port: must be between 1 and 65535",
		},
		{
			name:     "service port above range",
			mutate:   func(m *Metadata) { m.Processes["app"].HttpService.Ports[0].Port = 70000 },
			expected: "processes[app].http_service.ports[0].port: must be between 1 and 65535",
		},
		{
			name:     "service port invalid handler",
			mutate:   func(m *Metadata) { m.Processes["app"].HttpService.Ports[0].Handlers = []string{"grpc"} },
			expected: "processes[app].http_service.ports[0].handlers: 'grpc' must be one of",
		},
		{
			name: "check with invalid type",
			mutate: func(m *Metadata) {
				mutateCheck(m, "app", "readiness", func(c *Check) { c.Type = "grpc" })
			},
			expected: "processes[app].checks[readiness].type: must be http or tcp",
		},
		{
			name: "http check without method",
			mutate: func(m *Metadata) {
				mutateCheck(m, "app", "readiness", func(c *Check) { c.Method = "" })
			},
			expected: "processes[app].checks[readiness].method: required for http checks",
		},
		{
			name: "http check path without slash",
			mutate: func(m *Metadata) {
				mutateCheck(m, "app", "readiness", func(c *Check) { c.Path = "health" })
			},
			expected: "processes[app].checks[readiness].path: must start with / for http checks",
		},
		{
			name: "check port zero",
			mutate: func(m *Metadata) {
				mutateCheck(m, "app", "readiness", func(c *Check) { c.Port = 0 })
			},
			expected: "processes[app].checks[readiness].port: must be between 1 and 65535",
		},
		{
			name: "check interval zero",
			mutate: func(m *Metadata) {
				mutateCheck(m, "app", "readiness", func(c *Check) { c.IntervalSeconds = 0 })
			},
			expected: "processes[app].checks[readiness].interval_seconds: must be > 0",
		},
		{
			name: "check timeout zero",
			mutate: func(m *Metadata) {
				mutateCheck(m, "app", "readiness", func(c *Check) { c.TimeoutSeconds = 0 })
			},
			expected: "processes[app].checks[readiness].timeout_seconds: must be > 0",
		},
		{
			name: "check timeout not below interval",
			mutate: func(m *Metadata) {
				mutateCheck(m, "app", "readiness", func(c *Check) { c.TimeoutSeconds = 10 })
			},
			expected: "processes[app].checks[readiness].timeout_seconds: must be < interval_seconds",
		},
		{
			name: "invalid restart policy",
			mutate: func(m *Metadata) {
				mutateProcess(m, "worker", func(p *Process) { p.Restart = "sometimes" })
			},
			expected: "processes[worker].restart: must be one of",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			metadata := happyMetadata()
			containers := happyContainers()
			if c.containers != nil {
				containers = c.containers
			}
			c.mutate(metadata)

			err := Validate(metadata, containers)

			assert.Error(t, err)
			assert.Contains(t, err.Error(), c.expected)
		})
	}
}

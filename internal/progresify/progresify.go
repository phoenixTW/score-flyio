package progresify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const MetadataKey = "progresify"

const DefaultRegion = "iad"

var processNameReg = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

var validRestartPolicies = []string{"always", "no", "on-failure"}

var validIngressTypes = []string{"none", "private", "cloudflare"}

var validPortHandlers = []string{"http", "tls", "http+tls"}

// Metadata is the typed contract for the metadata.progresify workload section.
type Metadata struct {
	Owner           string             `json:"owner"`
	SlackChannel    string             `json:"slack_channel"`
	SecretNamespace string             `json:"secret_namespace"`
	Region          string             `json:"region"`
	Ingress         *Ingress           `json:"ingress"`
	ReleaseCommand  []string           `json:"release_command"`
	Variables       map[string]string  `json:"variables"`
	Processes       map[string]Process `json:"processes"`
}

// Ingress describes how external traffic reaches the workload.
type Ingress struct {
	Type     string `json:"type"`
	Hostname string `json:"hostname"`
	Tunnel   string `json:"tunnel"`
}

// Process configures one Score container inside a machine group.
type Process struct {
	MachineGroup string           `json:"machine_group"`
	Vm           *Vm              `json:"vm"`
	Scale        *Scale           `json:"scale"`
	HttpService  *HttpService     `json:"http_service"`
	Concurrency  map[string]any   `json:"concurrency"`
	Checks       map[string]Check `json:"checks"`
	Restart      string           `json:"restart"`
}

// Vm sets machine guest resources for a process.
type Vm struct {
	Cpus     int `json:"cpus"`
	MemoryMb int `json:"memory_mb"`
}

// Scale bounds the machine count of a machine group.
type Scale struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// HttpService exposes one container port through a Fly machine service.
type HttpService struct {
	InternalPort       int           `json:"internal_port"`
	Ports              []ServicePort `json:"ports"`
	Protocol           string        `json:"protocol"`
	AutoStop           string        `json:"auto_stop"`
	MinMachinesRunning int           `json:"min_machines_running"`
}

// ServicePort maps an external port with optional protocol handlers.
type ServicePort struct {
	Port     int      `json:"port"`
	Handlers []string `json:"handlers"`
}

// Check defines an http or tcp health check for a process.
type Check struct {
	Type               string            `json:"type"`
	Port               int               `json:"port"`
	Method             string            `json:"method"`
	Path               string            `json:"path"`
	Headers            map[string]string `json:"headers"`
	IntervalSeconds    int               `json:"interval_seconds"`
	TimeoutSeconds     int               `json:"timeout_seconds"`
	GracePeriodSeconds int               `json:"grace_period_seconds"`
}

// Parse decodes the raw metadata.progresify section, returning nil when absent.
func Parse(raw map[string]any) (*Metadata, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	rawJson, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("metadata.%s: failed to encode: %w", MetadataKey, err)
	}
	dec := json.NewDecoder(bytes.NewReader(rawJson))
	dec.DisallowUnknownFields()
	var out Metadata
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("metadata.%s: %w", MetadataKey, err)
	}
	return &out, nil
}

// Validate checks the contract against the workload's container names.
func Validate(m *Metadata, containerNames []string) error {
	if m == nil {
		return nil
	}
	var errs []error
	containers := make(map[string]struct{}, len(containerNames))
	for _, name := range containerNames {
		containers[name] = struct{}{}
	}

	names := make([]string, 0, len(m.Processes))
	for name := range m.Processes {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		if !processNameReg.MatchString(name) {
			errs = append(errs, fmt.Errorf("processes[%s]: name must match %s", name, processNameReg.String()))
		}
		if _, ok := containers[name]; !ok {
			errs = append(errs, fmt.Errorf("process '%s' has no matching Score container", name))
		}
	}

	sortedContainers := slices.Clone(containerNames)
	slices.Sort(sortedContainers)
	for _, name := range sortedContainers {
		if _, ok := m.Processes[name]; !ok {
			errs = append(errs, fmt.Errorf("container '%s' has no configuration in metadata.%s.processes", name, MetadataKey))
		}
	}

	firstInGroup := make(map[string]string, len(names))
	for _, name := range names {
		p := m.Processes[name]
		group := p.MachineGroup
		if group == "" {
			group = name
		}
		if first, ok := firstInGroup[group]; ok {
			other := m.Processes[first]
			if !vmEqual(p.Vm, other.Vm) || !scaleEqual(p.Scale, other.Scale) || p.Restart != other.Restart {
				errs = append(errs, fmt.Errorf("machine group '%s': process '%s' must share vm, scale, and restart with process '%s'", group, name, first))
			}
		} else {
			firstInGroup[group] = name
		}
	}

	if m.Ingress != nil {
		errs = append(errs, validateIngress(m.Ingress)...)
	}

	for _, name := range names {
		errs = append(errs, validateProcess(name, m.Processes[name])...)
	}

	return errors.Join(errs...)
}

func validateIngress(ingress *Ingress) []error {
	var errs []error
	if ingress.Type != "" && !slices.Contains(validIngressTypes, ingress.Type) {
		errs = append(errs, fmt.Errorf("ingress.type: must be one of %s", strings.Join(validIngressTypes, ", ")))
	}
	if ingress.Type == "cloudflare" && ingress.Hostname == "" {
		errs = append(errs, fmt.Errorf("ingress.type=cloudflare requires a hostname"))
	}
	if ingress.Type == "private" && ingress.Hostname != "" {
		errs = append(errs, fmt.Errorf("ingress.type=private must not set a hostname"))
	}
	if ingress.Type == "none" && (ingress.Hostname != "" || ingress.Tunnel != "") {
		errs = append(errs, fmt.Errorf("ingress.type=none must not set a hostname or tunnel"))
	}
	return errs
}

func validateProcess(name string, p Process) []error {
	var errs []error
	if p.Restart != "" && !slices.Contains(validRestartPolicies, p.Restart) {
		errs = append(errs, fmt.Errorf("processes[%s].restart: must be one of %s", name, strings.Join(validRestartPolicies, ", ")))
	}
	if p.Vm != nil {
		if p.Vm.Cpus < 1 {
			errs = append(errs, fmt.Errorf("processes[%s].vm.cpus: must be >= 1", name))
		}
		if p.Vm.MemoryMb < 256 || p.Vm.MemoryMb%256 != 0 {
			errs = append(errs, fmt.Errorf("processes[%s].vm.memory_mb: must be >= 256 and a multiple of 256", name))
		}
	}
	if p.Scale != nil {
		if p.Scale.Min < 0 {
			errs = append(errs, fmt.Errorf("processes[%s].scale.min: must be >= 0", name))
		}
		if p.Scale.Max < p.Scale.Min {
			errs = append(errs, fmt.Errorf("processes[%s].scale.max: must be >= min", name))
		}
		if p.Scale.Max > 10 {
			errs = append(errs, fmt.Errorf("processes[%s].scale.max: must be <= 10", name))
		}
	}
	if p.HttpService != nil {
		errs = append(errs, validateHttpService(name, p.HttpService)...)
	}
	checkNames := make([]string, 0, len(p.Checks))
	for checkName := range p.Checks {
		checkNames = append(checkNames, checkName)
	}
	slices.Sort(checkNames)
	for _, checkName := range checkNames {
		errs = append(errs, validateCheck(name, checkName, p.Checks[checkName])...)
	}
	return errs
}

func validateHttpService(processName string, svc *HttpService) []error {
	var errs []error
	if svc.InternalPort < 1 || svc.InternalPort > 65535 {
		errs = append(errs, fmt.Errorf("processes[%s].http_service.internal_port: must be between 1 and 65535", processName))
	}
	for i, port := range svc.Ports {
		if port.Port < 1 || port.Port > 65535 {
			errs = append(errs, fmt.Errorf("processes[%s].http_service.ports[%d].port: must be between 1 and 65535", processName, i))
		}
		for _, handler := range port.Handlers {
			if !slices.Contains(validPortHandlers, handler) {
				errs = append(errs, fmt.Errorf("processes[%s].http_service.ports[%d].handlers: '%s' must be one of %s", processName, i, handler, strings.Join(validPortHandlers, ", ")))
			}
		}
	}
	return errs
}

func validateCheck(processName, checkName string, c Check) []error {
	var errs []error
	if c.Type != "http" && c.Type != "tcp" {
		errs = append(errs, fmt.Errorf("processes[%s].checks[%s].type: must be http or tcp", processName, checkName))
	}
	if c.Type == "http" {
		if c.Method == "" {
			errs = append(errs, fmt.Errorf("processes[%s].checks[%s].method: required for http checks", processName, checkName))
		}
		if !strings.HasPrefix(c.Path, "/") {
			errs = append(errs, fmt.Errorf("processes[%s].checks[%s].path: must start with / for http checks", processName, checkName))
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("processes[%s].checks[%s].port: must be between 1 and 65535", processName, checkName))
	}
	if c.IntervalSeconds <= 0 {
		errs = append(errs, fmt.Errorf("processes[%s].checks[%s].interval_seconds: must be > 0", processName, checkName))
	}
	if c.TimeoutSeconds <= 0 {
		errs = append(errs, fmt.Errorf("processes[%s].checks[%s].timeout_seconds: must be > 0", processName, checkName))
	}
	if c.IntervalSeconds > 0 && c.TimeoutSeconds > 0 && c.TimeoutSeconds >= c.IntervalSeconds {
		errs = append(errs, fmt.Errorf("processes[%s].checks[%s].timeout_seconds: must be < interval_seconds", processName, checkName))
	}
	return errs
}

func vmEqual(a, b *Vm) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func scaleEqual(a, b *Scale) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

package flymetadata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const MetadataKey = "fly"

const DefaultRegion = "iad"

var processNameReg = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

var validRestartPolicies = []string{"always", "no", "on-failure"}

var validProfiles = []string{"service", "worker", "cron"}

var validIngressTypes = []string{"none", "private", "public"}

var validPortHandlers = []string{"http", "tls", "http+tls"}

var validConcurrencyKeys = []string{"type", "hard_limit", "soft_limit"}

// Metadata is the typed contract for the metadata.fly workload section.
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
	MachineGroup string            `json:"machine_group"`
	Profile      string            `json:"profile"`
	Image        string            `json:"image"`
	Command      []string          `json:"command"`
	Args         []string          `json:"args"`
	Variables    map[string]string `json:"variables"`
	Vm           *Vm               `json:"vm"`
	Scale        *Scale            `json:"scale"`
	HttpService  *HttpService      `json:"http_service"`
	Concurrency  map[string]any    `json:"concurrency"`
	Checks       map[string]Check  `json:"checks"`
	Restart      string            `json:"restart"`
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
	InternalPort       int              `json:"internal_port"`
	Ports              []ServicePort    `json:"ports"`
	Protocol           string           `json:"protocol"`
	AutoStop           string           `json:"auto_stop"`
	AutoStart          *bool            `json:"auto_start"`
	MinMachinesRunning int              `json:"min_machines_running"`
	Checks             map[string]Check `json:"checks"`
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

// Parse decodes the raw metadata.fly section, returning nil when absent.
func Parse(raw map[string]any) (*Metadata, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if hasPublishedShape(raw) {
		var err error
		raw, err = normalizePublishedShape(raw)
		if err != nil {
			return nil, fmt.Errorf("metadata.%s: %w", MetadataKey, err)
		}
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

func hasPublishedShape(raw map[string]any) bool {
	_, primaryRegion := raw["primaryRegion"]
	_, releaseCommand := raw["releaseCommand"]
	_, secrets := raw["secrets"]
	_, slack := raw["slack"]
	return primaryRegion || releaseCommand || secrets || slack
}

func normalizePublishedShape(raw map[string]any) (map[string]any, error) {
	allowed := map[string]bool{
		"owner": true, "slack": true, "slack_channel": true, "secrets": true, "secret_namespace": true,
		"primaryRegion": true, "region": true, "ingress": true, "releaseCommand": true, "release_command": true,
		"variables": true, "processes": true,
	}
	for key := range raw {
		if !allowed[key] {
			return nil, fmt.Errorf("unknown field %q", key)
		}
	}
	out := map[string]any{}
	copyField(out, raw, "owner", "owner")
	copyField(out, raw, "slack_channel", "slack")
	copyField(out, raw, "slack_channel", "slack_channel")
	copyField(out, raw, "region", "primaryRegion")
	copyField(out, raw, "region", "region")
	if value, ok := raw["ingress"]; ok {
		if ingress, ok := value.(string); ok {
			out["ingress"] = map[string]any{"type": ingress}
		} else {
			out["ingress"] = value
		}
	}
	copyField(out, raw, "variables", "variables")
	if value, ok := raw["secret_namespace"]; ok {
		out["secret_namespace"] = value
	} else if value, ok := raw["secrets"]; ok {
		namespace, err := publishedSecretNamespace(value)
		if err != nil {
			return nil, err
		}
		out["secret_namespace"] = namespace
	}
	if value, ok := raw["release_command"]; ok {
		out["release_command"] = value
	} else if value, ok := raw["releaseCommand"]; ok {
		command, err := publishedCommand(value)
		if err != nil {
			return nil, fmt.Errorf("releaseCommand: %w", err)
		}
		out["release_command"] = command
	}
	if value, ok := raw["processes"]; ok {
		processes, err := normalizePublishedProcesses(value)
		if err != nil {
			return nil, err
		}
		out["processes"] = processes
	}
	return out, nil
}

func copyField(out, raw map[string]any, destination, source string) {
	if value, ok := raw[source]; ok {
		out[destination] = value
	}
}

func publishedSecretNamespace(value any) (string, error) {
	secrets, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("secrets must be an object")
	}
	for key := range secrets {
		if key != "provider" && key != "coords" {
			return "", fmt.Errorf("secrets: unknown field %q", key)
		}
	}
	provider, _ := secrets["provider"].(string)
	coords, _ := secrets["coords"].(map[string]any)
	if provider == "" {
		return "", fmt.Errorf("secrets.provider must not be empty")
	}
	if environment, ok := coords["environment"].(string); ok && environment != "" {
		return provider + ":" + environment, nil
	}
	return provider, nil
}

func publishedCommand(value any) ([]string, error) {
	switch command := value.(type) {
	case string:
		if strings.TrimSpace(command) == "" {
			return nil, nil
		}
		return strings.Fields(command), nil
	case []any:
		out := make([]string, 0, len(command))
		for _, item := range command {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("command entries must be strings")
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("command must be a string or list")
	}
}

func normalizePublishedProcesses(value any) (map[string]any, error) {
	processes, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("processes must be an object")
	}
	out := make(map[string]any, len(processes))
	for name, value := range processes {
		process, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("processes.%s must be an object", name)
		}
		canonical, err := normalizePublishedProcess(name, process)
		if err != nil {
			return nil, err
		}
		out[name] = canonical
	}
	return out, nil
}

func normalizePublishedProcess(name string, raw map[string]any) (map[string]any, error) {
	allowed := map[string]bool{"profile": true, "command": true, "args": true, "image": true, "machineGroup": true, "machine_group": true, "variables": true, "vm": true, "scale": true, "httpService": true, "http_service": true, "concurrency": true, "checks": true, "restart": true}
	for key := range raw {
		if !allowed[key] {
			return nil, fmt.Errorf("processes.%s: unknown field %q", name, key)
		}
	}
	out := map[string]any{}
	copyField(out, raw, "profile", "profile")
	copyField(out, raw, "image", "image")
	copyField(out, raw, "variables", "variables")
	copyField(out, raw, "restart", "restart")
	if value, ok := raw["machine_group"]; ok {
		out["machine_group"] = value
	} else if value, ok := raw["machineGroup"]; ok {
		out["machine_group"] = value
	}
	if value, ok := raw["command"]; ok {
		command, err := publishedCommand(value)
		if err != nil {
			return nil, fmt.Errorf("processes.%s.command: %w", name, err)
		}
		out["command"] = command
	}
	if value, ok := raw["args"]; ok {
		args, err := publishedCommand(value)
		if err != nil {
			return nil, fmt.Errorf("processes.%s.args: %w", name, err)
		}
		out["args"] = args
	}
	if value, ok := raw["vm"]; ok {
		vm, err := normalizePublishedVM(value)
		if err != nil {
			return nil, fmt.Errorf("processes.%s.vm: %w", name, err)
		}
		out["vm"] = vm
	}
	if value, ok := raw["scale"]; ok {
		out["scale"] = value
	}
	serviceValue, serviceOK := raw["http_service"]
	if !serviceOK {
		serviceValue, serviceOK = raw["httpService"]
	}
	if serviceOK {
		service, err := normalizePublishedHTTPService(serviceValue, name)
		if err != nil {
			return nil, err
		}
		out["http_service"] = service
		if _, exists := out["concurrency"]; !exists {
			if concurrency, exists := service["concurrency"]; exists {
				out["concurrency"] = concurrency
			}
		}
	}
	if value, ok := raw["concurrency"]; ok {
		out["concurrency"] = normalizePublishedConcurrency(value)
	}
	if value, ok := raw["checks"]; ok {
		out["checks"] = value
	}
	return out, nil
}

func normalizePublishedVM(value any) (map[string]any, error) {
	vm, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be an object")
	}
	out := map[string]any{}
	if cpus, ok := vm["cpus"]; ok {
		out["cpus"] = cpus
	} else if size, ok := vm["size"].(string); ok {
		parts := strings.Split(size, "-")
		last := parts[len(parts)-1]
		last = strings.TrimSuffix(last, "x")
		cpus, err := strconv.Atoi(last)
		if err != nil || cpus < 1 {
			return nil, fmt.Errorf("size %q is invalid", size)
		}
		out["cpus"] = cpus
	}
	if memory, ok := vm["memory_mb"]; ok {
		out["memory_mb"] = memory
	} else if memory, ok := vm["memory"].(string); ok {
		value, err := parseMemoryMB(memory)
		if err != nil {
			return nil, err
		}
		out["memory_mb"] = value
	}
	return out, nil
}

func parseMemoryMB(value string) (int, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.HasSuffix(value, "gb") {
		n, err := strconv.Atoi(strings.TrimSuffix(value, "gb"))
		return n * 1024, err
	}
	if strings.HasSuffix(value, "mb") {
		return strconv.Atoi(strings.TrimSuffix(value, "mb"))
	}
	return 0, fmt.Errorf("memory %q must use mb or gb", value)
}

func normalizePublishedHTTPService(value any, process string) (map[string]any, error) {
	service, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("processes.%s.httpService must be an object", process)
	}
	out := map[string]any{}
	for source, destination := range map[string]string{"internalPort": "internal_port", "ports": "ports", "protocol": "protocol", "autoStartMachines": "auto_start", "minMachinesRunning": "min_machines_running"} {
		if value, ok := service[source]; ok {
			out[destination] = value
		}
	}
	if value, ok := service["autoStopMachines"]; ok {
		if enabled, ok := value.(bool); ok {
			if enabled {
				out["auto_stop"] = "stop"
			} else {
				out["auto_stop"] = "off"
			}
		} else {
			out["auto_stop"] = value
		}
	}
	if value, ok := service["concurrency"]; ok {
		out["concurrency"] = normalizePublishedConcurrency(value)
	}
	if value, ok := service["checks"]; ok {
		checks, err := normalizePublishedChecks(value)
		if err != nil {
			return nil, fmt.Errorf("processes.%s.httpService.checks: %w", process, err)
		}
		if port, ok := service["internalPort"].(int); ok {
			for _, check := range checks {
				if checkMap, ok := check.(map[string]any); ok {
					if _, exists := checkMap["port"]; !exists {
						checkMap["port"] = port
					}
				}
			}
		}
		out["checks"] = checks
	}
	return out, nil
}

func normalizePublishedConcurrency(value any) map[string]any {
	input, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]any{}
	for source, destination := range map[string]string{"type": "type", "softLimit": "soft_limit", "hardLimit": "hard_limit", "soft_limit": "soft_limit", "hard_limit": "hard_limit"} {
		if value, ok := input[source]; ok {
			out[destination] = value
		}
	}
	return out
}

func normalizePublishedChecks(value any) (map[string]any, error) {
	checks, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be an object")
	}
	out := map[string]any{}
	for name, value := range checks {
		check, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object", name)
		}
		canonical := map[string]any{"type": "http"}
		for source, destination := range map[string]string{"type": "type", "port": "port", "method": "method", "path": "path", "headers": "headers", "intervalSeconds": "interval_seconds", "timeoutSeconds": "timeout_seconds", "gracePeriodSeconds": "grace_period_seconds"} {
			if value, ok := check[source]; ok {
				canonical[destination] = value
			}
		}
		if interval, ok := check["interval"].(string); ok {
			canonical["interval_seconds"] = durationSeconds(interval)
		}
		if timeout, ok := check["timeout"].(string); ok {
			canonical["timeout_seconds"] = durationSeconds(timeout)
		}
		if grace, ok := check["gracePeriod"].(string); ok {
			canonical["grace_period_seconds"] = durationSeconds(grace)
		}
		out[name] = canonical
	}
	return out, nil
}

func durationSeconds(value string) int {
	value = strings.TrimSpace(strings.TrimSuffix(value, "s"))
	seconds, _ := strconv.Atoi(value)
	return seconds
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
	if p.Profile != "" && !slices.Contains(validProfiles, p.Profile) {
		errs = append(errs, fmt.Errorf("processes[%s].profile: must be one of %s", name, strings.Join(validProfiles, ", ")))
	}
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
	}
	if p.HttpService != nil {
		errs = append(errs, validateHttpService(name, p.HttpService)...)
		for checkName, check := range p.HttpService.Checks {
			errs = append(errs, validateCheck(name, checkName, check)...)
		}
	}
	concurrencyKeys := make([]string, 0, len(p.Concurrency))
	for key := range p.Concurrency {
		concurrencyKeys = append(concurrencyKeys, key)
	}
	slices.Sort(concurrencyKeys)
	for _, key := range concurrencyKeys {
		if !slices.Contains(validConcurrencyKeys, key) {
			errs = append(errs, fmt.Errorf("processes[%s].concurrency: key '%s' must be one of %s", name, key, strings.Join(validConcurrencyKeys, ", ")))
		}
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

// Package machineconfig models multi-container Fly Machines deployment plans.
// It converts typed Score-driven groups into Fly Machines API configuration.
package machineconfig

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/astromechza/score-flyio/internal"
	"github.com/astromechza/score-flyio/internal/flymachines"
)

const ContainerNamePattern = `^[a-z][a-z0-9-]{0,62}$`

const (
	RestartPolicyAlways    = "always"
	RestartPolicyNo        = "no"
	RestartPolicyOnFailure = "on-failure"
)

const (
	DependencyConditionStarted            = "started"
	DependencyConditionHealthy            = "healthy"
	DependencyConditionExitedSuccessfully = "exited_successfully"
)

const (
	AutoStopOff     = "off"
	AutoStopStop    = "stop"
	AutoStopSuspend = "suspend"
)

const (
	CheckTypeHTTP = "http"
	CheckTypeTCP  = "tcp"
)

// MetadataIngressHostname carries the external ingress hostname served
// by a machine group.
const MetadataIngressHostname = "flydeploy.ingress-hostname"

var (
	containerNameRegexp = regexp.MustCompile(ContainerNamePattern)
	regionRegexp        = regexp.MustCompile(`^[a-z]{2,4}$`)
	signalRegexp        = regexp.MustCompile(`^SIG[A-Z]+$`)
	imageDigestRegexp   = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
)

// Plan is the full desired state for one Score workload on Fly Machines.
type Plan struct {
	AppName         string   `json:"app_name,omitempty"`
	RendererVersion string   `json:"renderer_version,omitempty"`
	Workload        string   `json:"workload,omitempty"`
	Environment     string   `json:"environment,omitempty"`
	ReleaseCommand  []string `json:"release_command,omitempty"`
	Groups          []Group  `json:"groups"`
}

// Group is one machine group: colocated containers sharing scale and lifecycle.
type Group struct {
	Name               string            `json:"name"`
	Region             string            `json:"region,omitempty"`
	Guest              *Guest            `json:"guest,omitempty"`
	MinMachines        int               `json:"min_machines,omitempty"`
	MaxMachines        int               `json:"max_machines,omitempty"`
	Restart            string            `json:"restart,omitempty"`
	Schedule           string            `json:"schedule,omitempty"`
	AutoDestroy        bool              `json:"auto_destroy,omitempty"`
	StopSignal         string            `json:"stop_signal,omitempty"`
	StopTimeoutSeconds int               `json:"stop_timeout_seconds,omitempty"`
	Services           []Service         `json:"services,omitempty"`
	Checks             map[string]Check  `json:"checks,omitempty"`
	Metrics            *Metrics          `json:"metrics,omitempty"`
	Volumes            []Volume          `json:"volumes,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Containers         []Container       `json:"containers"`
}

// Guest describes machine level CPU and memory resources.
type Guest struct {
	CpuKind  string `json:"cpu_kind,omitempty"`
	CpuArch  string `json:"cpu_arch,omitempty"`
	Cpus     int    `json:"cpus"`
	MemoryMb int    `json:"memory_mb"`
}

// Volume declares a named volume attachable by container mounts.
type Volume struct {
	Name   string `json:"name"`
	SizeGb int    `json:"size_gb,omitempty"`
}

// Container is one container inside a machine group.
type Container struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	ImageDigest string            `json:"image_digest,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Files       []File            `json:"files,omitempty"`
	Mounts      []Mount           `json:"mounts,omitempty"`
	Restart     string            `json:"restart,omitempty"`
	DependsOn   []Dependency      `json:"depends_on,omitempty"`
}

// File writes base64 content at GuestPath inside the container.
type File struct {
	GuestPath  string `json:"guest_path"`
	RawContent string `json:"raw_content,omitempty"`
}

// Mount attaches a group volume at Path inside the container.
type Mount struct {
	Volume string `json:"volume"`
	Path   string `json:"path"`
}

// Dependency orders container startup relative to a sibling container.
type Dependency struct {
	Name      string `json:"name"`
	Condition string `json:"condition"`
}

// Service exposes a machine group over the Fly network edge.
type Service struct {
	Protocol           string             `json:"protocol,omitempty"`
	InternalPort       int                `json:"internal_port"`
	Ports              []ServicePort      `json:"ports,omitempty"`
	AutoStop           string             `json:"auto_stop,omitempty"`
	AutoStart          bool               `json:"auto_start,omitempty"`
	MinMachinesRunning int                `json:"min_machines_running,omitempty"`
	Concurrency        map[string]any     `json:"concurrency,omitempty"`
	Checks             []ServiceHttpCheck `json:"checks,omitempty"`
}

// ServicePort maps an edge port with handlers to the service.
type ServicePort struct {
	Port     int      `json:"port"`
	Handlers []string `json:"handlers,omitempty"`
}

// ServiceHttpCheck probes the service HTTP endpoint for readiness.
type ServiceHttpCheck struct {
	Method             string            `json:"method,omitempty"`
	Path               string            `json:"path,omitempty"`
	Protocol           string            `json:"protocol,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	TlsServerName      string            `json:"tls_server_name,omitempty"`
	TlsSkipVerify      bool              `json:"tls_skip_verify,omitempty"`
	IntervalSeconds    int               `json:"interval_seconds,omitempty"`
	TimeoutSeconds     int               `json:"timeout_seconds,omitempty"`
	GracePeriodSeconds int               `json:"grace_period_seconds,omitempty"`
}

// Check is a machine level tcp or http health check.
type Check struct {
	Type               string            `json:"type"`
	Port               int               `json:"port"`
	Method             string            `json:"method,omitempty"`
	Path               string            `json:"path,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	IntervalSeconds    int               `json:"interval_seconds,omitempty"`
	TimeoutSeconds     int               `json:"timeout_seconds,omitempty"`
	GracePeriodSeconds int               `json:"grace_period_seconds,omitempty"`
}

// Metrics configures the Prometheus scrape endpoint of the group.
type Metrics struct {
	Https bool   `json:"https,omitempty"`
	Path  string `json:"path"`
	Port  int    `json:"port"`
}

// ValidateContainerName rejects empty, duplicate prone, or invalid names.
func ValidateContainerName(name string) error {
	if name == "" {
		return errors.New("container name must not be empty")
	}
	if !containerNameRegexp.MatchString(name) {
		return fmt.Errorf("container name '%s' must match %s", name, ContainerNamePattern)
	}
	return nil
}

// Validate checks plan level invariants including deterministic ordering.
func (p *Plan) Validate() error {
	if p.AppName == "" {
		return errors.New("app_name must not be empty")
	}
	if p.Workload == "" {
		return errors.New("workload must not be empty")
	}
	if len(p.Groups) == 0 {
		return errors.New("groups must not be empty")
	}
	for _, part := range p.ReleaseCommand {
		if part == "" {
			return errors.New("release_command must not contain empty elements")
		}
	}
	for i := range p.Groups {
		if i > 0 && p.Groups[i].Name <= p.Groups[i-1].Name {
			return fmt.Errorf("groups[%d] name '%s' must sort after '%s'", i, p.Groups[i].Name, p.Groups[i-1].Name)
		}
	}
	for i := range p.Groups {
		if err := p.Groups[i].validate(fmt.Sprintf("groups[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// ValidateImmutableImages enforces the release-time image contract.
// Structural validation stays separate; mutation paths call this gate.
func (p *Plan) ValidateImmutableImages() error {
	if p == nil {
		return errors.New("plan must not be nil")
	}
	for _, group := range p.Groups {
		for _, container := range group.Containers {
			if container.ImageDigest == "" || !imageDigestRegexp.MatchString(container.ImageDigest) || !strings.Contains(container.Image, "@"+container.ImageDigest) {
				return fmt.Errorf("group %s container %s: image must be pinned to an immutable sha256 digest", group.Name, container.Name)
			}
		}
	}
	return nil
}

func (g *Group) validate(path string) error {
	if err := ValidateContainerName(g.Name); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if g.Region != "" && !regionRegexp.MatchString(g.Region) {
		return fmt.Errorf("%s: region '%s' must be 2-4 lowercase letters", path, g.Region)
	}
	if g.Restart != "" && !allowedRestart(g.Restart) {
		return fmt.Errorf("%s: restart '%s' must be one of always, no, on-failure", path, g.Restart)
	}
	if g.StopTimeoutSeconds < 0 {
		return fmt.Errorf("%s: stop_timeout_seconds must not be negative", path)
	}
	if g.StopSignal != "" && !signalRegexp.MatchString(g.StopSignal) {
		return fmt.Errorf("%s: stop_signal '%s' must be an uppercase signal name like SIGTERM", path, g.StopSignal)
	}
	if g.Guest != nil {
		if g.Guest.CpuKind != "" && g.Guest.CpuKind != "shared" && g.Guest.CpuKind != "performance" {
			return fmt.Errorf("%s: guest cpu_kind '%s' must be shared or performance", path, g.Guest.CpuKind)
		}
		if g.Guest.CpuArch != "" && g.Guest.CpuArch != "amd64" && g.Guest.CpuArch != "arm64" {
			return fmt.Errorf("%s: guest cpu_arch '%s' must be amd64 or arm64", path, g.Guest.CpuArch)
		}
		if g.Guest.Cpus < 1 {
			return fmt.Errorf("%s: guest cpus must be at least 1", path)
		}
		if g.Guest.MemoryMb < 256 {
			return fmt.Errorf("%s: guest memory_mb must be at least 256", path)
		}
		if g.Guest.MemoryMb%256 != 0 {
			return fmt.Errorf("%s: guest memory_mb %d must be a multiple of 256", path, g.Guest.MemoryMb)
		}
	}
	if g.MinMachines < 0 {
		return fmt.Errorf("%s: min_machines must not be negative", path)
	}
	if g.MaxMachines < g.MinMachines {
		return fmt.Errorf("%s: max_machines %d must be at least min_machines %d", path, g.MaxMachines, g.MinMachines)
	}
	if g.MaxMachines > 10 {
		return fmt.Errorf("%s: max_machines %d must not exceed 10", path, g.MaxMachines)
	}
	for i := range g.Volumes {
		if i > 0 && g.Volumes[i].Name <= g.Volumes[i-1].Name {
			return fmt.Errorf("%s: volumes[%d] name '%s' must sort after '%s'", path, i, g.Volumes[i].Name, g.Volumes[i-1].Name)
		}
		if err := ValidateContainerName(g.Volumes[i].Name); err != nil {
			return fmt.Errorf("%s: volumes[%d]: %w", path, i, err)
		}
	}
	volumeNames := make(map[string]bool, len(g.Volumes))
	for _, v := range g.Volumes {
		volumeNames[v.Name] = true
	}
	if len(g.Containers) == 0 {
		return fmt.Errorf("%s: containers must not be empty", path)
	}
	containerNames := make(map[string]bool, len(g.Containers))
	for _, c := range g.Containers {
		containerNames[c.Name] = true
	}
	for i := range g.Containers {
		c := &g.Containers[i]
		if i > 0 && c.Name <= g.Containers[i-1].Name {
			return fmt.Errorf("%s: containers[%d] name '%s' must sort after '%s'", path, i, c.Name, g.Containers[i-1].Name)
		}
		if err := ValidateContainerName(c.Name); err != nil {
			return fmt.Errorf("%s: containers[%d]: %w", path, i, err)
		}
		if c.Image == "" {
			return fmt.Errorf("%s: containers[%d] image must not be empty", path, i)
		}
		if c.Restart != "" && !allowedRestart(c.Restart) {
			return fmt.Errorf("%s: containers[%d] restart '%s' must be one of always, no, on-failure", path, i, c.Restart)
		}
		for j, f := range c.Files {
			if f.GuestPath == "" || f.GuestPath[0] != '/' {
				return fmt.Errorf("%s: containers[%d] files[%d] guest_path '%s' must be absolute", path, i, j, f.GuestPath)
			}
		}
		for j, m := range c.Mounts {
			if !volumeNames[m.Volume] {
				return fmt.Errorf("%s: containers[%d] mounts[%d] references undeclared volume '%s'", path, i, j, m.Volume)
			}
			if m.Path == "" || m.Path[0] != '/' {
				return fmt.Errorf("%s: containers[%d] mounts[%d] path '%s' must be absolute", path, i, j, m.Path)
			}
		}
		for j, d := range c.DependsOn {
			if d.Name == c.Name {
				return fmt.Errorf("%s: containers[%d] depends_on[%d] must not reference itself", path, i, j)
			}
			if !containerNames[d.Name] {
				return fmt.Errorf("%s: containers[%d] depends_on[%d] references unknown sibling '%s'", path, i, j, d.Name)
			}
			if !allowedDependencyCondition(d.Condition) {
				return fmt.Errorf("%s: containers[%d] depends_on[%d] condition '%s' must be one of started, healthy, exited_successfully", path, i, j, d.Condition)
			}
		}
	}
	for i := range g.Services {
		s := &g.Services[i]
		if i > 0 && s.InternalPort <= g.Services[i-1].InternalPort {
			return fmt.Errorf("%s: services[%d] internal_port %d must sort above %d", path, i, s.InternalPort, g.Services[i-1].InternalPort)
		}
		if s.InternalPort < 1 || s.InternalPort > 65535 {
			return fmt.Errorf("%s: services[%d] internal_port %d must be between 1 and 65535", path, i, s.InternalPort)
		}
		if s.Protocol != "" && s.Protocol != "tcp" && s.Protocol != "udp" {
			return fmt.Errorf("%s: services[%d] protocol '%s' must be tcp or udp", path, i, s.Protocol)
		}
		if s.AutoStop != "" && !allowedAutoStop(s.AutoStop) {
			return fmt.Errorf("%s: services[%d] auto_stop '%s' must be one of off, stop, suspend", path, i, s.AutoStop)
		}
		for j, sp := range s.Ports {
			if sp.Port < 1 || sp.Port > 65535 {
				return fmt.Errorf("%s: services[%d] ports[%d] port %d must be between 1 and 65535", path, i, j, sp.Port)
			}
		}
		for j, hc := range s.Checks {
			if err := validateHttpCheck(hc); err != nil {
				return fmt.Errorf("%s: services[%d] checks[%d]: %w", path, i, j, err)
			}
		}
	}
	for name, c := range g.Checks {
		if name == "" {
			return fmt.Errorf("%s: checks keys must not be empty", path)
		}
		if c.Type != CheckTypeHTTP && c.Type != CheckTypeTCP {
			return fmt.Errorf("%s: checks[%s] type '%s' must be http or tcp", path, name, c.Type)
		}
		if c.Port < 1 || c.Port > 65535 {
			return fmt.Errorf("%s: checks[%s] port %d must be between 1 and 65535", path, name, c.Port)
		}
		if c.Type == CheckTypeHTTP && (c.Path == "" || c.Path[0] != '/') {
			return fmt.Errorf("%s: checks[%s] http path '%s' must start with /", path, name, c.Path)
		}
	}
	for k := range g.Metadata {
		if k == "" {
			return fmt.Errorf("%s: metadata keys must not be empty", path)
		}
	}
	return nil
}

func validateHttpCheck(hc ServiceHttpCheck) error {
	if hc.Path == "" || hc.Path[0] != '/' {
		return fmt.Errorf("http check path '%s' must start with /", hc.Path)
	}
	if hc.Protocol != "" && hc.Protocol != "http" && hc.Protocol != "https" {
		return fmt.Errorf("http check protocol '%s' must be http or https", hc.Protocol)
	}
	if hc.IntervalSeconds < 0 || hc.TimeoutSeconds < 0 || hc.GracePeriodSeconds < 0 {
		return errors.New("http check intervals must not be negative")
	}
	return nil
}

func allowedRestart(v string) bool {
	return v == RestartPolicyAlways || v == RestartPolicyNo || v == RestartPolicyOnFailure
}

func allowedAutoStop(v string) bool {
	return v == AutoStopOff || v == AutoStopStop || v == AutoStopSuspend
}

func allowedDependencyCondition(v string) bool {
	return v == DependencyConditionStarted || v == DependencyConditionHealthy || v == DependencyConditionExitedSuccessfully
}

// ToFlyMachineConfig converts the group into an API machine configuration.
func (g *Group) ToFlyMachineConfig() flymachines.FlyMachineConfig {
	containers := make([]flymachines.FlyContainerConfig, 0, len(g.Containers))
	for i := range g.Containers {
		containers = append(containers, g.Containers[i].toFlyContainerConfig())
	}
	out := flymachines.FlyMachineConfig{Containers: &containers}
	if g.Guest != nil {
		out.Guest = &flymachines.FlyMachineGuest{
			CpuKind:  internal.Ref(g.Guest.CpuKind),
			Cpus:     internal.Ref(g.Guest.Cpus),
			MemoryMb: internal.Ref(g.Guest.MemoryMb),
		}
	}
	if g.Restart != "" {
		out.Restart = &flymachines.FlyMachineRestart{Policy: internal.Ref(flymachines.FlyMachineRestartPolicy(g.Restart))}
	}
	if g.Schedule != "" {
		out.Schedule = internal.Ref(g.Schedule)
	}
	if g.AutoDestroy {
		out.AutoDestroy = internal.Ref(true)
	}
	if g.StopSignal != "" || g.StopTimeoutSeconds > 0 {
		sc := flymachines.FlyStopConfig{Signal: internal.Ref(g.StopSignal)}
		if g.StopTimeoutSeconds > 0 {
			sc.Timeout = &flymachines.FlyDuration{TimeDuration: internal.Ref(g.StopTimeoutSeconds * int(time.Second))}
		}
		out.StopConfig = &sc
	}
	if len(g.Volumes) > 0 {
		volumes := make([]flymachines.FlyVolumeConfig, 0, len(g.Volumes))
		for _, v := range g.Volumes {
			volumes = append(volumes, flymachines.FlyVolumeConfig{Name: internal.Ref(v.Name)})
		}
		out.Volumes = &volumes
	}
	if len(g.Services) > 0 {
		services := make([]flymachines.FlyMachineService, 0, len(g.Services))
		for i := range g.Services {
			services = append(services, g.Services[i].toFlyMachineService())
		}
		out.Services = &services
	}
	if len(g.Checks) > 0 {
		checks := make(map[string]flymachines.FlyMachineCheck, len(g.Checks))
		for name, c := range g.Checks {
			checks[name] = c.toFlyMachineCheck()
		}
		out.Checks = &checks
	}
	if g.Metrics != nil {
		out.Metrics = &flymachines.FlyMachineMetrics{
			Https: internal.Ref(g.Metrics.Https),
			Path:  internal.Ref(g.Metrics.Path),
			Port:  internal.Ref(g.Metrics.Port),
		}
	}
	if len(g.Metadata) > 0 {
		metadata := make(map[string]string, len(g.Metadata))
		for k, v := range g.Metadata {
			metadata[k] = v
		}
		out.Metadata = &metadata
	}
	return out
}

func (c *Container) toFlyContainerConfig() flymachines.FlyContainerConfig {
	out := flymachines.FlyContainerConfig{
		Name:  internal.Ref(c.Name),
		Image: internal.Ref(c.Image),
	}
	if len(c.Command) > 0 {
		command := append([]string(nil), c.Command...)
		out.Entrypoint = &command
	}
	if len(c.Args) > 0 {
		args := append([]string(nil), c.Args...)
		out.Cmd = &args
	}
	if len(c.Env) > 0 {
		env := make(map[string]string, len(c.Env))
		for k, v := range c.Env {
			env[k] = v
		}
		out.Env = &env
	}
	if len(c.Files) > 0 {
		files := make([]flymachines.FlyFile, 0, len(c.Files))
		for _, f := range c.Files {
			files = append(files, flymachines.FlyFile{GuestPath: internal.Ref(f.GuestPath), RawValue: internal.Ref(f.RawContent)})
		}
		slices.SortFunc(files, func(a, b flymachines.FlyFile) int {
			return strings.Compare(internal.DerefOrZero(a.GuestPath), internal.DerefOrZero(b.GuestPath))
		})
		out.Files = &files
	}
	if len(c.Mounts) > 0 {
		mounts := make([]flymachines.FlyContainerMount, 0, len(c.Mounts))
		for _, m := range c.Mounts {
			mounts = append(mounts, flymachines.FlyContainerMount{Name: internal.Ref(m.Volume), Path: internal.Ref(m.Path)})
		}
		slices.SortFunc(mounts, func(a, b flymachines.FlyContainerMount) int {
			return strings.Compare(internal.DerefOrZero(a.Path), internal.DerefOrZero(b.Path))
		})
		out.Mounts = &mounts
	}
	if c.Restart != "" {
		out.Restart = &flymachines.FlyMachineRestart{Policy: internal.Ref(flymachines.FlyMachineRestartPolicy(c.Restart))}
	}
	if len(c.DependsOn) > 0 {
		dependsOn := make([]flymachines.FlyContainerDependency, 0, len(c.DependsOn))
		for _, d := range c.DependsOn {
			dependsOn = append(dependsOn, flymachines.FlyContainerDependency{
				Name:      internal.Ref(d.Name),
				Condition: internal.Ref(flymachines.FlyContainerDependencyCondition(d.Condition)),
			})
		}
		slices.SortFunc(dependsOn, func(a, b flymachines.FlyContainerDependency) int {
			return strings.Compare(internal.DerefOrZero(a.Name), internal.DerefOrZero(b.Name))
		})
		out.DependsOn = &dependsOn
	}
	return out
}

func (s *Service) toFlyMachineService() flymachines.FlyMachineService {
	out := flymachines.FlyMachineService{InternalPort: internal.Ref(s.InternalPort)}
	if s.Protocol != "" {
		out.Protocol = internal.Ref(s.Protocol)
	}
	if len(s.Ports) > 0 {
		ports := make([]flymachines.FlyMachinePort, 0, len(s.Ports))
		for _, p := range s.Ports {
			port := flymachines.FlyMachinePort{Port: internal.Ref(p.Port)}
			if len(p.Handlers) > 0 {
				handlers := append([]string(nil), p.Handlers...)
				port.Handlers = &handlers
			}
			ports = append(ports, port)
		}
		slices.SortFunc(ports, func(a, b flymachines.FlyMachinePort) int {
			return cmp.Compare(internal.DerefOrZero(a.Port), internal.DerefOrZero(b.Port))
		})
		out.Ports = &ports
	}
	if s.AutoStop != "" {
		out.Autostop = internal.Ref(flymachines.FlyMachineServiceAutostop(s.AutoStop))
	}
	if s.AutoStart {
		out.Autostart = internal.Ref(true)
	}
	if s.MinMachinesRunning > 0 {
		out.MinMachinesRunning = internal.Ref(s.MinMachinesRunning)
	}
	if len(s.Concurrency) > 0 {
		out.Concurrency = concurrencyToFly(s.Concurrency)
	}
	if len(s.Checks) > 0 {
		checks := make([]flymachines.FlyMachineCheck, 0, len(s.Checks))
		for i := range s.Checks {
			checks = append(checks, s.Checks[i].toFlyMachineCheck())
		}
		out.Checks = &checks
	}
	return out
}

func concurrencyToFly(in map[string]any) *flymachines.FlyMachineServiceConcurrency {
	if len(in) == 0 {
		return nil
	}
	out := flymachines.FlyMachineServiceConcurrency{}
	if v, ok := in["type"].(string); ok {
		out.Type = internal.Ref(v)
	}
	if v, ok := numberValue(in["hard_limit"]); ok {
		out.HardLimit = internal.Ref(v)
	}
	if v, ok := numberValue(in["soft_limit"]); ok {
		out.SoftLimit = internal.Ref(v)
	}
	return &out
}

func numberValue(in any) (int, bool) {
	switch v := in.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}

func (c *Check) toFlyMachineCheck() flymachines.FlyMachineCheck {
	out := flymachines.FlyMachineCheck{
		Type: internal.Ref(c.Type),
		Port: internal.Ref(c.Port),
	}
	if c.Method != "" {
		out.Method = internal.Ref(c.Method)
	}
	if c.Path != "" {
		out.Path = internal.Ref(c.Path)
	}
	if len(c.Headers) > 0 {
		out.Headers = headersToFly(c.Headers)
	}
	if c.IntervalSeconds > 0 {
		out.Interval = internal.Ref(fmt.Sprintf("%ds", c.IntervalSeconds))
	}
	if c.TimeoutSeconds > 0 {
		out.Timeout = internal.Ref(fmt.Sprintf("%ds", c.TimeoutSeconds))
	}
	if c.GracePeriodSeconds > 0 {
		out.GracePeriod = &flymachines.FlyDuration{TimeDuration: internal.Ref(c.GracePeriodSeconds * int(time.Second))}
	}
	return out
}

func (h *ServiceHttpCheck) toFlyMachineCheck() flymachines.FlyMachineCheck {
	out := flymachines.FlyMachineCheck{Type: internal.Ref(CheckTypeHTTP)}
	if h.Method != "" {
		out.Method = internal.Ref(h.Method)
	} else {
		out.Method = internal.Ref("get")
	}
	if h.Path != "" {
		out.Path = internal.Ref(h.Path)
	}
	if h.Protocol != "" {
		out.Protocol = internal.Ref(h.Protocol)
	}
	if h.TlsServerName != "" {
		out.TlsServerName = internal.Ref(h.TlsServerName)
	}
	if h.TlsSkipVerify {
		out.TlsSkipVerify = internal.Ref(true)
	}
	if len(h.Headers) > 0 {
		out.Headers = headersToFly(h.Headers)
	}
	if h.IntervalSeconds > 0 {
		out.Interval = internal.Ref(fmt.Sprintf("%ds", h.IntervalSeconds))
	}
	if h.TimeoutSeconds > 0 {
		out.Timeout = internal.Ref(fmt.Sprintf("%ds", h.TimeoutSeconds))
	}
	if h.GracePeriodSeconds > 0 {
		out.GracePeriod = &flymachines.FlyDuration{TimeDuration: internal.Ref(h.GracePeriodSeconds * int(time.Second))}
	}
	return out
}

func headersToFly(in map[string]string) *[]flymachines.FlyMachineHTTPHeader {
	out := make([]flymachines.FlyMachineHTTPHeader, 0, len(in))
	for k, v := range in {
		values := []string{v}
		out = append(out, flymachines.FlyMachineHTTPHeader{Name: internal.Ref(k), Values: &values})
	}
	slices.SortFunc(out, func(a, b flymachines.FlyMachineHTTPHeader) int {
		return strings.Compare(internal.DerefOrZero(a.Name), internal.DerefOrZero(b.Name))
	})
	return &out
}

// ConfigHash returns a stable hash of the group machine configuration.
func ConfigHash(g *Group) (string, error) {
	raw, err := json.Marshal(g.ToFlyMachineConfig())
	if err != nil {
		return "", fmt.Errorf("failed to marshal machine config: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

package convert

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/score-spec/score-go/framework"
	scoretypes "github.com/score-spec/score-go/types"

	"github.com/astromechza/score-flyio/internal/machineconfig"
	"github.com/astromechza/score-flyio/internal/progresify"
	"github.com/astromechza/score-flyio/internal/provisioners"
	"github.com/astromechza/score-flyio/internal/state"
)

const metadataKeyPrefix = "progresify."

// MachinePlan converts the workload using default plan metadata. Secret values
// are returned only by MachinePlanWithSecrets, never as part of the plan.
func MachinePlan(currentState *state.State, workloadName string) (*machineconfig.Plan, error) {
	plan, _, err := MachinePlanWithSecrets(currentState, workloadName, "", "")
	return plan, err
}

// MachinePlanWithSecrets converts a workload and returns runtime secret values
// separately for the deploy step. Callers must not serialize the returned map
// as a plan or include it in logs.
func MachinePlanWithSecrets(currentState *state.State, workloadName string, environment string, rendererVersion string) (*machineconfig.Plan, map[string]string, error) {
	workload, ok := currentState.Workloads[workloadName]
	if !ok {
		return nil, nil, fmt.Errorf("workload '%s': does not exist", workloadName)
	}
	rawMetaValue, hasMeta := workload.Spec.Metadata[progresify.MetadataKey]
	if !hasMeta {
		return nil, nil, nil
	}
	rawMeta, ok := rawMetaValue.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("metadata.%s: must be an object", progresify.MetadataKey)
	}

	resOutputs, err := currentState.GetResourceOutputForWorkload(workloadName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate outputs: %w", err)
	}
	sf := framework.BuildSubstitutionFunction(workload.Spec.Metadata, resOutputs)
	pm, err := progresify.Parse(rawMeta)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse metadata.%s: %w", progresify.MetadataKey, err)
	}
	if pm == nil {
		return nil, nil, fmt.Errorf("workload '%s' requires metadata.%s for machine deployment", workloadName, progresify.MetadataKey)
	}
	if pm.Ingress != nil && pm.Ingress.Type == "cloudflare" && environment != "staging" {
		return nil, nil, fmt.Errorf("metadata.%s.ingress: cloudflare ingress is restricted to staging", progresify.MetadataKey)
	}

	containerNames := slices.Sorted(maps.Keys(workload.Spec.Containers))
	if err := progresify.Validate(pm, containerNames); err != nil {
		return nil, nil, fmt.Errorf("invalid metadata.%s: %w", progresify.MetadataKey, err)
	}

	outputSecrets := make(map[string]string)

	groups := make(map[string]*machineconfig.Group)
	for _, processName := range slices.Sorted(maps.Keys(pm.Processes)) {
		p := pm.Processes[processName]
		groupName := p.MachineGroup
		if groupName == "" {
			groupName = processName
		}
		if _, ok := groups[groupName]; ok {
			continue
		}
		restart := p.Restart
		if restart == "" {
			restart = machineconfig.RestartPolicyAlways
		}
		group := &machineconfig.Group{
			Name:        groupName,
			Region:      progresify.DefaultRegion,
			Guest:       &machineconfig.Guest{CpuKind: "shared", Cpus: 1, MemoryMb: 256},
			MinMachines: 1,
			MaxMachines: 1,
			Restart:     restart,
			Metadata:    groupMetadata(workloadName, environment, rendererVersion, groupName, pm),
		}
		if pm.Region != "" {
			group.Region = pm.Region
		}
		if p.Vm != nil {
			group.Guest = &machineconfig.Guest{CpuKind: "shared", Cpus: p.Vm.Cpus, MemoryMb: p.Vm.MemoryMb}
		}
		if p.Scale != nil {
			group.MinMachines = p.Scale.Min
			group.MaxMachines = p.Scale.Max
		}
		groups[groupName] = group
	}

	for _, containerName := range containerNames {
		container := workload.Spec.Containers[containerName]
		p := pm.Processes[containerName]
		groupName := p.MachineGroup
		if groupName == "" {
			groupName = containerName
		}
		group := groups[groupName]
		if p.HttpService != nil {
			if len(p.HttpService.Ports) > 0 && (pm.Ingress == nil || pm.Ingress.Type != "cloudflare") {
				return nil, nil, fmt.Errorf("process '%s': public http_service requires cloudflare ingress", containerName)
			}
			group.Services = append(group.Services, serviceFromHttpService(p.HttpService, p.Concurrency))
		}
		if len(p.Checks) > 0 {
			if group.Checks == nil {
				group.Checks = make(map[string]machineconfig.Check)
			}
			for checkName, check := range p.Checks {
				group.Checks[containerName+"-"+checkName] = checkFromProgresify(check)
			}
		}

		if container.Image == "." {
			return nil, nil, fmt.Errorf("container '%s': machine deployment requires a prebuilt image (image == '.' is not supported)", containerName)
		}
		restart := p.Restart
		if restart == "" {
			restart = machineconfig.RestartPolicyAlways
		}
		out := machineconfig.Container{Name: containerName, Image: container.Image, Restart: restart}
		if at := strings.LastIndex(container.Image, "@"); at >= 0 && at+1 < len(container.Image) {
			out.ImageDigest = container.Image[at+1:]
		}
		if len(container.Command) > 0 {
			out.Command = append([]string(nil), container.Command...)
		}
		if len(container.Args) > 0 {
			out.Args = append([]string(nil), container.Args...)
		}

		out.Env = make(map[string]string)
		for _, key := range slices.Sorted(maps.Keys(pm.Variables)) {
			resolved, secret, err := substituteVariable(pm.Variables[key], sf)
			if err != nil {
				return nil, nil, fmt.Errorf("metadata.%s.variables: %s: %w", progresify.MetadataKey, key, err)
			}
			if secret {
				outputSecrets[key] = resolved
			} else {
				out.Env[key] = resolved
			}
		}
		for _, key := range slices.Sorted(maps.Keys(container.Variables)) {
			resolved, secret, err := substituteVariable(container.Variables[key], sf)
			if err != nil {
				return nil, nil, fmt.Errorf("container[%s].variables: %s: %w", containerName, key, err)
			}
			if secret {
				delete(out.Env, key)
				slog.Warn("Secret accessed as part of resolving container variables, converting variable into a runtime secret", slog.String("key", key))
				outputSecrets[key] = resolved
			} else {
				out.Env[key] = resolved
			}
		}

		if len(container.Files) > 0 {
			out.Files = make([]machineconfig.File, 0, len(container.Files))
			for i, f := range container.Files {
				if f.Mode != nil {
					return nil, nil, fmt.Errorf("container[%s].files[%d]: mode not supported", containerName, i)
				}
				if f.BinaryContent != nil {
					out.Files = append(out.Files, machineconfig.File{GuestPath: f.Target, RawContent: *f.BinaryContent})
					continue
				}
				if f.Source != nil {
					if !filepath.IsAbs(*f.Source) && workload.File != nil {
						lp := filepath.Join(filepath.Dir(*workload.File), *f.Source)
						f.Source = &lp
					}
				}
				if f.NoExpand == nil || !*f.NoExpand {
					if f.Content != nil {
						resolved, secret, err := substituteVariable(*f.Content, sf)
						if err != nil {
							return nil, nil, fmt.Errorf("container[%s].files[%d]: failed to interpolate in contents: %w", containerName, i, err)
						}
						if secret {
							return nil, nil, fmt.Errorf("container[%s].files[%d]: runtime secret-backed files are not supported by machine plans", containerName, i)
						}
						f.Content = &resolved
					} else if f.Source != nil {
						raw, err := os.ReadFile(*f.Source)
						if err != nil {
							return nil, nil, fmt.Errorf("container[%s].files[%d]: failed to read file: %w", containerName, i, err)
						} else if !utf8.Valid(raw) {
							return nil, nil, fmt.Errorf("container[%s].files[%d]: cannot perform interpolation on non utf-8 file (did you mean to set noExpand?)", containerName, i)
						}
						stringRaw := string(raw)
						resolved, secret, err := substituteVariable(stringRaw, sf)
						if err != nil {
							return nil, nil, fmt.Errorf("container[%s].files[%d]: failed to interpolate in source file: %w", containerName, i, err)
						}
						if secret {
							return nil, nil, fmt.Errorf("container[%s].files[%d]: runtime secret-backed files are not supported by machine plans", containerName, i)
						}
						if stringRaw != resolved {
							f.Source = nil
							f.Content = &resolved
						}
					}
				}
				if f.Content != nil {
					out.Files = append(out.Files, machineconfig.File{GuestPath: f.Target, RawContent: base64.StdEncoding.EncodeToString([]byte(*f.Content))})
				} else if f.Source != nil {
					return nil, nil, fmt.Errorf("container '%s'.files[%d]: local_path files are not supported for machine deployment", containerName, i)
				} else {
					return nil, nil, fmt.Errorf("container[%s].files[%d]: content or source must be set", containerName, i)
				}
			}
		}

		if len(container.Volumes) > 0 {
			out.Mounts = make([]machineconfig.Mount, 0, len(container.Volumes))
			for i, volume := range container.Volumes {
				if volume.Path != nil && *volume.Path != "/" {
					return nil, nil, fmt.Errorf("container[%s].volumes[%d]: sub-path is not supported", containerName, i)
				} else if volume.ReadOnly != nil && *volume.ReadOnly {
					return nil, nil, fmt.Errorf("container[%s].volumes[%d]: read-only=true is not supported", containerName, i)
				}
				source, err := framework.SubstituteString(volume.Source, sf)
				if err != nil {
					return nil, nil, fmt.Errorf("container[%s].volumes[%d]: failed to interpolate source: %w", containerName, i, err)
				}
				if source == "" {
					return nil, nil, fmt.Errorf("container '%s'.volumes[%d]: volume source must not be empty", containerName, i)
				}
				out.Mounts = append(out.Mounts, machineconfig.Mount{Volume: source, Path: volume.Target})
				if !slices.ContainsFunc(group.Volumes, func(v machineconfig.Volume) bool { return v.Name == source }) {
					group.Volumes = append(group.Volumes, machineconfig.Volume{Name: source})
				}
			}
		}

		group.Containers = append(group.Containers, out)

		if container.LivenessProbe != nil {
			if container.LivenessProbe.Exec != nil {
				slog.Warn("Exec probes are not supported, ignoring it in the livenessProbes")
			}
			if container.LivenessProbe.HttpGet != nil && !groupHasCheck(group, containerName+"-liveness_probe") {
				if group.Checks == nil {
					group.Checks = make(map[string]machineconfig.Check)
				}
				group.Checks[containerName+"-liveness_probe"] = checkFromProbe(*container.LivenessProbe.HttpGet)
			}
		}
		if container.ReadinessProbe != nil {
			if container.ReadinessProbe.Exec != nil {
				slog.Warn("Exec probes are not supported, ignoring it in the readinessProbe")
			}
			if container.ReadinessProbe.HttpGet != nil {
				hg := container.ReadinessProbe.HttpGet
				serviceIndex := slices.IndexFunc(group.Services, func(s machineconfig.Service) bool {
					return s.InternalPort == hg.Port
				})
				if serviceIndex >= 0 {
					svc := group.Services[serviceIndex]
					svc.Checks = append(svc.Checks, machineconfig.ServiceHttpCheck{
						Method:          "get",
						Path:            hg.Path,
						Headers:         probeHeaders(hg),
						IntervalSeconds: 10,
						TimeoutSeconds:  5,
					})
					group.Services[serviceIndex] = svc
				} else if !groupHasCheck(group, containerName+"-readiness_probe") {
					if group.Checks == nil {
						group.Checks = make(map[string]machineconfig.Check)
					}
					group.Checks[containerName+"-readiness_probe"] = checkFromProbe(*hg)
				}
			}
		}
	}

	groupNames := slices.Sorted(maps.Keys(groups))
	plan := &machineconfig.Plan{
		AppName:         currentState.Extras.AppPrefix + workloadName,
		RendererVersion: rendererVersion,
		Workload:        workloadName,
		Environment:     environment,
		Groups:          make([]machineconfig.Group, 0, len(groupNames)),
	}
	for _, groupName := range groupNames {
		g := groups[groupName]
		slices.SortFunc(g.Volumes, func(a, b machineconfig.Volume) int { return strings.Compare(a.Name, b.Name) })
		slices.SortFunc(g.Services, func(a, b machineconfig.Service) int { return a.InternalPort - b.InternalPort })
		plan.Groups = append(plan.Groups, *g)
	}

	if err := plan.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid machine plan: %w", err)
	}
	return plan, outputSecrets, nil
}

func substituteVariable(value string, sf func(string) (string, error)) (string, bool, error) {
	sf2, sa := provisioners.BuildSubstitutionFuncWithSecretWatch(sf)
	out, err := framework.SubstituteString(value, sf2)
	if err != nil {
		return "", false, err
	}
	return out, *sa, nil
}

func groupMetadata(workloadName string, environment string, rendererVersion string, groupName string, pm *progresify.Metadata) map[string]string {
	out := map[string]string{
		metadataKeyPrefix + "workload":         workloadName,
		metadataKeyPrefix + "environment":      environment,
		metadataKeyPrefix + "group":            groupName,
		metadataKeyPrefix + "renderer-version": rendererVersion,
		metadataKeyPrefix + "owner":            pm.Owner,
		metadataKeyPrefix + "secret-namespace": pm.SecretNamespace,
	}
	for k, v := range out {
		if v == "" {
			delete(out, k)
		}
	}
	return out
}

func serviceFromHttpService(hs *progresify.HttpService, concurrency map[string]any) machineconfig.Service {
	svc := machineconfig.Service{
		Protocol:           "tcp",
		InternalPort:       hs.InternalPort,
		AutoStop:           hs.AutoStop,
		AutoStart:          hs.AutoStop != "" && hs.AutoStop != "off",
		MinMachinesRunning: hs.MinMachinesRunning,
		Concurrency:        maps.Clone(concurrency),
	}
	if hs.Protocol != "" {
		svc.Protocol = hs.Protocol
	}
	if len(hs.Ports) > 0 {
		svc.Ports = make([]machineconfig.ServicePort, 0, len(hs.Ports))
		for _, p := range hs.Ports {
			svc.Ports = append(svc.Ports, machineconfig.ServicePort{Port: p.Port, Handlers: append([]string(nil), p.Handlers...)})
		}
	}
	return svc
}

func checkFromProgresify(c progresify.Check) machineconfig.Check {
	return machineconfig.Check{
		Type:               c.Type,
		Port:               c.Port,
		Method:             c.Method,
		Path:               c.Path,
		Headers:            maps.Clone(c.Headers),
		IntervalSeconds:    c.IntervalSeconds,
		TimeoutSeconds:     c.TimeoutSeconds,
		GracePeriodSeconds: c.GracePeriodSeconds,
	}
}

func checkFromProbe(probe scoretypes.HttpProbe) machineconfig.Check {
	return machineconfig.Check{
		Type:    "http",
		Port:    probe.Port,
		Method:  "get",
		Path:    probe.Path,
		Headers: probeHeaders(&probe),
	}
}

func probeHeaders(probe *scoretypes.HttpProbe) map[string]string {
	if probe.HttpHeaders == nil {
		return nil
	}
	headers := make(map[string]string, len(probe.HttpHeaders))
	for _, header := range probe.HttpHeaders {
		headers[header.Name] = header.Value
	}
	return headers
}

func groupHasCheck(group *machineconfig.Group, name string) bool {
	_, ok := group.Checks[name]
	return ok
}

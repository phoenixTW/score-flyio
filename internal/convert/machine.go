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

	"github.com/phoenixTW/score-flyio/internal/flymetadata"
	"github.com/phoenixTW/score-flyio/internal/provisioners"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/machineconfig"
	"github.com/phoenixTW/score-flyio/pkg/flydeploy/planner"
	"github.com/phoenixTW/score-flyio/pkg/state"
)

const metadataKeyPrefix = "fly."

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
	rawMetaValue, hasMeta := workload.Spec.Metadata[flymetadata.MetadataKey]
	if !hasMeta {
		return nil, nil, nil
	}
	rawMeta, ok := rawMetaValue.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("metadata.%s: must be an object", flymetadata.MetadataKey)
	}

	resOutputs, err := currentState.GetResourceOutputForWorkload(workloadName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate outputs: %w", err)
	}
	sf := framework.BuildSubstitutionFunction(workload.Spec.Metadata, resOutputs)
	pm, err := flymetadata.Parse(rawMeta)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse metadata.%s: %w", flymetadata.MetadataKey, err)
	}
	if pm == nil {
		return nil, nil, fmt.Errorf("workload '%s' requires metadata.%s for machine deployment", workloadName, flymetadata.MetadataKey)
	}

	containerNames := slices.Sorted(maps.Keys(workload.Spec.Containers))
	if err := flymetadata.Validate(pm, containerNames); err != nil {
		return nil, nil, fmt.Errorf("invalid metadata.%s: %w", flymetadata.MetadataKey, err)
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
			Region:      flymetadata.DefaultRegion,
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
			if len(p.HttpService.Ports) > 0 && (pm.Ingress == nil || pm.Ingress.Type != "public") {
				return nil, nil, fmt.Errorf("process '%s': public http_service requires public ingress", containerName)
			}
			group.Services = append(group.Services, serviceFromHttpService(p.HttpService, p.Concurrency))
		}
		if len(p.Checks) > 0 {
			if group.Checks == nil {
				group.Checks = make(map[string]machineconfig.Check)
			}
			for checkName, check := range p.Checks {
				group.Checks[containerName+"-"+checkName] = checkFromMetadata(check)
			}
		}

		image := container.Image
		if p.Image != "" {
			image, err = framework.SubstituteString(p.Image, sf)
			if err != nil {
				return nil, nil, fmt.Errorf("process[%s].image: failed to interpolate: %w", containerName, err)
			}
		}
		if image == "." {
			return nil, nil, fmt.Errorf("container '%s': machine deployment requires a prebuilt image (image == '.' is not supported)", containerName)
		}
		restart := p.Restart
		if restart == "" {
			restart = machineconfig.RestartPolicyAlways
		}
		out := machineconfig.Container{Name: containerName, Image: image, Restart: restart}
		if at := strings.LastIndex(image, "@"); at >= 0 && at+1 < len(image) {
			out.ImageDigest = image[at+1:]
		}
		command := container.Command
		if len(p.Command) > 0 {
			command = p.Command
		}
		if len(command) > 0 {
			out.Command = append([]string(nil), command...)
		}
		args := container.Args
		if len(p.Args) > 0 {
			args = p.Args
		}
		if len(args) > 0 {
			out.Args = append([]string(nil), args...)
		}

		out.Env = make(map[string]string)
		for _, key := range slices.Sorted(maps.Keys(pm.Variables)) {
			resolved, secret, err := substituteVariable(pm.Variables[key], sf)
			if err != nil {
				return nil, nil, fmt.Errorf("metadata.%s.variables: %s: %w", flymetadata.MetadataKey, key, err)
			}
			if secret {
				outputSecrets[key] = resolved
			} else {
				out.Env[key] = resolved
			}
		}
		for _, key := range slices.Sorted(maps.Keys(p.Variables)) {
			resolved, secret, err := substituteVariable(p.Variables[key], sf)
			if err != nil {
				return nil, nil, fmt.Errorf("metadata.%s.processes.%s.variables: %s: %w", flymetadata.MetadataKey, containerName, key, err)
			}
			if secret {
				delete(out.Env, key)
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
			for target, f := range container.Files {
				if f.Mode != nil {
					return nil, nil, fmt.Errorf("container[%s].files[%s]: mode not supported", containerName, target)
				}
				if f.BinaryContent != nil {
					out.Files = append(out.Files, machineconfig.File{GuestPath: target, RawContent: *f.BinaryContent})
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
							return nil, nil, fmt.Errorf("container[%s].files[%s]: failed to interpolate in contents: %w", containerName, target, err)
						}
						if secret {
							return nil, nil, fmt.Errorf("container[%s].files[%s]: runtime secret-backed files are not supported by machine plans", containerName, target)
						}
						f.Content = &resolved
					} else if f.Source != nil {
						raw, err := os.ReadFile(*f.Source)
						if err != nil {
							return nil, nil, fmt.Errorf("container[%s].files[%s]: failed to read file: %w", containerName, target, err)
						} else if !utf8.Valid(raw) {
							return nil, nil, fmt.Errorf("container[%s].files[%s]: cannot perform interpolation on non utf-8 file (did you mean to set noExpand?)", containerName, target)
						}
						stringRaw := string(raw)
						resolved, secret, err := substituteVariable(stringRaw, sf)
						if err != nil {
							return nil, nil, fmt.Errorf("container[%s].files[%s]: failed to interpolate in source file: %w", containerName, target, err)
						}
						if secret {
							return nil, nil, fmt.Errorf("container[%s].files[%s]: runtime secret-backed files are not supported by machine plans", containerName, target)
						}
						if stringRaw != resolved {
							f.Source = nil
							f.Content = &resolved
						}
					}
				}
				if f.Content != nil {
					out.Files = append(out.Files, machineconfig.File{GuestPath: target, RawContent: base64.StdEncoding.EncodeToString([]byte(*f.Content))})
				} else if f.Source != nil {
					return nil, nil, fmt.Errorf("container '%s'.files[%s]: local_path files are not supported for machine deployment", containerName, target)
				} else {
					return nil, nil, fmt.Errorf("container[%s].files[%s]: content or source must be set", containerName, target)
				}
			}
		}

		if len(container.Volumes) > 0 {
			out.Mounts = make([]machineconfig.Mount, 0, len(container.Volumes))
			for target, volume := range container.Volumes {
				if volume.Path != nil && *volume.Path != "/" {
					return nil, nil, fmt.Errorf("container[%s].volumes[%s]: sub-path is not supported", containerName, target)
				} else if volume.ReadOnly != nil && *volume.ReadOnly {
					return nil, nil, fmt.Errorf("container[%s].volumes[%s]: read-only=true is not supported", containerName, target)
				}
				source, err := framework.SubstituteString(volume.Source, sf)
				if err != nil {
					return nil, nil, fmt.Errorf("container[%s].volumes[%s]: failed to interpolate source: %w", containerName, target, err)
				}
				if source == "" {
					return nil, nil, fmt.Errorf("container '%s'.volumes[%s]: volume source must not be empty", containerName, target)
				}
				out.Mounts = append(out.Mounts, machineconfig.Mount{Volume: source, Path: target})
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
		ReleaseCommand:  pm.ReleaseCommand,
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

func groupMetadata(workloadName string, environment string, rendererVersion string, groupName string, pm *flymetadata.Metadata) map[string]string {
	out := map[string]string{
		metadataKeyPrefix + "workload":         workloadName,
		metadataKeyPrefix + "environment":      environment,
		planner.MetadataGroup:                  groupName,
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

func serviceFromHttpService(hs *flymetadata.HttpService, concurrency map[string]any) machineconfig.Service {
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
	for _, name := range slices.Sorted(maps.Keys(hs.Checks)) {
		check := hs.Checks[name]
		if check.Type != "http" {
			continue
		}
		serviceCheck := machineconfig.ServiceHttpCheck{
			Method:             check.Method,
			Path:               check.Path,
			Headers:            maps.Clone(check.Headers),
			IntervalSeconds:    check.IntervalSeconds,
			TimeoutSeconds:     check.TimeoutSeconds,
			GracePeriodSeconds: check.GracePeriodSeconds,
			Protocol:           hs.Protocol,
		}
		svc.Checks = append(svc.Checks, serviceCheck)
	}
	return svc
}

func checkFromMetadata(c flymetadata.Check) machineconfig.Check {
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

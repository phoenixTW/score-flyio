// Package planner computes a deterministic, read-only diff between a desired
// MachinePlan and the machines currently associated with a Fly app.
package planner

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/astromechza/score-flyio/internal/flymachines"
	"github.com/astromechza/score-flyio/internal/machineconfig"
)

const (
	MetadataGroup = "progresify.group"
	MetadataHash  = "progresify.config-hash"
)

type Action string

const (
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"
	ActionDelete  Action = "delete"
	ActionReplace Action = "replace"
	ActionNoop    Action = "noop"
)

// MachineChange is one mutation (or no-op) in an apply plan.
type MachineChange struct {
	Action              Action   `json:"action"`
	MachineID           string   `json:"machine_id,omitempty"`
	MachineName         string   `json:"machine_name,omitempty"`
	Group               string   `json:"group"`
	Region              string   `json:"region,omitempty"`
	ConfigHash          string   `json:"config_hash,omitempty"`
	ImageDigests        []string `json:"image_digests,omitempty"`
	RequiresReplacement bool     `json:"requires_replacement"`
}

// Diff compares desired groups to live machines using stable group
// metadata and a config hash so repeated applies are no-ops.
func Diff(desired *machineconfig.Plan, live []flymachines.Machine) ([]MachineChange, error) {
	if desired == nil {
		return nil, fmt.Errorf("desired plan must not be nil")
	}
	if err := desired.Validate(); err != nil {
		return nil, fmt.Errorf("invalid desired plan: %w", err)
	}
	if err := desired.ValidateImmutableImages(); err != nil {
		return nil, fmt.Errorf("invalid desired plan: %w", err)
	}

	byGroup := make(map[string][]flymachines.Machine)
	for _, m := range live {
		group := machineMetadata(m, MetadataGroup)
		if group != "" {
			byGroup[group] = append(byGroup[group], m)
		}
	}

	changes := make([]MachineChange, 0)
	seen := make(map[string]bool)
	for _, group := range desired.Groups {
		seen[group.Name] = true
		hash, err := machineconfig.ConfigHash(&group)
		if err != nil {
			return nil, err
		}
		digests := imageDigests(group)
		machines := append([]flymachines.Machine(nil), byGroup[group.Name]...)
		sort.Slice(machines, func(i, j int) bool { return deref(machines[i].Id) < deref(machines[j].Id) })

		managedCount := len(machines)
		if group.MaxMachines == 0 || managedCount > group.MaxMachines {
			managedCount = group.MaxMachines
		}
		for i := 0; i < managedCount; i++ {
			machine := machines[i]
			change := MachineChange{Group: group.Name, Region: group.Region, ConfigHash: hash, ImageDigests: digests, MachineID: deref(machine.Id), MachineName: deref(machine.Name)}
			if machineMetadata(machine, MetadataHash) == hash && machine.Region != nil && deref(machine.Region) == group.Region {
				change.Action = ActionNoop
			} else {
				change.Action = ActionUpdate
				change.RequiresReplacement = requiresReplacement(machine, group)
				if change.RequiresReplacement {
					change.Action = ActionReplace
				}
			}
			changes = append(changes, change)
		}
		for i := len(machines); i < group.MinMachines; i++ {
			changes = append(changes, MachineChange{Action: ActionCreate, Group: group.Name, Region: group.Region, ConfigHash: hash, ImageDigests: digests})
		}
		deleteFrom := group.MaxMachines
		if deleteFrom > len(machines) {
			deleteFrom = len(machines)
		}
		for _, machine := range machines[deleteFrom:] {
			changes = append(changes, MachineChange{Action: ActionDelete, Group: group.Name, MachineID: deref(machine.Id), MachineName: deref(machine.Name)})
		}
	}

	for group, machines := range byGroup {
		if seen[group] {
			continue
		}
		for _, machine := range machines {
			changes = append(changes, MachineChange{Action: ActionDelete, Group: group, MachineID: deref(machine.Id), MachineName: deref(machine.Name)})
		}
	}
	slices.SortFunc(changes, func(a, b MachineChange) int {
		if a.Group != b.Group {
			return strings.Compare(a.Group, b.Group)
		}
		if a.Action != b.Action {
			return strings.Compare(string(a.Action), string(b.Action))
		}
		return strings.Compare(a.MachineID, b.MachineID)
	})
	return changes, nil
}

func requiresReplacement(machine flymachines.Machine, group machineconfig.Group) bool {
	if machine.Region != nil && deref(machine.Region) != group.Region {
		return true
	}
	return false
}

func imageDigests(group machineconfig.Group) []string {
	out := make([]string, 0, len(group.Containers))
	for _, c := range group.Containers {
		if c.ImageDigest != "" {
			out = append(out, c.ImageDigest)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func machineMetadata(machine flymachines.Machine, key string) string {
	if machine.Config == nil || machine.Config.Metadata == nil {
		return ""
	}
	return (*machine.Config.Metadata)[key]
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

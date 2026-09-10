package planner

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/astromechza/score-flyio/internal"
	"github.com/astromechza/score-flyio/internal/flymachines"
	"github.com/astromechza/score-flyio/internal/machineconfig"
)

func testPlan(t *testing.T) *machineconfig.Plan {
	t.Helper()
	group := machineconfig.Group{
		Name: "app", Region: "iad", MinMachines: 1, MaxMachines: 1,
		Guest:      &machineconfig.Guest{Cpus: 1, MemoryMb: 256},
		Containers: []machineconfig.Container{{Name: "api", Image: "example/api@sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("a", 64)}},
		Metadata:   map[string]string{MetadataGroup: "app"},
	}
	return &machineconfig.Plan{AppName: "test", Workload: "api", Groups: []machineconfig.Group{group}}
}

func testMachineNamed(t *testing.T, id, name, group, hash string) flymachines.Machine {
	t.Helper()
	metadata := map[string]string{MetadataGroup: group, MetadataHash: hash}
	return flymachines.Machine{Id: internal.Ref(id), Name: internal.Ref(name), Region: internal.Ref("iad"), Config: &flymachines.FlyMachineConfig{Metadata: &metadata}}
}

func testMachine(t *testing.T, hash string) flymachines.Machine {
	t.Helper()

	return testMachineNamed(t, "m-1", "app-1", "app", hash)
}

func TestDiffCreatesMissingMinimum(t *testing.T) {
	changes, err := Diff(testPlan(t), nil)

	assert.NoError(t, err)
	assert.Len(t, changes, 1)
	assert.Equal(t, ActionCreate, changes[0].Action)
	assert.Equal(t, "sha256:"+strings.Repeat("a", 64), changes[0].ImageDigests[0])
}

func TestDiffIsNoopForAdoptedMatchingMachine(t *testing.T) {
	plan := testPlan(t)
	hash, err := machineconfig.ConfigHash(&plan.Groups[0])

	assert.NoError(t, err)

	changes, err := Diff(plan, []flymachines.Machine{testMachine(t, hash)})

	assert.NoError(t, err)
	assert.Len(t, changes, 1)
	assert.Equal(t, ActionNoop, changes[0].Action)
}

func TestDiffDeletesOnlyManagedOrphans(t *testing.T) {
	plan := testPlan(t)
	managed := testMachine(t, "stale")
	managed.Config.Metadata = &map[string]string{MetadataGroup: "old"}
	unmanaged := flymachines.Machine{Id: internal.Ref("unmanaged"), Config: &flymachines.FlyMachineConfig{}}

	changes, err := Diff(plan, []flymachines.Machine{managed, unmanaged})

	assert.NoError(t, err)
	assert.Len(t, changes, 2)
	assert.Equal(t, ActionCreate, changes[0].Action)
	assert.Equal(t, ActionDelete, changes[1].Action)
	assert.Equal(t, "m-1", changes[1].MachineID)
}

func TestDiffEdgeCases(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name     string
		mutate   func(*machineconfig.Group)
		live     func(*testing.T, string) []flymachines.Machine
		expected func(string) []MachineChange
	}{
		{
			name:   "scale zero group with no live machines produces no changes",
			mutate: func(g *machineconfig.Group) { g.MinMachines, g.MaxMachines = 0, 0 },
			live:   func(*testing.T, string) []flymachines.Machine { return nil },
			expected: func(string) []MachineChange {
				return []MachineChange{}
			},
		},
		{
			name:   "live machines beyond max are deleted",
			mutate: func(*machineconfig.Group) {},
			live: func(t *testing.T, hash string) []flymachines.Machine {
				return []flymachines.Machine{
					testMachineNamed(t, "m-1", "app-1", "app", hash),
					testMachineNamed(t, "m-2", "app-2", "app", hash),
					testMachineNamed(t, "m-3", "app-3", "app", hash),
				}
			},
			expected: func(hash string) []MachineChange {
				return []MachineChange{
					{Action: ActionDelete, MachineID: "m-2", MachineName: "app-2", Group: "app"},
					{Action: ActionDelete, MachineID: "m-3", MachineName: "app-3", Group: "app"},
					{Action: ActionNoop, MachineID: "m-1", MachineName: "app-1", Group: "app", Region: "iad", ConfigHash: hash, ImageDigests: []string{digest}},
				}
			},
		},
		{
			name:   "orphan group machines are deleted while kept group stays noop",
			mutate: func(*machineconfig.Group) {},
			live: func(t *testing.T, hash string) []flymachines.Machine {
				return []flymachines.Machine{
					testMachineNamed(t, "m-1", "app-1", "app", hash),
					testMachineNamed(t, "m-9", "old-1", "old", "stale"),
				}
			},
			expected: func(hash string) []MachineChange {
				return []MachineChange{
					{Action: ActionNoop, MachineID: "m-1", MachineName: "app-1", Group: "app", Region: "iad", ConfigHash: hash, ImageDigests: []string{digest}},
					{Action: ActionDelete, MachineID: "m-9", MachineName: "old-1", Group: "old"},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := testPlan(t)
			tt.mutate(&plan.Groups[0])
			hash, err := machineconfig.ConfigHash(&plan.Groups[0])

			assert.NoError(t, err)

			changes, err := Diff(plan, tt.live(t, hash))

			assert.NoError(t, err)
			assert.Equal(t, tt.expected(hash), changes)
		})
	}
}

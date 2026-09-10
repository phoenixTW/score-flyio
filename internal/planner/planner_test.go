package planner

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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

func testMachine(t *testing.T, hash string) flymachines.Machine {
	t.Helper()
	metadata := map[string]string{MetadataGroup: "app", MetadataHash: hash}
	return flymachines.Machine{Id: internal.Ref("m-1"), Name: internal.Ref("app-1"), Region: internal.Ref("iad"), Config: &flymachines.FlyMachineConfig{Metadata: &metadata}}
}

func TestDiffCreatesMissingMinimum(t *testing.T) {
	changes, err := Diff(testPlan(t), nil)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Equal(t, ActionCreate, changes[0].Action)
	require.Equal(t, "sha256:"+strings.Repeat("a", 64), changes[0].ImageDigests[0])
}

func TestDiffIsNoopForAdoptedMatchingMachine(t *testing.T) {
	plan := testPlan(t)
	hash, err := machineconfig.ConfigHash(&plan.Groups[0])
	require.NoError(t, err)
	changes, err := Diff(plan, []flymachines.Machine{testMachine(t, hash)})
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Equal(t, ActionNoop, changes[0].Action)
}

func TestDiffDeletesOnlyManagedOrphans(t *testing.T) {
	plan := testPlan(t)
	managed := testMachine(t, "stale")
	managed.Config.Metadata = &map[string]string{MetadataGroup: "old"}
	unmanaged := flymachines.Machine{Id: internal.Ref("unmanaged"), Config: &flymachines.FlyMachineConfig{}}
	changes, err := Diff(plan, []flymachines.Machine{managed, unmanaged})
	require.NoError(t, err)
	require.Len(t, changes, 2)
	require.Equal(t, ActionCreate, changes[0].Action)
	require.Equal(t, ActionDelete, changes[1].Action)
	require.Equal(t, "m-1", changes[1].MachineID)
}

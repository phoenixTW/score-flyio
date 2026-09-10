package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/score-spec/score-go/framework"
	scoretypes "github.com/score-spec/score-go/types"
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
)

func writeStateFile(t *testing.T, directory string, content string) {
	t.Helper()
	assert.NoError(t, os.MkdirAll(filepath.Join(directory, DefaultRelativeStateDirectory), 0755))
	assert.NoError(t, os.WriteFile(filepath.Join(directory, DefaultRelativeStateDirectory, FileName), []byte(content), 0644))
}

func TestLoadStateDirectoryMigratesMissingSchemaVersion(t *testing.T) {
	directory := t.TempDir()
	writeStateFile(t, directory, "workloads: {}\nresources: {}\n")

	sd, found, err := LoadStateDirectory(directory)

	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, StateSchemaVersion, sd.State.Extras.SchemaVersion)
}

func TestLoadStateDirectoryRejectsFutureSchemaVersion(t *testing.T) {
	directory := t.TempDir()
	writeStateFile(t, directory, "schema_version: 99\nworkloads: {}\nresources: {}\n")

	sd, found, err := LoadStateDirectory(directory)

	assert.EqualError(t, err, "state schema version 99 is newer than supported 1, upgrade the tool")
	assert.True(t, found)
	assert.Nil(t, sd)
}

func TestPersistWritesCurrentSchemaVersion(t *testing.T) {
	directory := t.TempDir()
	sd := &StateDirectory{Path: filepath.Join(directory, DefaultRelativeStateDirectory), State: State{}}

	assert.NoError(t, sd.Persist())

	raw, readErr := os.ReadFile(filepath.Join(sd.Path, FileName))
	assert.NoError(t, readErr)
	assert.Contains(t, string(raw), "schema_version: 1\n")
	reloaded, found, err := LoadStateDirectory(directory)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, StateSchemaVersion, reloaded.State.Extras.SchemaVersion)
}

func TestPersistConcurrentIsSerializedByLock(t *testing.T) {
	sharedPath := filepath.Join(t.TempDir(), DefaultRelativeStateDirectory)
	first := &StateDirectory{Path: sharedPath, State: State{}}
	assert.NoError(t, first.Persist())

	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)
	for _, payload := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func(payload string) {
			defer wg.Done()
			sd := &StateDirectory{Path: sharedPath, State: State{}}
			for i := 0; i < 25; i++ {
				spec := scoretypes.Workload{}
				spec.Metadata = scoretypes.WorkloadMetadata{"name": payload}
				next, err := sd.State.WithWorkload(&spec, nil, WorkloadExtras{})
				if err != nil {
					errorsCh <- err
					return
				}
				sd.State = *next
				if err := sd.Persist(); err != nil {
					errorsCh <- err
					return
				}
			}
		}(payload)
	}
	wg.Wait()
	close(errorsCh)

	for err := range errorsCh {
		assert.NoError(t, err)
	}
	raw, readErr := os.ReadFile(filepath.Join(sharedPath, FileName))
	assert.NoError(t, readErr)
	var decoded State
	assert.NoError(t, yaml.Unmarshal(raw, &decoded))
	assert.Len(t, decoded.Workloads, 1)
	_, lockErr := os.Stat(filepath.Join(sharedPath, FileName+".lock"))
	assert.NoError(t, lockErr)
}

func TestPersistDoesNotSerializeSecretValues(t *testing.T) {
	directory := t.TempDir()
	spec := scoretypes.Workload{}
	spec.Metadata = scoretypes.WorkloadMetadata{"name": "api"}
	spec.Containers = map[string]scoretypes.Container{
		"api": {
			Image:     "ghcr.io/progresify/api:1.2.3",
			Variables: map[string]string{"API_TOKEN": "${resources.auth.token}"},
		},
	}
	current := &State{}
	var err error
	current, err = current.WithWorkload(&spec, nil, WorkloadExtras{ReleaseCommandHash: "hash-value"})
	assert.NoError(t, err)
	current.Resources = map[framework.ResourceUid]framework.ScoreResourceState[ResourceExtras]{
		framework.NewResourceUid("api", "auth", "fake-auth", nil, nil): {
			Outputs: map[string]interface{}{"host": "db.internal"},
			OutputLookupFunc: func(keys ...string) (interface{}, error) {
				return "tok-123", nil
			},
		},
	}
	sd := &StateDirectory{Path: filepath.Join(directory, DefaultRelativeStateDirectory), State: *current}

	assert.NoError(t, sd.Persist())

	raw, readErr := os.ReadFile(filepath.Join(sd.Path, FileName))
	assert.NoError(t, readErr)
	serialized := string(raw)
	assert.Contains(t, serialized, "db.internal")
	assert.Contains(t, serialized, "${resources.auth.token}")
	assert.NotContains(t, serialized, "tok-123")
	reloaded, found, err := LoadStateDirectory(directory)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, scoretypes.ContainerVariables{"API_TOKEN": "${resources.auth.token}"}, reloaded.State.Workloads["api"].Spec.Containers["api"].Variables)
}

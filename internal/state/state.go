// Copyright 2024 Humanitec
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package state

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/score-spec/score-go/framework"
	"gopkg.in/yaml.v3"
)

const (
	DefaultRelativeStateDirectory = ".score-flyio"
	FileName                      = "state.yaml"
	SharedStateAppPrefixKey       = "score-flyio-app-prefix"
	StateSchemaVersion            = 1
)

type StateExtras struct {
	SchemaVersion int           `yaml:"schema_version,omitempty"`
	AppPrefix     string        `yaml:"app_prefix"`
	Provisioners  []Provisioner `yaml:"provisioners"`
}

type Provisioner struct {
	ProvisionerId string                  `yaml:"id"`
	ResourceType  string                  `yaml:"resource_type"`
	ResourceClass string                  `yaml:"resource_class,omitempty"`
	ResourceId    string                  `yaml:"resource_id,omitempty"`
	Cmd           *CmdProvisioner         `yaml:"cmd,omitempty"`
	Http          *HttpProvisioner        `yaml:"http,omitempty"`
	Static        *map[string]interface{} `yaml:"static,omitempty"`
}

type CmdProvisioner struct {
	Binary string   `json:"binary"`
	Args   []string `json:"args"`
}

type HttpProvisioner struct {
	Url string `json:"url"`
}

type WorkloadExtras struct {
	ReleaseCommandHash string                `yaml:"release_command_hash,omitempty"`
	ScaleOverrides     map[string]ScaleRange `yaml:"scale_overrides,omitempty"`
}

type ScaleRange struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

type ResourceExtras struct{}

type State = framework.State[StateExtras, WorkloadExtras, ResourceExtras]

// The StateDirectory holds the local state of the project, including any configuration, extensions,
// plugins, or resource provisioning state when possible.
type StateDirectory struct {
	// The path to the state directory
	Path string
	// The current state file
	State State
}

// Persist ensures that the directory is created and that the current config file has been written with the latest settings.
func (sd *StateDirectory) Persist() error {
	if sd.Path == "" {
		return fmt.Errorf("path not set")
	}
	if err := os.Mkdir(sd.Path, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("failed to create directory '%s': %w", sd.Path, err)
	}
	sd.State.Extras.SchemaVersion = StateSchemaVersion
	lockFile, err := lockStateFile(filepath.Join(sd.Path, FileName+".lock"))
	if err != nil {
		return err
	}
	defer func() {
		_ = unlockStateFile(lockFile)
	}()
	out := new(bytes.Buffer)
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	if err := enc.Encode(sd.State); err != nil {
		return fmt.Errorf("failed to encode content: %w", err)
	}

	// important that we overwrite this file atomically via an inode move
	if err := os.WriteFile(filepath.Join(sd.Path, FileName+".temp"), out.Bytes(), 0755); err != nil {
		return fmt.Errorf("failed to write state: %w", err)
	} else if err := os.Rename(filepath.Join(sd.Path, FileName+".temp"), filepath.Join(sd.Path, FileName)); err != nil {
		return fmt.Errorf("failed to complete writing state: %w", err)
	}
	return nil
}

// LoadStateDirectory loads the state directory for the given directory (usually PWD).
func LoadStateDirectory(directory string) (*StateDirectory, bool, error) {
	d := filepath.Join(directory, DefaultRelativeStateDirectory)
	content, err := os.ReadFile(filepath.Join(d, FileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("state file couldn't be read: %w", err)
	}

	var out State
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(true)
	if err := dec.Decode(&out); err != nil {
		return nil, true, fmt.Errorf("state file couldn't be decoded: %w", err)
	}
	if out.Extras.SchemaVersion == 0 {
		out.Extras.SchemaVersion = StateSchemaVersion
	} else if out.Extras.SchemaVersion > StateSchemaVersion {
		return nil, true, fmt.Errorf("state schema version %d is newer than supported %d, upgrade the tool", out.Extras.SchemaVersion, StateSchemaVersion)
	}
	return &StateDirectory{d, out}, true, nil
}

func lockStateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to open state lock file '%s': %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to lock state file '%s': %w", path, err)
	}
	return f, nil
}

func unlockStateFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to unlock state file: %w", err)
	}
	return f.Close()
}

func (p *Provisioner) Matches(uid framework.ResourceUid) bool {
	return p.ResourceType == uid.Type() && (p.ResourceClass == "" || p.ResourceClass == uid.Class()) && (p.ResourceId == "" || p.ResourceId == uid.Id())
}

package convert

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

var goldenUpdate = flag.Bool("update", false, "rewrite golden fixture files")

func TestGoldenMachinePlanJson(t *testing.T) {
	plan, secrets, err := MachinePlanWithSecrets(happyState(), "api", "staging", "0.1.0")

	assert.NoError(t, err)
	assert.NotNil(t, plan)
	assert.Contains(t, secrets, "API_TOKEN")

	raw, err := json.MarshalIndent(plan, "", "  ")
	assert.NoError(t, err)
	raw = append(raw, '\n')

	goldenPath := filepath.Join("testdata", "golden", "plan.json")

	if *goldenUpdate {
		assert.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0755))
		assert.NoError(t, os.WriteFile(goldenPath, raw, 0644))
		return
	}

	expected, readErr := os.ReadFile(goldenPath)
	assert.NoError(t, readErr)
	assert.Equal(t, string(expected), string(raw))
}

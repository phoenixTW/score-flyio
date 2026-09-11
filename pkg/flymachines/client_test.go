package flymachines

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewFlyClientUsesFlyApiBaseUrlOverride(t *testing.T) {
	var mu sync.Mutex
	receivedPath := ""
	receivedAuth := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	t.Setenv("FLY_API_TOKEN", "FlyV1 test-token")
	t.Setenv("FLY_API_BASE_URL", server.URL+"/")

	client, err := NewFlyClient()

	assert.NoError(t, err)
	if assert.NotNil(t, client) {
		assert.Equal(t, "test-token", client.ApiToken)
	}

	app, found, err := GetApp(client, "test-app")

	assert.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, app)
	mu.Lock()
	assert.Equal(t, "/apps/test-app", receivedPath)
	assert.Equal(t, "Bearer test-token", receivedAuth)
	mu.Unlock()
}

func TestNewFlyClientDefaultsToPublicApiWithoutOverride(t *testing.T) {
	t.Setenv("FLY_API_TOKEN", "test-token")

	client, err := NewFlyClient()

	assert.NoError(t, err)
	assert.NotNil(t, client)
	assert.Equal(t, "test-token", client.ApiToken)
}

func TestNewFlyClientRejectsInvalidBaseUrlOverride(t *testing.T) {
	t.Setenv("FLY_API_TOKEN", "test-token")
	t.Setenv("FLY_API_BASE_URL", "ftp://machines.example.test/v1")

	client, err := NewFlyClient()

	assert.Nil(t, client)
	assert.ErrorContains(t, err, "invalid FLY_API_BASE_URL")
	assert.ErrorContains(t, err, "scheme must be http or https")
}

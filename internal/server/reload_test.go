package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serviceDir(t *testing.T, root, name, operationID string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	body := fmt.Sprintf(`{"openapi":"3.0.1","info":{"title":%q,"version":"1.0"},
      "paths":{"/api/thing":{"get":{"operationId":%q,
        "responses":{"200":{"description":"OK"}}}}}}`, name, operationID)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "openapi.json"), []byte(body), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "service.yaml"),
		[]byte("endpoint:\n  base_url: http://127.0.0.1:1\n"), 0o600))
}

func discoveredConfig(t *testing.T, root string) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Server:      config.ServerConfig{Name: "auto-mcp", Version: "1.0.0"},
		ServicesDir: root,
	}
	require.NoError(t, cfg.Rediscover())
	return cfg
}

// A service added after startup becomes reachable without a restart. This is
// what "drop in a spec, get an endpoint" means once the process is already up.
func TestReload_PicksUpANewService(t *testing.T) {
	root := t.TempDir()
	serviceDir(t, root, "hotel", "getHotel")

	srv, base := startServer(t, discoveredConfig(t, root))
	require.Equal(t, []string{"getHotel"}, toolNames(t, base+"/mcp/hotel"))

	serviceDir(t, root, "flight", "getFlight")
	require.NoError(t, srv.Reload())

	assert.Equal(t, []string{"getFlight"}, toolNames(t, base+"/mcp/flight"))
	assert.Equal(t, []string{"getHotel"}, toolNames(t, base+"/mcp/hotel"),
		"the service that did not change keeps working")
}

// An open session survives a reload and is told the tool list changed, rather
// than being disconnected or left looking at a stale list. The notification is
// the protocol's own answer to this, so neither of those is necessary.
func TestReload_OpenSessionSeesTheNewToolsAfterANotification(t *testing.T) {
	root := t.TempDir()
	serviceDir(t, root, "hotel", "getHotel")
	srv, base := startServer(t, discoveredConfig(t, root))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	changed := make(chan struct{}, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "reload-test", Version: "1"},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				changed <- struct{}{}
			},
		})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp/hotel"}, nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()

	before, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, before.Tools, 1)

	// The same service now exposes a different operation.
	serviceDir(t, root, "hotel", "getHotelV2")
	require.NoError(t, srv.Reload())

	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed notification reached the open session")
	}

	after, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	names := []string{}
	for _, tool := range after.Tools {
		names = append(names, tool.Name)
	}
	assert.Equal(t, []string{"getHotelV2"}, names, "the session sees the new tools on the same connection")
}

// A reload that cannot be completed leaves the running configuration alone. A
// broken spec must not take down a process that was serving correctly.
func TestReload_BrokenSpecKeepsTheRunningConfiguration(t *testing.T) {
	root := t.TempDir()
	serviceDir(t, root, "hotel", "getHotel")
	srv, base := startServer(t, discoveredConfig(t, root))

	require.NoError(t, os.WriteFile(filepath.Join(root, "hotel", "openapi.json"),
		[]byte("{ this is not a spec"), 0o600))

	err := srv.Reload()
	require.Error(t, err, "the reload has to report the problem")

	assert.Equal(t, []string{"getHotel"}, toolNames(t, base+"/mcp/hotel"),
		"the previously working service still serves")
}

// A service whose directory is gone stops answering.
func TestReload_RemovedServiceStopsBeingRouted(t *testing.T) {
	root := t.TempDir()
	serviceDir(t, root, "hotel", "getHotel")
	serviceDir(t, root, "flight", "getFlight")
	srv, base := startServer(t, discoveredConfig(t, root))
	require.Equal(t, []string{"getFlight"}, toolNames(t, base+"/mcp/flight"))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "flight")))
	require.NoError(t, srv.Reload())

	resp, err := http.Post(base+"/mcp/flight", "application/json", http.NoBody)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	assert.Equal(t, []string{"getHotel"}, toolNames(t, base+"/mcp/hotel"))
}

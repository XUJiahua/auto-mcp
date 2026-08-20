package server

import (
	"context"
	"fmt"
	"net"
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

func specFile(t *testing.T, dir, name, operationID string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := fmt.Sprintf(`{
      "openapi": "3.0.1", "info": {"title": %q, "version": "1.0"},
      "paths": {"/api/thing": {"get": {"operationId": %q,
        "responses": {"200": {"description": "OK"}}}}}}`, operationID, operationID)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// startServer runs the server on a free port and returns it with its base URL.
func startServer(t *testing.T, cfg *config.Config) (*Server, string) {
	t.Helper()
	port := freePort(t)
	cfg.Server.Port = port
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Mode = config.ServerModeHTTP

	srv := NewServer(cfg)
	require.NotNil(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Start(ctx) }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForServer(t, base)
	return srv, base
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

func waitForServer(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server at %s did not start", base)
}

func toolNames(t *testing.T, endpoint string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "routing-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	require.NoError(t, err, "connecting to %s", endpoint)
	defer func() { _ = session.Close() }()

	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)

	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// Each configured service gets its own route and exposes only its own tools.
// This is what "drop in a spec, get an MCP endpoint" reduces to.
func TestRouting_EachServiceHasItsOwnEndpoint(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{Name: "auto-mcp", Version: "1.0.0"},
		Services: []config.ServiceConfig{
			{Name: "hotel", SwaggerFile: specFile(t, dir, "hotel.json", "getHotel"),
				Endpoint: config.EndpointConfig{BaseURL: "http://127.0.0.1:1"}},
			{Name: "flight", SwaggerFile: specFile(t, dir, "flight.json", "getFlight"),
				Endpoint: config.EndpointConfig{BaseURL: "http://127.0.0.1:2"}},
		},
	}
	_, base := startServer(t, cfg)

	assert.Equal(t, []string{"getHotel"}, toolNames(t, base+"/mcp/hotel"))
	assert.Equal(t, []string{"getFlight"}, toolNames(t, base+"/mcp/flight"))
}

// An unknown service is a 404, not an empty tool list: a typo in the route must
// not look like a service with nothing in it.
func TestRouting_UnknownServiceIs404(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{Name: "auto-mcp", Version: "1.0.0"},
		Services: []config.ServiceConfig{
			{Name: "hotel", SwaggerFile: specFile(t, dir, "hotel.json", "getHotel"),
				Endpoint: config.EndpointConfig{BaseURL: "http://127.0.0.1:1"}},
		},
	}
	_, base := startServer(t, cfg)

	resp, err := http.Post(base+"/mcp/nope", "application/json", http.NoBody)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// The single-service form stays at /mcp, so a one-API deployment does not have
// to invent a name and its callers' URLs do not change.
func TestRouting_SingleServiceStaysAtTheRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Server:      config.ServerConfig{Name: "auto-mcp", Version: "1.0.0"},
		SwaggerFile: specFile(t, dir, "only.json", "getOnly"),
	}
	_, base := startServer(t, cfg)

	assert.Equal(t, []string{"getOnly"}, toolNames(t, base+"/mcp"))
}

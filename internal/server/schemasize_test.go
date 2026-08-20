package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wideSpec produces one operation whose request body has n properties, as a way
// to grow a tool's schema predictably.
func wideSpec(t *testing.T, dir string, properties int) string {
	t.Helper()
	props := map[string]any{}
	for i := 0; i < properties; i++ {
		props[fmt.Sprintf("field%03d", i)] = map[string]any{
			"type":        "string",
			"description": strings.Repeat("x", 60),
		}
	}
	spec := map[string]any{
		"openapi": "3.0.1",
		"info":    map[string]any{"title": "wide", "version": "1.0"},
		"paths": map[string]any{"/api/wide": map[string]any{"post": map[string]any{
			"operationId": "postWide",
			"requestBody": map[string]any{"required": true, "content": map[string]any{
				"application/json": map[string]any{"schema": map[string]any{
					"type": "object", "properties": props}}}},
			"responses": map[string]any{"200": map[string]any{"description": "OK"}}}}},
	}
	body, err := json.Marshal(spec)
	require.NoError(t, err)
	path := filepath.Join(dir, "wide.json")
	require.NoError(t, os.WriteFile(path, body, 0o600))
	return path
}

func wideConfig(t *testing.T, properties int, limit int) *config.Config {
	t.Helper()
	return &config.Config{
		Server:           config.ServerConfig{Name: "auto-mcp", Version: "1.0.0"},
		SwaggerFile:      wideSpec(t, t.TempDir(), properties),
		MaxToolSchemaKiB: limit,
	}
}

// The size of what goes over the wire is reported rather than guessed. tools/list
// reaches a model's context, so its cost is a fact worth having at startup
// instead of discovering later as latency and tokens.
func TestSchemaSize_IsMeasuredPerService(t *testing.T) {
	srv := NewServer(wideConfig(t, 40, 0))

	sizes := srv.SchemaSizes()
	require.Len(t, sizes, 1)

	only := sizes[0]
	assert.Equal(t, 1, only.Tools)
	assert.Positive(t, only.TotalBytes)
	assert.Equal(t, "postWide", only.LargestTool)
	assert.Equal(t, only.TotalBytes, only.LargestBytes, "with one tool the largest is the total")
}

// A limit is opt-in. Nothing about a large schema is wrong on its own, so the
// default cannot be a number this project invented.
func TestSchemaSize_NoLimitByDefault(t *testing.T) {
	cfg := wideConfig(t, 400, 0)

	srv := NewServer(cfg)

	assert.Positive(t, srv.SchemaSizes()[0].TotalBytes)
}

// A configured limit is checked when the tools are built, so a pathological spec
// is refused at startup rather than at the first call.
func TestSchemaSize_ExceedingAConfiguredLimitIsRefused(t *testing.T) {
	cfg := wideConfig(t, 400, 1) // 1 KiB is far below 400 described fields

	_, err := buildForTest(t, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "postWide")
	assert.Contains(t, err.Error(), "max_tool_schema_kib")
}

// A schema inside the limit passes.
func TestSchemaSize_WithinTheLimitIsAccepted(t *testing.T) {
	cfg := wideConfig(t, 5, 64)

	_, err := buildForTest(t, cfg)

	require.NoError(t, err)
}

func buildForTest(t *testing.T, cfg *config.Config) (*Server, error) {
	t.Helper()
	return New(cfg)
}

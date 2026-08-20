package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inTempDir runs the test from an empty working directory so that the
// repository's own config.yaml is not picked up, and gives each case a fresh
// flag set because InitFlags registers onto the global one.
func inTempDir(t *testing.T, args ...string) {
	t.Helper()
	dir := t.TempDir()
	previous, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(previous) })

	pflag.CommandLine = pflag.NewFlagSet("test", pflag.ContinueOnError)
	InitFlags()
	require.NoError(t, pflag.CommandLine.Parse(args))
}

// The documented configuration paths are CLI flags and AUTO_MCP_* environment
// variables. Neither could be reached without a config.yaml on disk, because a
// missing file was returned as a fatal error, which is exactly the situation in
// a container.
func TestLoad_StartsWithoutAConfigFile(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml", "--mode=http")

	cfg, err := Load()
	require.NoError(t, err, "a missing config.yaml must not be fatal")
	require.NotNil(t, cfg)

	assert.Equal(t, "spec.yaml", cfg.SwaggerFile)
	assert.Equal(t, ServerModeHTTP, cfg.Server.Mode)
}

// Defaults have to be usable on their own: tolerating the missing file is no
// help if the process then binds port 0 with an empty server name.
func TestLoad_DefaultsAreUsableOnTheirOwn(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")

	cfg, err := Load()
	require.NoError(t, err)

	assert.NotZero(t, cfg.Server.Port, "a port must be usable without a config file")
	assert.NotEmpty(t, cfg.Server.Host)
	assert.NotEmpty(t, cfg.Server.Name)
	assert.NotEmpty(t, cfg.Server.Version)
	assert.NotEmpty(t, cfg.Server.Timeout)
	assert.NotEmpty(t, cfg.Logging.Level)
	assert.Equal(t, ServerModeSTDIO, cfg.Server.Mode, "stdio stays the default transport")
}

// The default bind host stays on loopback. The MCP endpoint carries no
// authentication of its own (see docs/KNOWN_ISSUES.md), so a process started
// with no configuration at all must not be reachable from off the machine.
func TestLoad_DefaultHostIsLoopback(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Contains(t, []string{"localhost", "127.0.0.1"}, cfg.Server.Host)
}

func TestLoad_EnvironmentSuppliesTheSpecPath(t *testing.T) {
	inTempDir(t)
	t.Setenv("AUTO_MCP_SWAGGER_FILE", "/from/env.yaml")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "/from/env.yaml", cfg.SwaggerFile)
}

// Tolerating an absent file must not extend to tolerating a broken one: a
// hand-edited config that fails to parse has to stop startup rather than be
// silently replaced by defaults.
func TestLoad_MalformedConfigFileIsStillFatal(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	require.NoError(t, os.WriteFile(filepath.Join(".", "config.yaml"),
		[]byte("server:\n  port: [not, a, port\n"), 0o600))

	_, err := Load()
	require.Error(t, err, "a config.yaml that cannot be parsed must be fatal")
}

// A config file that is present is still read, and still loses to an explicit
// flag.
func TestLoad_ConfigFileIsReadAndFlagsWin(t *testing.T) {
	inTempDir(t, "--swagger-file=from-flag.yaml")
	require.NoError(t, os.WriteFile(filepath.Join(".", "config.yaml"), []byte(
		"server:\n  port: 9999\n  name: \"From File\"\nswagger_file: from-file.yaml\n"), 0o600))

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, 9999, cfg.Server.Port, "values from the file are applied")
	assert.Equal(t, "From File", cfg.Server.Name)
	assert.Equal(t, "from-flag.yaml", cfg.SwaggerFile, "an explicit flag wins over the file")
}

func TestLoad_MissingSpecIsStillAnError(t *testing.T) {
	inTempDir(t)

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "swagger file is required")
}

// The --mode flag carries a non-empty default, and it was applied
// unconditionally, so it overrode every other source. server.mode in a config
// file and the documented AUTO_MCP_SERVER_MODE both had no effect: the process
// always came up in stdio unless --mode was passed explicitly.
func TestLoad_ModeComesFromTheConfigFileWhenNoFlagIsPassed(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	require.NoError(t, os.WriteFile("config.yaml", []byte("server:\n  mode: sse\n"), 0o600))

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ServerModeSSE, cfg.Server.Mode)
}

func TestLoad_ModeComesFromTheEnvironment(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	t.Setenv("AUTO_MCP_SERVER_MODE", "http")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ServerModeHTTP, cfg.Server.Mode)
}

// An explicit flag still wins over both.
func TestLoad_ExplicitModeFlagWinsOverFileAndEnv(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml", "--mode=stdio")
	require.NoError(t, os.WriteFile("config.yaml", []byte("server:\n  mode: sse\n"), 0o600))
	t.Setenv("AUTO_MCP_SERVER_MODE", "http")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ServerModeSTDIO, cfg.Server.Mode)
}

// docs/CONFIGURATION.md lists the upstream base URL as settable by environment
// variable. Without it the process starts but every tool call builds a relative
// URL and fails at request time.
func TestLoad_UpstreamBaseURLComesFromTheEnvironment(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	t.Setenv("AUTO_MCP_ENDPOINT_BASE_URL", "https://upstream.example.com")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "https://upstream.example.com", cfg.EndpointConfig.BaseURL)
}

// The adjustment file path is spelled the same way everywhere: the flag, the
// config key and the environment variable all use the singular.
func TestLoad_AdjustmentFileFlag(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml", "--adjustment-file=adj.yaml")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "adj.yaml", cfg.AdjustmentFile)
}

// A misspelled mode used to be silently ignored, leaving the process in
// whatever mode happened to be configured. It now fails at startup, because a
// server listening on a transport the operator did not ask for is worse than
// not starting.
func TestLoad_UnsupportedModeIsRejected(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	t.Setenv("AUTO_MCP_SERVER_MODE", "websocket")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported server mode")
}

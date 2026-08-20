package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeServiceDir(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	for file, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600))
	}
}

const minimalSpec = "openapi: 3.0.1\ninfo: {title: T, version: \"1.0\"}\npaths: {}\n"

// Dropping a directory with a spec in it is enough to get a service, which is
// the point of a services directory: adding an API should not mean editing
// configuration.
func TestDiscover_DirectoryBecomesAService(t *testing.T) {
	inTempDir(t)
	writeServiceDir(t, "services", "hotel", map[string]string{"openapi.yaml": minimalSpec})
	writeServiceDir(t, "services", "flight", map[string]string{"openapi.json": `{"openapi":"3.0.1"}`})
	writeConfig(t, "server:\n  mode: http\nservices_dir: services\n")

	cfg, err := Load()
	require.NoError(t, err)

	services := cfg.ResolvedServices()
	require.Len(t, services, 2)
	assert.Equal(t, "flight", services[0].Name, "discovery order is stable")
	assert.Equal(t, filepath.Join("services", "flight", "openapi.json"), services[0].SwaggerFile)
	assert.Equal(t, "hotel", services[1].Name)
	assert.Equal(t, filepath.Join("services", "hotel", "openapi.yaml"), services[1].SwaggerFile)
}

// A per-service file carries what the spec cannot: where to send the requests
// and which credential to use.
func TestDiscover_ServiceFileSuppliesEndpointAndCredential(t *testing.T) {
	inTempDir(t)
	t.Setenv("HOTEL_KEY", "HK")
	writeServiceDir(t, "services", "hotel", map[string]string{
		"openapi.yaml": minimalSpec,
		"service.yaml": "endpoint:\n  base_url: https://hotel.example.com\nupstream_security:\n  id: hotel_key\n",
	})
	writeConfig(t, `
server:
  mode: http
services_dir: services
security_schemes:
  - {id: hotel_key, type: apiKey, in: header, name: X-API-Key, default_credential: "${HOTEL_KEY}"}
`)

	cfg, err := Load()
	require.NoError(t, err)

	services := cfg.ResolvedServices()
	require.Len(t, services, 1)
	assert.Equal(t, "https://hotel.example.com", services[0].Endpoint.BaseURL)
	require.NotNil(t, services[0].UpstreamSecurity)
	assert.Equal(t, "hotel_key", services[0].UpstreamSecurity.ID)
}

// An adjustment file next to the spec is picked up without being named.
func TestDiscover_AdjustmentFileIsFoundBesideTheSpec(t *testing.T) {
	inTempDir(t)
	writeServiceDir(t, "services", "hotel", map[string]string{
		"openapi.yaml":    minimalSpec,
		"adjustment.yaml": "routes: []\n",
	})
	writeConfig(t, "server:\n  mode: http\nservices_dir: services\n")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, filepath.Join("services", "hotel", "adjustment.yaml"),
		cfg.ResolvedServices()[0].AdjustmentFile)
}

// Discovered services and explicitly listed ones combine.
func TestDiscover_CombinesWithAnExplicitList(t *testing.T) {
	inTempDir(t)
	writeServiceDir(t, "services", "hotel", map[string]string{"openapi.yaml": minimalSpec})
	writeConfig(t, "server:\n  mode: http\nservices_dir: services\n"+
		"services:\n  - {name: flight, swagger_file: flight.yaml}\n")

	cfg, err := Load()
	require.NoError(t, err)

	names := []string{}
	for _, service := range cfg.ResolvedServices() {
		names = append(names, service.Name)
	}
	assert.ElementsMatch(t, []string{"flight", "hotel"}, names)
}

func TestDiscover_Validation(t *testing.T) {
	t.Run("a directory name that is not a path segment", func(t *testing.T) {
		inTempDir(t)
		writeServiceDir(t, "services", "not a name", map[string]string{"openapi.yaml": minimalSpec})
		writeConfig(t, "server:\n  mode: http\nservices_dir: services\n")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "service name")
	})

	t.Run("a directory with no spec", func(t *testing.T) {
		inTempDir(t)
		writeServiceDir(t, "services", "hotel", map[string]string{"readme.md": "hi"})
		writeConfig(t, "server:\n  mode: http\nservices_dir: services\n")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no OpenAPI document")
	})

	t.Run("a name discovered twice", func(t *testing.T) {
		inTempDir(t)
		writeServiceDir(t, "services", "hotel", map[string]string{"openapi.yaml": minimalSpec})
		writeConfig(t, "server:\n  mode: http\nservices_dir: services\n"+
			"services:\n  - {name: hotel, swagger_file: other.yaml}\n")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate service name")
	})

	t.Run("a missing directory", func(t *testing.T) {
		inTempDir(t, "--swagger-file=spec.yaml")
		writeConfig(t, "server:\n  mode: http\nservices_dir: nope\n")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "services_dir")
	})
}

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One spec at the top level is the single-service form. It keeps working and
// stays unnamed, so its route does not move.
func TestServices_TopLevelSpecIsASingleUnnamedService(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	writeConfig(t, "endpoint:\n  base_url: http://one\n")

	cfg, err := Load()
	require.NoError(t, err)

	services := cfg.ResolvedServices()
	require.Len(t, services, 1)
	assert.Empty(t, services[0].Name, "a single service has no route segment")
	assert.Equal(t, "spec.yaml", services[0].SwaggerFile)
	assert.Equal(t, "http://one", services[0].Endpoint.BaseURL)
}

// Several specs each become their own service, and each gets its own upstream.
func TestServices_EachServiceKeepsItsOwnUpstream(t *testing.T) {
	inTempDir(t)
	t.Setenv("HOTEL_KEY", "HK")
	writeConfig(t, `
server:
  mode: http
security_schemes:
  - id: hotel_key
    type: apiKey
    in: header
    name: X-API-Key
    default_credential: "${HOTEL_KEY}"
services:
  - name: hotel
    swagger_file: hotel.yaml
    endpoint:
      base_url: https://hotel.example.com
    upstream_security:
      id: hotel_key
  - name: flight
    swagger_file: flight.yaml
    adjustment_file: flight-adj.yaml
    endpoint:
      base_url: https://flight.example.com
`)

	cfg, err := Load()
	require.NoError(t, err)

	services := cfg.ResolvedServices()
	require.Len(t, services, 2)

	assert.Equal(t, "hotel", services[0].Name)
	assert.Equal(t, "https://hotel.example.com", services[0].Endpoint.BaseURL)
	require.NotNil(t, services[0].UpstreamSecurity)
	assert.Equal(t, "hotel_key", services[0].UpstreamSecurity.ID)

	assert.Equal(t, "flight", services[1].Name)
	assert.Equal(t, "flight-adj.yaml", services[1].AdjustmentFile)
	assert.Nil(t, services[1].UpstreamSecurity, "an upstream needing no credential says nothing")
}

func TestServices_Validation(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		args    []string
		wantErr string
	}{
		{
			name:    "no spec anywhere",
			config:  "services: []\n",
			wantErr: "swagger file is required",
		},
		{
			name:    "a service without a spec",
			config:  "services:\n  - name: hotel\n",
			wantErr: "no swagger_file",
		},
		{
			name:    "two services sharing a name",
			config:  "services:\n  - {name: a, swagger_file: x.yaml}\n  - {name: a, swagger_file: y.yaml}\n",
			wantErr: "duplicate service name",
		},
		{
			name:    "a name that is not one path segment",
			config:  "services:\n  - {name: \"a/b\", swagger_file: x.yaml}\n",
			wantErr: "service name",
		},
		{
			name:    "several services but one is unnamed",
			config:  "services:\n  - {swagger_file: x.yaml}\n  - {name: b, swagger_file: y.yaml}\n",
			wantErr: "must be named",
		},
		{
			name:    "a service pointing at an unknown scheme",
			config:  "services:\n  - {name: a, swagger_file: x.yaml, upstream_security: {id: nope, credential: c}}\n",
			wantErr: "unknown security scheme",
		},
		{
			name:    "stdio cannot serve more than one",
			config:  "server:\n  mode: stdio\nservices:\n  - {name: a, swagger_file: x.yaml}\n  - {name: b, swagger_file: y.yaml}\n",
			wantErr: "stdio",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			inTempDir(t, tt.args...)
			writeConfig(t, tt.config)

			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// Route paths are derived from the name, and stated by the config rather than
// assembled by every caller.
func TestServices_RoutePath(t *testing.T) {
	assert.Equal(t, "/mcp/hotel", ServiceConfig{Name: "hotel"}.RoutePath())
	assert.Equal(t, "/mcp", ServiceConfig{}.RoutePath())
}

// A per-service credential is interpolated like any other.
func TestServices_CredentialsInterpolate(t *testing.T) {
	inTempDir(t)
	t.Setenv("SVC_TOKEN", "T-1")
	writeConfig(t, `
server:
  mode: http
security_schemes:
  - {id: s, type: http, scheme: bearer}
services:
  - name: a
    swagger_file: x.yaml
    upstream_security:
      id: s
      credential: "${SVC_TOKEN}"
`)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "T-1", cfg.ResolvedServices()[0].UpstreamSecurity.Credential)
}

package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile("config.yaml", []byte(body), 0o600))
}

const merchantKeyScheme = `
security_schemes:
  - id: merchant_key
    type: apiKey
    in: header
    name: X-API-Key
    default_credential: "${MERCHANT_KEY}"
  - id: caller_bearer
    type: http
    scheme: bearer
`

// Credentials are interpolated from the environment so that a deployment does
// not have to put them in a file that ends up in version control.
func TestSecurity_CredentialsInterpolateFromTheEnvironment(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	t.Setenv("MERCHANT_KEY", "AK-live")
	writeConfig(t, merchantKeyScheme+`
upstream_security:
  id: merchant_key
`)

	cfg, err := Load()
	require.NoError(t, err)

	scheme, ok := cfg.SecurityScheme("merchant_key")
	require.True(t, ok)
	assert.Equal(t, "AK-live", scheme.DefaultCredential)
}

// An unset variable is an error rather than an empty credential: sending no
// credential looks like a permission problem at the upstream, which is a long
// way from the actual cause.
func TestSecurity_MissingEnvironmentVariableIsFatal(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	require.NoError(t, os.Unsetenv("MERCHANT_KEY"))
	writeConfig(t, merchantKeyScheme+`
upstream_security:
  id: merchant_key
`)

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MERCHANT_KEY")
}

// Binding beyond loopback exposes the MCP endpoint, and the endpoint carries
// whatever credentials the upstream requires. Doing that without any way to
// authenticate the caller must not be possible by accident.
func TestSecurity_PublicBindRequiresDownstreamAuth(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	writeConfig(t, "server:\n  mode: http\n  host: \"0.0.0.0\"\n")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "downstream_security")
}

func TestSecurity_PublicBindWithDownstreamAuthIsAllowed(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	t.Setenv("CALLER_TOKEN", "shared-secret")
	t.Setenv("MERCHANT_KEY", "AK-live")
	writeConfig(t, "server:\n  mode: http\n  host: \"0.0.0.0\"\n"+merchantKeyScheme+`
downstream_security:
  id: caller_bearer
  credential: "${CALLER_TOKEN}"
`)

	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.DownstreamSecurity)
	assert.Equal(t, "shared-secret", cfg.DownstreamSecurity.Credential)
}

// OAuth is also a way to authenticate the caller, so it satisfies the same rule.
func TestSecurity_PublicBindWithOAuthIsAllowed(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	writeConfig(t, "server:\n  mode: http\n  host: \"0.0.0.0\"\noauth:\n  enabled: true\n  provider: github\n  client_id: id\n  client_secret: secret\n")

	_, err := Load()
	require.NoError(t, err)
}

// Loopback is not exposed, so it stays usable with no configuration at all.
func TestSecurity_LoopbackNeedsNoDownstreamAuth(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	writeConfig(t, "server:\n  mode: http\n  host: \"127.0.0.1\"\n")

	_, err := Load()
	require.NoError(t, err)
}

// stdio has no listening socket, so the rule does not apply.
func TestSecurity_StdioIsExemptFromTheBindRule(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	writeConfig(t, "server:\n  mode: stdio\n  host: \"0.0.0.0\"\n")

	_, err := Load()
	require.NoError(t, err)
}

func TestSecurity_SchemeValidation(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "unknown type",
			config:  "security_schemes:\n  - id: s\n    type: magic\n",
			wantErr: "unsupported security scheme type",
		},
		{
			name:    "apiKey without a name",
			config:  "security_schemes:\n  - id: s\n    type: apiKey\n    in: header\n",
			wantErr: "requires a name",
		},
		{
			name:    "apiKey with an unusable location",
			config:  "security_schemes:\n  - id: s\n    type: apiKey\n    in: body\n    name: k\n",
			wantErr: "in must be header or query",
		},
		{
			name:    "http with an unsupported scheme",
			config:  "security_schemes:\n  - id: s\n    type: http\n    scheme: digest\n",
			wantErr: "scheme must be basic or bearer",
		},
		{
			name:    "duplicate ids",
			config:  "security_schemes:\n  - id: s\n    type: http\n    scheme: bearer\n  - id: s\n    type: http\n    scheme: basic\n",
			wantErr: "duplicate security scheme id",
		},
		{
			name:    "requirement referencing an unknown scheme",
			config:  "upstream_security:\n  id: nope\n  credential: x\n",
			wantErr: "unknown security scheme",
		},
		{
			name: "requirement with no credential to use",
			config: "security_schemes:\n  - id: s\n    type: http\n    scheme: bearer\n" +
				"upstream_security:\n  id: s\n",
			wantErr: "no credential",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			inTempDir(t, "--swagger-file=spec.yaml")
			writeConfig(t, tt.config)

			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// Passthrough takes the caller's credential, so it needs none of its own.
func TestSecurity_PassthroughNeedsNoCredential(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	writeConfig(t, "security_schemes:\n  - id: s\n    type: http\n    scheme: bearer\n"+
		"upstream_security:\n  id: s\n  passthrough: true\n")

	_, err := Load()
	require.NoError(t, err)
}

// An upstream that needs no credential is configured by saying nothing, rather
// than by naming a "none" auth type.
func TestSecurity_UpstreamWithoutSecurityIsAllowed(t *testing.T) {
	inTempDir(t, "--swagger-file=spec.yaml")
	writeConfig(t, "endpoint:\n  base_url: http://x\n")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Nil(t, cfg.UpstreamSecurity)
	assert.Equal(t, "http://x", cfg.EndpointConfig.BaseURL)
}

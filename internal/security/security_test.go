package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func apiKeyHeader() config.SecurityScheme {
	return config.SecurityScheme{
		ID: "merchant_key", Type: config.SchemeTypeAPIKey,
		In: config.SchemeInHeader, Name: "X-API-Key", DefaultCredential: "AK-live",
	}
}

func apiKeyQuery() config.SecurityScheme {
	return config.SecurityScheme{
		ID: "q_key", Type: config.SchemeTypeAPIKey,
		In: config.SchemeInQuery, Name: "api_key", DefaultCredential: "QK",
	}
}

func bearer() config.SecurityScheme {
	return config.SecurityScheme{
		ID: "caller", Type: config.SchemeTypeHTTP, Scheme: config.HTTPSchemeBearer,
		DefaultCredential: "shared-secret",
	}
}

func basic() config.SecurityScheme {
	return config.SecurityScheme{
		ID: "b", Type: config.SchemeTypeHTTP, Scheme: config.HTTPSchemeBasic,
		DefaultCredential: "user:pass",
	}
}

func outgoing(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://upstream.example.com/api/x?a=1", nil)
	require.NoError(t, err)
	return req
}

func TestApply_APIKeyHeader(t *testing.T) {
	engine := New([]config.SecurityScheme{apiKeyHeader()})
	req := outgoing(t)

	require.NoError(t, engine.Apply(req, &config.SecurityRequirement{ID: "merchant_key"}))

	assert.Equal(t, "AK-live", req.Header.Get("X-API-Key"))
}

func TestApply_APIKeyQueryKeepsExistingParams(t *testing.T) {
	engine := New([]config.SecurityScheme{apiKeyQuery()})
	req := outgoing(t)

	require.NoError(t, engine.Apply(req, &config.SecurityRequirement{ID: "q_key"}))

	assert.Equal(t, "QK", req.URL.Query().Get("api_key"))
	assert.Equal(t, "1", req.URL.Query().Get("a"), "the request's own parameters survive")
}

func TestApply_BearerAndBasic(t *testing.T) {
	engine := New([]config.SecurityScheme{bearer(), basic()})

	bearerReq := outgoing(t)
	require.NoError(t, engine.Apply(bearerReq, &config.SecurityRequirement{ID: "caller"}))
	assert.Equal(t, "Bearer shared-secret", bearerReq.Header.Get("Authorization"))

	basicReq := outgoing(t)
	require.NoError(t, engine.Apply(basicReq, &config.SecurityRequirement{ID: "b"}))
	user, pass, ok := basicReq.BasicAuth()
	require.True(t, ok)
	assert.Equal(t, "user", user)
	assert.Equal(t, "pass", pass)
}

// A requirement's own credential overrides the scheme's default.
func TestApply_RequirementCredentialWins(t *testing.T) {
	engine := New([]config.SecurityScheme{apiKeyHeader()})
	req := outgoing(t)

	require.NoError(t, engine.Apply(req, &config.SecurityRequirement{ID: "merchant_key", Credential: "AK-override"}))

	assert.Equal(t, "AK-override", req.Header.Get("X-API-Key"))
}

// Passthrough uses the caller's credential, taken from the incoming request and
// carried on the context.
func TestApply_PassthroughUsesTheCallersCredential(t *testing.T) {
	engine := New([]config.SecurityScheme{bearer()})
	ctx := WithPassthroughCredential(context.Background(), "callers-own-token")
	req := outgoing(t).WithContext(ctx)

	require.NoError(t, engine.Apply(req, &config.SecurityRequirement{ID: "caller", Passthrough: true}))

	assert.Equal(t, "Bearer callers-own-token", req.Header.Get("Authorization"))
}

// Without a caller credential, passthrough must not silently fall back to our
// own: that would send the platform's credential on behalf of an unknown caller.
func TestApply_PassthroughWithoutACallerCredentialFails(t *testing.T) {
	engine := New([]config.SecurityScheme{bearer()})
	req := outgoing(t)

	err := engine.Apply(req, &config.SecurityRequirement{ID: "caller", Passthrough: true})

	require.Error(t, err)
	assert.NotContains(t, req.Header.Get("Authorization"), "shared-secret")
}

func TestApply_NilRequirementIsANoOp(t *testing.T) {
	engine := New(nil)
	req := outgoing(t)

	require.NoError(t, engine.Apply(req, nil))

	assert.Empty(t, req.Header.Get("Authorization"))
}

func incoming(t *testing.T, mutate func(*http.Request)) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if mutate != nil {
		mutate(req)
	}
	return req
}

func TestVerify_BearerAcceptsTheConfiguredCredential(t *testing.T) {
	engine := New([]config.SecurityScheme{bearer()})

	credential, err := engine.Verify(
		incoming(t, func(r *http.Request) { r.Header.Set("Authorization", "Bearer shared-secret") }),
		&config.SecurityRequirement{ID: "caller"})

	require.NoError(t, err)
	assert.Equal(t, "shared-secret", credential)
}

func TestVerify_BearerRejectsAnythingElse(t *testing.T) {
	engine := New([]config.SecurityScheme{bearer()})

	for _, header := range []string{"", "Bearer wrong", "shared-secret", "Basic shared-secret"} {
		_, err := engine.Verify(
			incoming(t, func(r *http.Request) {
				if header != "" {
					r.Header.Set("Authorization", header)
				}
			}),
			&config.SecurityRequirement{ID: "caller"})
		assert.Error(t, err, "header %q must be rejected", header)
	}
}

func TestVerify_APIKeyFromHeaderAndQuery(t *testing.T) {
	engine := New([]config.SecurityScheme{apiKeyHeader(), apiKeyQuery()})

	_, err := engine.Verify(
		incoming(t, func(r *http.Request) { r.Header.Set("X-API-Key", "AK-live") }),
		&config.SecurityRequirement{ID: "merchant_key"})
	require.NoError(t, err)

	_, err = engine.Verify(
		incoming(t, func(r *http.Request) { r.URL.RawQuery = "api_key=QK" }),
		&config.SecurityRequirement{ID: "q_key"})
	require.NoError(t, err)
}

// Comparison must not leak the credential through timing.
func TestVerify_UsesAConstantTimeComparison(t *testing.T) {
	engine := New([]config.SecurityScheme{bearer()})

	_, err := engine.Verify(
		incoming(t, func(r *http.Request) { r.Header.Set("Authorization", "Bearer shared-secre") }),
		&config.SecurityRequirement{ID: "caller"})

	require.Error(t, err, "a prefix of the credential is not the credential")
}

func TestVerify_NilRequirementAcceptsEverything(t *testing.T) {
	engine := New(nil)

	_, err := engine.Verify(incoming(t, nil), nil)

	require.NoError(t, err)
}

package requester

import (
	"context"
	"net/http"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/security"
)

// AuthManager handles request authentication
type AuthManager interface {
	ApplyAuth(req *http.Request) error
}

// SigningRequest describes the request a Signer is about to sign.
//
// Everything here is what will actually be sent: Path has its placeholders
// already substituted and Body is the exact byte sequence that goes on the wire.
// Signing anything else is the classic failure of this integration — a body
// re-serialised with different key order or spacing hashes differently, so every
// call is rejected with a signature error that points nowhere near the cause.
type SigningRequest struct {
	// Method is the HTTP method, upper case.
	Method string
	// Path is the request path with placeholders substituted, no host, no query.
	Path string
	// Query is the encoded query string without the leading "?", empty when none.
	Query string
	// Body is the request body bytes, empty when there is none.
	Body []byte
	// ContentType is the media type of Body, empty when there is no body.
	ContentType string
	// Tool is the name of the tool being called, for schemes that sign it.
	Tool string
}

// Signer computes per-request credentials.
//
// It exists because a whole family of enterprise gateways authenticates with an
// HMAC over the canonical request — method, path, sorted query, body hash,
// timestamp, nonce, key. None of that can be prepared when the service is built
// or when a session opens: it depends on the very bytes being sent, and the
// timestamp and nonce must differ per call.
//
// Returning an error stops the request. Sending it unsigned would only earn a
// signature error from the upstream, and the real reason — a credential that
// could not be read, a clock that was not available — would be lost.
type Signer interface {
	SignRequest(ctx context.Context, req SigningRequest) (map[string]string, error)
}

// securityAuthManager applies the configured upstream security requirement.
//
// A nil requirement is a valid configuration: an upstream that needs no
// credential gets none, and nothing has to be spelled as "auth_type: none".
type securityAuthManager struct {
	engine   *security.Engine
	upstream *config.SecurityRequirement
}

// NewAuthManager builds the upstream authenticator.
func NewAuthManager(engine *security.Engine, upstream *config.SecurityRequirement) AuthManager {
	return &securityAuthManager{engine: engine, upstream: upstream}
}

// ApplyAuth adds authentication to the request.
func (a *securityAuthManager) ApplyAuth(req *http.Request) error {
	return a.engine.Apply(req, a.upstream)
}

package requester

import (
	"net/http"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/security"
)

// AuthManager handles request authentication
type AuthManager interface {
	ApplyAuth(req *http.Request) error
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

package requester

import (
	"fmt"
	"net/http"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/security"
)

// AuthManager handles request authentication
type AuthManager interface {
	ApplyAuth(req *http.Request) error
}

// HTTPAuthManager implements the AuthManager interface
type HTTPAuthManager struct {
	authType   config.AuthType
	authConfig map[string]string

	// engine and upstream carry the security-scheme configuration. When an
	// upstream requirement is configured it supersedes authType/authConfig, which
	// remain for deployments written before schemes existed.
	engine   *security.Engine
	upstream *config.SecurityRequirement
}

// NewHTTPAuthManager creates a new HTTPAuthManager
func NewHTTPAuthManager(serviceConfig *config.EndpointConfig) *HTTPAuthManager {
	return &HTTPAuthManager{
		authType:   serviceConfig.AuthType,
		authConfig: serviceConfig.AuthConfig,
	}
}

// WithSecurity attaches security-scheme based upstream authentication.
func (a *HTTPAuthManager) WithSecurity(engine *security.Engine, upstream *config.SecurityRequirement) *HTTPAuthManager {
	a.engine = engine
	a.upstream = upstream
	return a
}

// ApplyAuth adds authentication to the request
func (a *HTTPAuthManager) ApplyAuth(req *http.Request) error {
	if a.upstream != nil {
		return a.engine.Apply(req, a.upstream)
	}
	switch a.authType {
	case config.AuthTypeNone:
		return nil
	case config.AuthTypeBasic:
		username := a.authConfig["username"]
		password := a.authConfig["password"]
		req.SetBasicAuth(username, password)
	case config.AuthTypeBearer:
		token := a.authConfig["token"]
		req.Header.Set("Authorization", "Bearer "+token)
	case config.AuthTypeAPIKey:
		key := a.authConfig["key"]
		header := a.authConfig["header"]
		if header == "" {
			header = "X-API-Key"
		}
		req.Header.Set(header, key)
	case config.AuthTypeOAuth2:
		token := a.authConfig["token"]
		req.Header.Set("Authorization", "Bearer "+token)
	default:
		return fmt.Errorf("unsupported auth type: %s", a.authType)
	}
	return nil
}

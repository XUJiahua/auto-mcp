package requester

import (
	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/security"
	"go.uber.org/fx"
)

// newAuthManager builds the upstream authenticator, preferring the configured
// security scheme over the older endpoint auth_type when both are present.
func newAuthManager(cfg *config.Config, endpoint *config.EndpointConfig) AuthManager {
	manager := NewHTTPAuthManager(endpoint)
	if cfg.UpstreamSecurity != nil {
		manager = manager.WithSecurity(security.New(cfg.SecuritySchemes), cfg.UpstreamSecurity)
	}
	return manager
}

// Module provides the requester module dependencies
var Module = fx.Options(
	fx.Provide(
		NewHTTPRequester,
		newAuthManager,
		NewHTTPRequestBuilder,
	),
)

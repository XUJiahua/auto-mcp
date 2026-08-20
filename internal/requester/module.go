package requester

import (
	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/security"
	"go.uber.org/fx"
)

func newAuthManager(cfg *config.Config) AuthManager {
	return NewAuthManager(security.New(cfg.SecuritySchemes), cfg.UpstreamSecurity)
}

// Module provides the requester module dependencies
var Module = fx.Options(
	fx.Provide(
		NewHTTPRequester,
		newAuthManager,
		NewHTTPRequestBuilder,
	),
)

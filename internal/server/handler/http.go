// Package handler provides HTTP request handling for the MCP server.
package handler

import (
	"errors"
	"net/http"

	"github.com/brizzai/auto-mcp/internal/auth"
	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/logger"
	"github.com/brizzai/auto-mcp/internal/security"
	"go.uber.org/zap"
)

// Handler manages HTTP request handling and middleware configuration.
type Handler struct {
	auth       *auth.Service
	security   *security.Engine
	downstream *config.SecurityRequirement
}

// NewHandler creates a new HTTP handler.
func NewHandler(auth *auth.Service, engine *security.Engine, downstream *config.SecurityRequirement) *Handler {
	return &Handler{
		auth:       auth,
		security:   engine,
		downstream: downstream,
	}
}

// authenticateDownstream rejects callers that do not present the configured
// credential, and records the one they did present so that an upstream
// requirement configured for passthrough can forward it.
func (h *Handler) authenticateDownstream(next http.Handler) http.Handler {
	if h.downstream == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential, err := h.security.Verify(r, h.downstream)
		if err != nil {
			if !errors.Is(err, security.ErrUnauthenticated) {
				// A misconfiguration, not a rejected caller.
				logger.Error("Downstream authentication is misconfigured", zap.Error(err))
			}
			// The response says only that authentication failed. Explaining the
			// difference between "absent" and "wrong" tells a prober which of
			// the two it achieved.
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(security.WithPassthroughCredential(r.Context(), credential)))
	})
}

// CreateHTTPHandler creates an HTTP handler with the appropriate middleware stack.
// If authentication is enabled, it adds authentication middleware to protected routes.
func (h *Handler) CreateHTTPHandler(mcpHandler http.Handler) http.Handler {
	mux := http.NewServeMux()

	// Set up authentication routes and middleware if enabled
	if h.auth != nil {
		h.auth.RegisterRoutes(mux)
		logger.Info("Registered authentication routes")
		mux.Handle("/", h.auth.Authenticate()(mcpHandler))
		logger.Info("Enabled authentication for all routes")
		return h.auth.WrapWithCors(mux)
	}

	if h.downstream != nil {
		mux.Handle("/", h.authenticateDownstream(mcpHandler))
		logger.Info("Enabled downstream authentication",
			zap.String("scheme", h.downstream.ID))
		return mux
	}

	mux.Handle("/", mcpHandler)
	logger.Info("Running without authentication")
	return mux
}

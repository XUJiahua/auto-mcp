// Package handler provides HTTP request handling for the MCP server.
package handler

import (
	"errors"
	"net/http"
	"strings"
	"sync"

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
	// services is the set of route segments that exist. A single unnamed service
	// is represented by the empty string and answers on any path. A reload
	// rewrites it while requests are being served, so it is guarded.
	mu       sync.RWMutex
	services map[string]bool
}

// NewHandler creates a new HTTP handler.
func NewHandler(auth *auth.Service, engine *security.Engine,
	downstream *config.SecurityRequirement, services []string) *Handler {

	h := &Handler{
		auth:       auth,
		security:   engine,
		downstream: downstream,
	}
	h.SetServices(services)
	return h
}

// SetServices replaces the set of routable service names.
func (h *Handler) SetServices(services []string) {
	known := make(map[string]bool, len(services))
	for _, name := range services {
		known[name] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.services = known
}

func (h *Handler) knows(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.services[name]
}

// requireKnownService rejects a route that names no configured service.
//
// The SDK answers an unresolved server with 400 "no server available"; a missing
// service is a wrong address, so it is reported as 404. Without this a typo in
// the route would present as a service that exists and has no tools.
func (h *Handler) requireKnownService(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The single-service form answers on any path, so it is checked per
		// request rather than once: a reload can change which form is in effect.
		if h.knows("") {
			next.ServeHTTP(w, r)
			return
		}
		if !h.knows(serviceNameFromPath(r.URL.Path)) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serviceNameFromPath reads the service name out of /mcp/{name}/...
func serviceNameFromPath(path string) string {
	rest := strings.TrimPrefix(path, "/mcp")
	rest = strings.TrimPrefix(rest, "/")
	name, _, _ := strings.Cut(rest, "/")
	return name
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
	routed := h.requireKnownService(mcpHandler)

	if h.auth != nil {
		h.auth.RegisterRoutes(mux)
		logger.Info("Registered authentication routes")
		mux.Handle("/", h.auth.Authenticate()(routed))
		logger.Info("Enabled authentication for all routes")
		return h.auth.WrapWithCors(mux)
	}

	if h.downstream != nil {
		mux.Handle("/", h.authenticateDownstream(routed))
		logger.Info("Enabled downstream authentication",
			zap.String("scheme", h.downstream.ID))
		return mux
	}

	mux.Handle("/", routed)
	logger.Info("Running without authentication")
	return mux
}

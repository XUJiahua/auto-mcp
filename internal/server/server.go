// Package server provides the core MCP (Model Control Protocol) server implementation.
package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/brizzai/auto-mcp/internal/auth"
	"github.com/brizzai/auto-mcp/internal/auth/providers"
	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/logger"
	"github.com/brizzai/auto-mcp/internal/parser"
	"github.com/brizzai/auto-mcp/internal/requester"
	"github.com/brizzai/auto-mcp/internal/security"
	"github.com/brizzai/auto-mcp/internal/server/handler"
	"github.com/brizzai/auto-mcp/internal/server/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	// shutdownTimeout is the maximum time to wait for server shutdown
	shutdownTimeout = 5 * time.Second
)

// ErrInvalidOAuthProvider indicates an unsupported OAuth provider was specified
var ErrInvalidOAuthProvider = fmt.Errorf("unsupported OAuth provider")

// Server exposes one MCP endpoint per configured service.
//
// Each service is a separate upstream with its own document, human edits,
// address and credential, so each gets its own mcp.Server and its own request
// builder. Nothing is shared between them but the listener and the front-door
// authentication, which is what keeps one upstream's credential from reaching
// another's endpoint.
type Server struct {
	config   *config.Config
	services map[string]*mcp.Server
	// single is set when the configuration names no services, in which case the
	// one endpoint answers on any path and keeps its address.
	single  *mcp.Server
	auth    *auth.Service
	handler *handler.Handler
	tool    *tool.Handler
}

// NewServer creates a new MCP server instance with the provided configuration.
// It initializes the server with the given parser and requester, and sets up
// authentication if enabled in the configuration.
func NewServer(cfg *config.Config) *Server {
	if cfg == nil {
		logger.Fatal("Config cannot be nil")
	}

	srv := &Server{
		config:   cfg,
		services: map[string]*mcp.Server{},
	}

	if cfg.OAuth != nil && cfg.OAuth.Enabled {
		if err := srv.setupAuth(); err != nil {
			logger.Fatal("Failed to setup authentication", zap.Error(err))
		}
	}

	srv.tool = tool.NewHandler(srv.auth != nil)

	if err := srv.setupTools(); err != nil {
		logger.Fatal("Failed to setup tools", zap.Error(err))
	}

	// The HTTP handler routes on the service names, so it can only be built once
	// the services exist.
	srv.handler = handler.NewHandler(srv.auth, security.New(cfg.SecuritySchemes),
		cfg.DownstreamSecurity, srv.ServiceNames())

	return srv
}

func (s *Server) setupAuth() error {
	var provider providers.OAuthProvider
	var err error

	switch s.config.OAuth.Provider {
	case "google":
		provider, err = providers.NewGoogleProvider(s.config.OAuth)
	case "github":
		provider = providers.NewGitHubProvider(s.config.OAuth)
	default:
		return fmt.Errorf("%w: %s", ErrInvalidOAuthProvider, s.config.OAuth.Provider)
	}

	if err != nil {
		return fmt.Errorf("failed to initialize provider %s: %w", s.config.OAuth.Provider, err)
	}

	authService, err := auth.NewService(s.config.OAuth, provider)
	if err != nil {
		return fmt.Errorf("failed to create auth service: %w", err)
	}

	s.auth = authService
	return nil
}

func (s *Server) setupTools() error {
	services := s.config.ResolvedServices()
	if len(services) == 0 {
		return fmt.Errorf("no service is configured")
	}

	engine := security.New(s.config.SecuritySchemes)
	for _, service := range services {
		mcpServer, err := s.buildService(service, engine)
		if err != nil {
			return err
		}
		s.services[service.Name] = mcpServer
		if service.Name == "" {
			s.single = mcpServer
		}
		logger.Info("Registered service",
			zap.String("route", service.RoutePath()),
			zap.String("spec", service.SwaggerFile))
	}
	return nil
}

// buildService turns one service configuration into an MCP server.
//
// The parser and the request builder are per-service instances rather than
// shared ones: a parser holds the document it read, and a builder holds the
// address and credential it sends to. Sharing either would let one upstream's
// configuration answer for another.
func (s *Server) buildService(service config.ServiceConfig, engine *security.Engine) (*mcp.Server, error) {
	name := s.config.Server.Name
	if service.Name != "" {
		name = fmt.Sprintf("%s/%s", name, service.Name)
	}
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    name,
		Version: s.config.Server.Version,
	}, nil)

	specParser := parser.NewSwaggerParser(parser.NewAdjuster())
	if err := specParser.Init(service.SwaggerFile, service.AdjustmentFile); err != nil {
		return nil, fmt.Errorf("service %q: failed to initialize parser: %w", service.Name, err)
	}

	endpoint := service.Endpoint
	upstream := requester.NewRequester(&endpoint,
		requester.NewAuthManager(engine, service.UpstreamSecurity))

	for _, route := range specParser.GetRouteTools() {
		executor, err := upstream.BuildRouteExecutor(route.RouteConfig)
		if err != nil {
			logger.Error("Failed to build route executor",
				zap.String("service", service.Name),
				zap.String("tool", route.Tool.Name), zap.Error(err))
			continue
		}
		mcpServer.AddTool(route.Tool, s.tool.CreateHandler(route.Tool, route.ResponseTemplate, executor))
	}
	return mcpServer, nil
}

func (s *Server) ServeSSE(ctx context.Context) error {
	logger.Info("Starting SSE server")
	return s.serveHTTP(ctx, mcp.NewSSEHandler(s.serverForRequest, nil), "SSE")
}

func (s *Server) ServeHTTP(ctx context.Context) error {
	logger.Info("Starting HTTP server")
	return s.serveHTTP(ctx, mcp.NewStreamableHTTPHandler(s.serverForRequest, nil), "HTTP")
}

// serverForRequest picks the MCP server that should handle an incoming request.
//
// Both HTTP transports resolve the server per request rather than binding one at
// construction, which is what lets one listener serve several upstreams. The
// service is identified by the route segment after /mcp.
func (s *Server) serverForRequest(r *http.Request) *mcp.Server {
	if s.single != nil {
		return s.single
	}
	return s.services[serviceNameFromPath(r.URL.Path)]
}

// serviceNameFromPath reads the service name out of /mcp/{name}/...
func serviceNameFromPath(path string) string {
	rest := strings.TrimPrefix(path, "/mcp")
	rest = strings.TrimPrefix(rest, "/")
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// ServiceNames returns the configured service names, for the HTTP handler to
// route on. An empty name means the single-service form, which answers anywhere.
func (s *Server) ServiceNames() []string {
	names := make([]string, 0, len(s.services))
	for name := range s.services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Server) serveHTTP(ctx context.Context, handler http.Handler, mode string) error {
	addr := fmt.Sprintf("%s:%d", s.config.Server.Host, s.config.Server.Port)
	server := &http.Server{
		Addr:    addr,
		Handler: s.handler.CreateHTTPHandler(handler),
	}

	// Channel for server errors
	errChan := make(chan error, 1)

	// Start server in a goroutine
	go func() {
		logger.Info("Starting server",
			zap.String("mode", mode),
			zap.String("address", addr),
		)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("server error: %w", err)
		}
	}()

	// Wait for context cancellation or server error
	select {
	case <-ctx.Done():
		logger.Info("Shutting down server",
			zap.String("mode", mode),
			zap.Duration("timeout", shutdownTimeout),
		)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server shutdown error: %w", err)
		}
		return nil

	case err := <-errChan:
		return err
	}
}

func (s *Server) ServeSTDIO(ctx context.Context) error {
	logger.Info("Starting STDIO server")
	// setupTools rejects several services in stdio mode, so exactly one exists.
	for _, mcpServer := range s.services {
		return mcpServer.Run(ctx, &mcp.StdioTransport{})
	}
	return fmt.Errorf("no service is configured")
}

// Start starts the server in the configured mode (SSE, HTTP, or STDIO).
// It returns an error if the server fails to start or encounters an error
// during operation.
func (s *Server) Start(ctx context.Context) error {
	logger.Info("Starting server",
		zap.String("mode", string(s.config.Server.Mode)),
		zap.String("version", s.config.Server.Version),
	)

	switch s.config.Server.Mode {
	case config.ServerModeSSE:
		return s.ServeSSE(ctx)
	case config.ServerModeHTTP:
		return s.ServeHTTP(ctx)
	case config.ServerModeSTDIO:
		return s.ServeSTDIO(ctx)
	default:
		return fmt.Errorf("unsupported server mode: %s", s.config.Server.Mode)
	}
}

// Module provides the MCP server dependencies
var Module = fx.Module("mcp_server",
	fx.Provide(
		NewServer,
	),
)

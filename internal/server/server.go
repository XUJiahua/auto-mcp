// Package server provides the core MCP (Model Control Protocol) server implementation.
package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
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
	config *config.Config

	// mu guards services, single and toolNames, which a reload rewrites while
	// requests are being routed.
	mu       sync.RWMutex
	services map[string]*mcp.Server
	// single is set when the configuration names no services, in which case the
	// one endpoint answers on any path and keeps its address.
	single *mcp.Server
	// toolNames records what each service currently exposes, so a reload can
	// remove exactly the tools that went away.
	toolNames map[string][]string
	// schemaSizes records what each service publishes, for reporting.
	schemaSizes map[string]SchemaSize

	auth    *auth.Service
	handler *handler.Handler
	tool    *tool.Handler
}

// NewServer creates a new MCP server instance with the provided configuration.
// It initializes the server with the given parser and requester, and sets up
// authentication if enabled in the configuration.
// NewServer creates the server, failing the process if it cannot be built.
//
// It is the entry point for the dependency graph, which has no way to handle an
// error. New is the same thing with the error returned, which is what makes the
// failure paths testable.
func NewServer(cfg *config.Config) *Server {
	srv, err := New(cfg)
	if err != nil {
		logger.Fatal("Failed to create server", zap.Error(err))
	}
	return srv
}

// New creates the server from a configuration.
func New(cfg *config.Config) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	srv := &Server{
		config:      cfg,
		services:    map[string]*mcp.Server{},
		toolNames:   map[string][]string{},
		schemaSizes: map[string]SchemaSize{},
	}

	if cfg.OAuth != nil && cfg.OAuth.Enabled {
		if err := srv.setupAuth(); err != nil {
			return nil, fmt.Errorf("failed to setup authentication: %w", err)
		}
	}

	srv.tool = tool.NewHandler(srv.auth != nil)

	if err := srv.setupTools(); err != nil {
		return nil, err
	}

	// The HTTP handler routes on the service names, so it can only be built once
	// the services exist.
	srv.handler = handler.NewHandler(srv.auth, security.New(cfg.SecuritySchemes),
		cfg.DownstreamSecurity, srv.ServiceNames())

	return srv, nil
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
	return s.apply(s.config.ResolvedServices())
}

// Reload rescans the configured services directory and brings the running server
// in line with what it finds.
//
// Services are updated in place rather than replaced: an existing service keeps
// its mcp.Server, so open sessions stay connected, and the SDK emits
// notifications/tools/list_changed as tools are added and removed. That is the
// protocol's own answer to a changing tool set, which is why a reload neither
// disconnects clients nor leaves them looking at a list that no longer exists.
//
// A reload that cannot be completed changes nothing. Every new service is built
// before anything is swapped in, so a spec that stopped parsing leaves a process
// that was serving correctly still serving.
func (s *Server) Reload() error {
	if err := s.config.Rediscover(); err != nil {
		return fmt.Errorf("rescanning services: %w", err)
	}
	return s.apply(s.config.ResolvedServices())
}

// apply makes the running set of services match the given configuration.
func (s *Server) apply(services []config.ServiceConfig) error {
	if len(services) == 0 {
		return fmt.Errorf("no service is configured")
	}

	engine := security.New(s.config.SecuritySchemes)

	// Everything is built first. Registering as we go would leave a half-applied
	// configuration behind when a later service fails to parse.
	type built struct {
		service config.ServiceConfig
		tools   []*registeredTool
		size    SchemaSize
	}
	prepared := make([]built, 0, len(services))
	for _, service := range services {
		tools, err := s.buildTools(service, engine)
		if err != nil {
			return err
		}
		size, err := measureSchemas(service.Name, tools, s.config.MaxToolSchemaKiB)
		if err != nil {
			return err
		}
		prepared = append(prepared, built{service: service, tools: tools, size: size})
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	live := make(map[string]bool, len(prepared))
	for _, entry := range prepared {
		name := entry.service.Name
		live[name] = true

		mcpServer, existing := s.services[name]
		if !existing {
			mcpServer = mcp.NewServer(&mcp.Implementation{
				Name:    s.serviceIdentity(entry.service),
				Version: s.config.Server.Version,
			}, nil)
			s.services[name] = mcpServer
			if name == "" {
				s.single = mcpServer
			}
		}

		// Removing the previous tools before adding the new ones keeps a renamed
		// operation from lingering under its old name.
		if previous := s.toolNames[name]; len(previous) > 0 {
			mcpServer.RemoveTools(previous...)
		}
		names := make([]string, 0, len(entry.tools))
		for _, registered := range entry.tools {
			mcpServer.AddTool(registered.tool, registered.handler)
			names = append(names, registered.tool.Name)
		}
		s.toolNames[name] = names
		s.schemaSizes[name] = entry.size

		action := "Registered service"
		if existing {
			action = "Updated service"
		}
		// The schema size is logged with the registration because it is a running
		// cost: every tools/list carries it into a model's context.
		logger.Info(action,
			zap.String("route", entry.service.RoutePath()),
			zap.String("spec", entry.service.SwaggerFile),
			zap.Int("tools", len(names)),
			zap.Int("schema_bytes", entry.size.TotalBytes),
			zap.String("largest_tool", entry.size.LargestTool),
			zap.Int("largest_tool_bytes", entry.size.LargestBytes))
	}

	for name := range s.services {
		if live[name] {
			continue
		}
		delete(s.services, name)
		delete(s.toolNames, name)
		delete(s.schemaSizes, name)
		if name == "" {
			s.single = nil
		}
		logger.Info("Removed service", zap.String("name", name))
	}

	if s.handler != nil {
		s.handler.SetServices(s.serviceNamesLocked())
	}
	return nil
}

// registeredTool pairs a tool with the handler that executes it.
type registeredTool struct {
	tool    *mcp.Tool
	handler mcp.ToolHandler
}

func (s *Server) serviceIdentity(service config.ServiceConfig) string {
	if service.Name == "" {
		return s.config.Server.Name
	}
	return fmt.Sprintf("%s/%s", s.config.Server.Name, service.Name)
}

// buildTools reads one service's document and prepares its tools.
//
// The parser and the request builder are per-service instances rather than
// shared ones: a parser holds the document it read, and a builder holds the
// address and credential it sends to. Sharing either would let one upstream's
// configuration answer for another.
func (s *Server) buildTools(service config.ServiceConfig, engine *security.Engine) ([]*registeredTool, error) {
	specParser := parser.NewSwaggerParser(parser.NewAdjuster())
	if err := specParser.Init(service.SwaggerFile, service.AdjustmentFile); err != nil {
		return nil, fmt.Errorf("service %q: failed to initialize parser: %w", service.Name, err)
	}

	endpoint := service.Endpoint
	upstream := requester.NewRequester(&endpoint,
		requester.NewAuthManager(engine, service.UpstreamSecurity))

	routes := specParser.GetRouteTools()
	tools := make([]*registeredTool, 0, len(routes))
	for _, route := range routes {
		executor, err := upstream.BuildRouteExecutor(route.RouteConfig)
		if err != nil {
			logger.Error("Failed to build route executor",
				zap.String("service", service.Name),
				zap.String("tool", route.Tool.Name), zap.Error(err))
			continue
		}
		tools = append(tools, &registeredTool{
			tool:    route.Tool,
			handler: s.tool.CreateHandler(route.Tool, route.ResponseTemplate, executor),
		})
	}
	return tools, nil
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
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.serviceNamesLocked()
}

func (s *Server) serviceNamesLocked() []string {
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
	s.mu.RLock()
	var only *mcp.Server
	for _, mcpServer := range s.services {
		only = mcpServer
		break
	}
	s.mu.RUnlock()
	if only == nil {
		return fmt.Errorf("no service is configured")
	}
	return only.Run(ctx, &mcp.StdioTransport{})
}

// WatchReloadSignals reloads on SIGHUP for as long as ctx is live.
//
// SIGHUP is the conventional "re-read your configuration" signal, and using it
// keeps the reload path free of a network endpoint that would itself need
// authenticating. A failed reload is logged and the running configuration is
// kept: a signal is not a good place to take a working process down.
func (s *Server) WatchReloadSignals(ctx context.Context) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)

	go func() {
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				logger.Info("Reloading configuration on SIGHUP")
				if err := s.Reload(); err != nil {
					logger.Error("Reload failed; keeping the running configuration", zap.Error(err))
					continue
				}
				logger.Info("Reload complete", zap.Strings("services", s.ServiceNames()))
			}
		}
	}()
}

// Start starts the server in the configured mode (SSE, HTTP, or STDIO).
// It returns an error if the server fails to start or encounters an error
// during operation.
func (s *Server) Start(ctx context.Context) error {
	logger.Info("Starting server",
		zap.String("mode", string(s.config.Server.Mode)),
		zap.String("version", s.config.Server.Version),
	)

	s.WatchReloadSignals(ctx)

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

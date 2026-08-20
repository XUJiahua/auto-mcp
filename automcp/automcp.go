// Package automcp turns an OpenAPI document into MCP tools that a host program
// registers on an MCP server it owns.
//
// This is the whole public surface of auto-mcp as a library. Everything else stays
// internal on purpose: the alternative — exporting the packages this is built from
// — would put the parser, the request builder, the security model and, through
// them, auto-mcp's own deployment configuration and its package-level logger into
// the host's compile-time dependencies. A host needs a handful of symbols, and
// every internal refactor would otherwise be a breaking change for it.
//
// The host keeps ownership of the parts that are its own concern: where the
// document came from, which mcp.Server the tools live on, how that server is
// served, and how credentials are resolved. This package parses, builds and hands
// back tools.
package automcp

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/parser"
	"github.com/brizzai/auto-mcp/internal/requester"
	"github.com/brizzai/auto-mcp/internal/security"
	"github.com/brizzai/auto-mcp/internal/server/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options describes one upstream API to expose.
type Options struct {
	// Spec is the OpenAPI document. It is read from memory rather than from a
	// path so that a host storing documents in a database does not have to write
	// them out first. OpenAPI 3.0, 3.1, 3.2 and Swagger 2.0 are accepted, in JSON
	// or YAML, and a document that does not conform is refused by Build.
	Spec io.Reader

	// Adjustment optionally carries the curation applied to the document:
	// which routes to expose, rewritten descriptions, and response templates.
	Adjustment io.Reader

	// BaseURL is the upstream address every generated tool calls.
	BaseURL string

	// Headers are sent with every upstream request.
	//
	// This is where a credential goes when the upstream wants one in a header.
	// The host resolves it and passes the value; auto-mcp only carries it. Keeping
	// resolution on one side is deliberate — a credential with two homes is one
	// that gets rotated in only one of them.
	Headers map[string]string

	// Timeout bounds a single upstream request. Zero uses the default.
	Timeout time.Duration
}

// Tool is a generated tool together with the handler that executes it.
type Tool struct {
	Tool    *mcp.Tool
	Handler mcp.ToolHandler
}

// Service is the set of tools generated from one document.
type Service struct {
	tools       []Tool
	schemaBytes int
}

// Build parses the document and prepares its tools.
//
// Nothing is registered and no request is made: a host can build a service purely
// to inspect what a document would produce, which is what an onboarding flow needs
// in order to show someone the result before committing to it.
func Build(opts Options) (*Service, error) {
	if opts.Spec == nil {
		return nil, fmt.Errorf("automcp: a spec reader is required")
	}
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("automcp: a base URL is required; generated tools have nowhere to call without it")
	}

	adjuster := parser.NewAdjuster()
	if opts.Adjustment != nil {
		if err := adjuster.LoadReader(opts.Adjustment); err != nil {
			return nil, fmt.Errorf("automcp: %w", err)
		}
	}

	specParser := parser.NewSwaggerParser(adjuster)
	if err := specParser.ParseReader(opts.Spec); err != nil {
		return nil, fmt.Errorf("automcp: %w", err)
	}

	upstream := requester.NewRequester(
		&config.EndpointConfig{BaseURL: opts.BaseURL, Headers: opts.Headers},
		// No security requirement: the host supplies whatever the upstream needs
		// through Headers or in the tool arguments it sends.
		requester.NewAuthManager(security.New(nil), nil),
	)
	if opts.Timeout > 0 {
		upstream.SetTimeout(opts.Timeout)
	}

	// Auth is the host's concern, so the tool handler does no checking of its own.
	handlers := tool.NewHandler(false)

	service := &Service{}
	for _, route := range specParser.GetRouteTools() {
		executor, err := upstream.BuildRouteExecutor(route.RouteConfig)
		if err != nil {
			return nil, fmt.Errorf("automcp: tool %q: %w", route.Tool.Name, err)
		}
		encoded, err := json.Marshal(route.Tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("automcp: tool %q has an unserialisable input schema: %w",
				route.Tool.Name, err)
		}
		service.schemaBytes += len(encoded)
		service.tools = append(service.tools, Tool{
			Tool:    route.Tool,
			Handler: handlers.CreateHandler(route.Tool, route.ResponseTemplate, executor),
		})
	}
	return service, nil
}

// Register adds every tool to the given server.
//
// The server belongs to the host, which is what makes several documents servable
// from one process without a second one: the host decides whether they share a
// server or get one each, and how each is addressed.
func (s *Service) Register(server *mcp.Server) {
	for _, t := range s.tools {
		server.AddTool(t.Tool, t.Handler)
	}
}

// Tools returns the generated tools, for inspection before or instead of serving.
func (s *Service) Tools() []Tool {
	out := make([]Tool, len(s.tools))
	copy(out, s.tools)
	return out
}

// SchemaBytes is the total size of the published input schemas.
//
// It is worth showing to whoever uploaded the document: every tools/list carries
// this into a model's context, so it is a running cost rather than a one-off, and
// a single tool of 35 KiB has been measured in the wild.
func (s *Service) SchemaBytes() int {
	return s.schemaBytes
}

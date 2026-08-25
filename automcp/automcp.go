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
	"context"
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

	// Signer computes credentials that depend on the request itself. Optional.
	//
	// Headers above cover the common case: a fixed credential the host resolved
	// once. A whole family of enterprise gateways cannot be served that way ——
	// they authenticate with an HMAC over the canonical request (method, path,
	// sorted query, body hash, timestamp, nonce, key), so the credential differs
	// for every call and cannot exist before the body does.
	//
	// The division of labour is the same as for Headers: the host owns the secret
	// and the algorithm, auto-mcp only tells it what is about to be sent and
	// carries what comes back.
	Signer Signer
	// URLBuilder rewrites the URL for each request. Optional; without it the URL
	// comes from the document exactly as before.
	URLBuilder URLBuilder
	// BodyBuilder rewrites the request body. Optional. It runs before Signer, so
	// what gets signed is what gets sent.
	BodyBuilder BodyBuilder

	// Lenient normalises a document that is conformant in substance but not in
	// letter, instead of refusing it. Off by default.
	//
	// It covers exactly two things, and both are reported by Notices:
	//   - example / examples values that disagree with their schema. Those are
	//     annotations: a tool's contract comes from type / properties / required,
	//     and an example only becomes a suggested value.
	//   - a document declaring 3.0.x while using 3.1-only type constructs. Such a
	//     document is 3.1 with a wrong version line.
	//
	// Structural problems are still refused with it on. A wrong example earns a
	// rejection from the upstream, which is visible and fixable; a wrong type does
	// not — it publishes a tool that looks right and is not.
	Lenient bool

	// Timeout bounds a single upstream request. Zero uses the default.
	Timeout time.Duration
}

// SigningRequest describes the request a Signer is about to sign.
//
// Everything here is what will actually be sent: Path has its placeholders
// already substituted, and Body is the exact byte sequence that goes on the wire.
// Signing anything else is the classic failure of this integration — a body
// re-serialised with different key order or spacing hashes differently, so every
// call is rejected for a reason that points nowhere near the cause.
type SigningRequest = requester.SigningRequest

// Signer computes per-request credentials.
//
// Returning an error stops the request. Sending it unsigned would only earn a
// rejection from the upstream, and the real reason — a credential that could not
// be read, a clock that was not available — would be lost on the way.
type Signer = requester.Signer

// SignerFunc adapts a plain function to Signer.
type SignerFunc func(ctx context.Context, req SigningRequest) (map[string]string, error)

// SignRequest implements Signer.
func (f SignerFunc) SignRequest(ctx context.Context, req SigningRequest) (map[string]string, error) {
	return f(ctx, req)
}

// BuildRequest is what a URL or body builder sees.
//
// It carries both the operation as the document declares it (Method, Path) and
// what the platform computed by default (URL, Body, ContentType). A hook needs
// both: the template says which operation this is, the default says what would
// have been sent.
type BuildRequest = requester.BuildRequest

// URLBuilder computes the URL for a request.
//
// It exists because a real supplier's path rules are often absent from the
// document. Shixin/Flink is the case that forced it: dictionaries live under
// /api/{lang}/…, flights under /api/v2/{lang}/…, and one endpoint is the literal
// /api/lang/fields. Without a hook the only way to express that is to hand-write
// every route into an adjustment file — which puts merchant-specific logic in a
// place that was never meant to carry it.
//
// Returning an error stops the request. Sending a half-built one would only earn
// a rejection whose message cannot say which part was wrong.
type URLBuilder = requester.URLBuilder

// URLBuilderFunc adapts a plain function to URLBuilder.
type URLBuilderFunc func(ctx context.Context, req BuildRequest) (string, error)

// BuildURL implements URLBuilder.
func (f URLBuilderFunc) BuildURL(ctx context.Context, req BuildRequest) (string, error) {
	return f(ctx, req)
}

// BodyBuilder computes the request body.
//
// It runs *before* the signer, so a signature is computed over the bytes this
// returns. The other order would produce a signature over bytes that were never
// sent, and the symptom is every call being rejected with a message that cannot
// point at the cause.
type BodyBuilder = requester.BodyBuilder

// BodyBuilderFunc adapts a plain function to BodyBuilder.
type BodyBuilderFunc func(ctx context.Context, req BuildRequest) ([]byte, string, error)

// BuildBody implements BodyBuilder.
func (f BodyBuilderFunc) BuildBody(ctx context.Context, req BuildRequest) ([]byte, string, error) {
	return f(ctx, req)
}

// Tool is a generated tool together with the handler that executes it.
type Tool struct {
	Tool    *mcp.Tool
	Handler mcp.ToolHandler
	// Method and Path are the operation this tool came from.
	//
	// A host that lets someone choose which operations to take needs them: the
	// choice is expressed as paths and methods, which is what an adjustment file
	// selects on, while the person choosing sees tool names. Without the route
	// here a host can only offer a raw file to write by hand, and nobody can write
	// that for a document they have just uploaded.
	//
	// They are also worth showing on their own: "which endpoint does this tool
	// call" is the first question anyone asks of a generated tool.
	Method string
	Path   string
}

// Service is the set of tools generated from one document.
type Service struct {
	tools       []Tool
	schemaBytes int
	notices     []string
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
	specParser.SetLenient(opts.Lenient)
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
	if opts.URLBuilder != nil {
		upstream.SetURLBuilder(opts.URLBuilder)
	}
	if opts.BodyBuilder != nil {
		upstream.SetBodyBuilder(opts.BodyBuilder)
	}
	if opts.Signer != nil {
		upstream.SetSigner(opts.Signer)
	}

	// Auth is the host's concern, so the tool handler does no checking of its own.
	handlers := tool.NewHandler(false)

	service := &Service{notices: specParser.Notices()}
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
			Method:  route.RouteConfig.Method,
			Path:    route.RouteConfig.Path,
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

// Notices reports what Lenient changed, one line each. Empty when nothing was.
//
// A host should show these to whoever uploaded the document: ignoring a problem
// without saying so is the silent degradation that makes leniency a hole rather
// than a convenience.
func (s *Service) Notices() []string {
	out := make([]string, len(s.notices))
	copy(out, s.notices)
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

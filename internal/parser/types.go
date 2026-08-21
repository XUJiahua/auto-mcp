package parser

import (
	"io"

	"github.com/brizzai/auto-mcp/internal/models"
	"github.com/brizzai/auto-mcp/internal/requester"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RouteTool combines a route configuration with its corresponding MCP tool
type RouteTool struct {
	RouteConfig *requester.RouteConfig
	Tool        *mcp.Tool
	// ResponseTemplate reshapes the upstream response, when one is configured.
	ResponseTemplate *models.ResponseTemplate
}

// Parser handles parsing of Swagger/OpenAPI specifications
type Parser interface {
	// Init parses a Swagger/OpenAPI specification from a file
	Init(openAPISpec string, adjustmentsFile string) error
	// ParseReader parses a Swagger/OpenAPI specification from a reader
	ParseReader(reader io.Reader) error
	// GetRouteTools returns the parsed route tools
	GetRouteTools() []*RouteTool
}

// SwaggerParser parses Swagger specifications and generates route configurations
type SwaggerParser struct {
	// lenient normalises a document that is conformant in substance but not in
	// letter. Off by default: nothing is changed unless a host asks for it.
	lenient bool
	// notices records every change lenient mode made, one line each.
	//
	// Ignoring a problem without saying so is the silent degradation this project
	// refuses; reporting is what makes the leniency acceptable rather than a hole.
	notices []string

	doc        *openapi3.T
	routeTools []*RouteTool
	adjuster   *Adjuster
	// usedToolNames guards against a spec that repeats an operationId.
	usedToolNames map[string]bool
}

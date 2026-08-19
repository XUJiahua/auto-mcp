package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/brizzai/auto-mcp/internal/logger"
	"github.com/brizzai/auto-mcp/internal/requester"
	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Package parser implements OpenAPI specification parsing functionality
// for converting OpenAPI/Swagger definitions into MCP tools.

// NewSwaggerParser creates a new SwaggerParser instance
func NewSwaggerParser(adjuster *Adjuster) *SwaggerParser {
	return &SwaggerParser{
		routeTools: make([]*RouteTool, 0),
		adjuster:   adjuster,
	}
}

// GetRouteTools returns the parsed route tools
func (p *SwaggerParser) GetRouteTools() []*RouteTool {
	return p.routeTools
}

// generateTool creates an MCP tool from a route configuration
func (p *SwaggerParser) generateTool(route *requester.RouteConfig) mcp.Tool {
	operation := p.findOperation(route)

	opts := []mcp.ToolOption{
		mcp.WithDescription(fmt.Sprintf("%s %s \n %s", route.Method, route.Path, route.Description)),
	}

	opts = append(opts, methodAnnotations(route.Method)...)

	opts = append(opts, p.parameterOptions(route, operation)...)

	// Add file upload configuration
	if route.MethodConfig.FileUpload != nil {
		opts = append(opts, mcp.WithString("file",
			mcp.Required(),
			mcp.Description("File to upload"),
		))
	}

	// Add form fields
	for _, field := range route.MethodConfig.FormFields {
		opts = append(opts, mcp.WithString(field,
			mcp.Description(fmt.Sprintf("Form field: %s", field)),
		))
	}

	// Add body parameter if it's a POST/PUT/PATCH request
	if route.Method == "POST" || route.Method == "PUT" || route.Method == "PATCH" {
		p.addBodyParameter(route, &opts)
	}

	return mcp.NewTool(p.toolName(route, operation), opts...)
}

// methodAnnotations states only what the HTTP method actually establishes.
//
// mcp.NewTool ships non-nil defaults (readOnlyHint=false, destructiveHint=true)
// for every tool, so an unannotated GET arrives at the client claiming to be a
// destructive write. Those defaults are guesses presented as facts, so they are
// cleared first and then only the method's own guarantees are set:
//
//   - GET reads and does not destroy.
//   - DELETE destroys.
//   - POST/PUT/PATCH are left unset. Many of these APIs serve reads over POST,
//     so readOnlyHint=false would be a false statement about half of them, and
//     a consumer that gates confirmation on the hint would either wave through
//     writes or demand confirmation for every read.
func methodAnnotations(method string) []mcp.ToolOption {
	opts := []mcp.ToolOption{mcp.WithToolAnnotation(mcp.ToolAnnotation{})}
	switch method {
	case http.MethodGet:
		opts = append(opts,
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
		)
	case http.MethodDelete:
		opts = append(opts, mcp.WithDestructiveHintAnnotation(true))
	}
	return opts
}

// toolName prefers the operationId over method+path.
//
// For these APIs the name is the only place the operation's semantics survive:
// reads and writes are both POST, so `post_api_createorder` and
// `post_api_queryhotelinfo` are indistinguishable to anything downstream that
// classifies tools, while `createOrder` and `queryHotelInfo` are not. The
// method+path form stays as the fallback for specs without operationIds.
func (p *SwaggerParser) toolName(route *requester.RouteConfig, operation *openapi3.Operation) string {
	candidate := ""
	if operation != nil {
		candidate = sanitizeToolName(operation.OperationID)
	}
	if candidate == "" {
		path := strings.TrimPrefix(route.Path, "/")
		path = strings.ReplaceAll(path, "/", "_")
		path = strings.ReplaceAll(path, "{", "")
		path = strings.ReplaceAll(path, "}", "")
		candidate = strings.ToLower(fmt.Sprintf("%s_%s", route.Method, path))
	}
	return p.uniqueToolName(candidate)
}

// sanitizeToolName keeps letters, digits, underscore and dash and collapses
// everything else, preserving camelCase word boundaries so consumers can still
// split the name into words.
func sanitizeToolName(raw string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
			lastUnderscore = false
		case r == '_':
			if !lastUnderscore && b.Len() > 0 {
				b.WriteRune('_')
				lastUnderscore = true
			}
		default:
			if !lastUnderscore && b.Len() > 0 {
				b.WriteRune('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// uniqueToolName suffixes duplicates. A spec that repeats an operationId is
// invalid but common, and silently registering two tools under one name loses
// one of them.
func (p *SwaggerParser) uniqueToolName(base string) string {
	if p.usedToolNames == nil {
		p.usedToolNames = map[string]bool{}
	}
	if !p.usedToolNames[base] {
		p.usedToolNames[base] = true
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", base, i)
		if !p.usedToolNames[candidate] {
			p.usedToolNames[candidate] = true
			return candidate
		}
	}
}

// parameterOptions exposes path, query, header and cookie parameters with the
// type, enum, default and example the spec declared for them.
func (p *SwaggerParser) parameterOptions(route *requester.RouteConfig, operation *openapi3.Operation) []mcp.ToolOption {
	var opts []mcp.ToolOption
	declared := map[string]bool{}

	for _, param := range p.operationParameters(route, operation) {
		if param.Value == nil || param.Value.Name == "" {
			continue
		}
		value := param.Value
		declared[value.Name] = true

		schema := jsonSchemaFor(value.Schema)
		if value.Description != "" {
			schema["description"] = value.Description
		}
		if example := parameterExample(value); example != nil {
			schema["example"] = example
		}
		// Path parameters are always required regardless of what the spec says:
		// the URL cannot be built without them.
		required := value.Required || value.In == string(requester.ParamInPath)
		opts = append(opts, withSchemaProperty(value.Name, schema, required))
	}

	// Path placeholders that the spec forgot to declare still have to be
	// fillable, otherwise the URL keeps its braces and the call 404s.
	for _, name := range extractPathParams(route.Path) {
		if declared[name] {
			continue
		}
		opts = append(opts, withSchemaProperty(name, map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Path parameter: %s (not declared in the spec)", name),
		}, true))
	}
	return opts
}

// operationParameters merges path-level parameters into the operation's own;
// the spec allows declaring shared parameters once per path.
func (p *SwaggerParser) operationParameters(route *requester.RouteConfig, operation *openapi3.Operation) openapi3.Parameters {
	var out openapi3.Parameters
	if p.doc != nil && p.doc.Paths != nil {
		if pathItem := p.doc.Paths.Find(route.Path); pathItem != nil {
			out = append(out, pathItem.Parameters...)
		}
	}
	if operation != nil {
		out = append(out, operation.Parameters...)
	}
	return out
}

func parameterExample(param *openapi3.Parameter) any {
	if param.Example != nil {
		return param.Example
	}
	for _, name := range sortedExampleKeys(param.Examples) {
		if ex := param.Examples[name]; ex != nil && ex.Value != nil && ex.Value.Value != nil {
			return ex.Value.Value
		}
	}
	return nil
}

func sortedExampleKeys(examples openapi3.Examples) []string {
	out := make([]string, 0, len(examples))
	for k := range examples {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// findOperation resolves the spec operation behind a route config.
func (p *SwaggerParser) findOperation(route *requester.RouteConfig) *openapi3.Operation {
	if p.doc == nil || p.doc.Paths == nil {
		return nil
	}
	pathItem := p.doc.Paths.Find(route.Path)
	if pathItem == nil {
		return nil
	}
	switch route.Method {
	case http.MethodGet:
		return pathItem.Get
	case http.MethodPost:
		return pathItem.Post
	case http.MethodPut:
		return pathItem.Put
	case http.MethodPatch:
		return pathItem.Patch
	case http.MethodDelete:
		return pathItem.Delete
	default:
		return nil
	}
}

// addBodyParameter adds body parameters to the tool options
func (p *SwaggerParser) addBodyParameter(route *requester.RouteConfig, opts *[]mcp.ToolOption) {
	operation := p.findOperation(route)
	if operation == nil {
		logger.Debug("No operation found",
			zap.String("path", route.Path),
			zap.String("method", route.Method))
		return
	}

	schema, required := getFirstBodySchema(operation)
	if schema != nil {
		*opts = append(*opts, schemaToMCPOptions(schema, "body", required))
	}
}

func getFirstBodySchema(operation *openapi3.Operation) (*openapi3.SchemaRef, bool) {
	if operation.RequestBody != nil && operation.RequestBody.Value != nil {
		content := operation.RequestBody.Value.Content

		// If there's no content, return nil
		if len(content) == 0 {
			return nil, false
		}

		// If there's only one content type, return its schema
		if len(content) == 1 {
			for _, mediaType := range content {
				return mediaType.Schema, operation.RequestBody.Value.Required
			}
		}

		// If there are multiple content types, merge their schemas
		mergedSchema := &openapi3.SchemaRef{
			Value: &openapi3.Schema{
				Type:       &openapi3.Types{"object"},
				Properties: make(openapi3.Schemas),
			},
		}

		// Merge all schemas
		for _, mediaType := range content {
			if mediaType.Schema != nil && mediaType.Schema.Value != nil {
				for propName, propSchema := range mediaType.Schema.Value.Properties {
					mergedSchema.Value.Properties[propName] = propSchema
				}
			}
		}

		return mergedSchema, operation.RequestBody.Value.Required
	}
	return nil, false
}

func schemaTypeOf(ref *openapi3.SchemaRef) string {
	if ref == nil || ref.Value == nil || ref.Value.Type == nil || len(ref.Value.Type.Slice()) == 0 {
		return ""
	}
	return ref.Value.Type.Slice()[0]
}

// extractPathParams extracts path parameters from a URL path
func extractPathParams(path string) []string {
	var params []string
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			param := strings.TrimSuffix(strings.TrimPrefix(part, "{"), "}")
			params = append(params, param)
		}
	}
	return params
}

// probeSpecDocument decodes the spec far enough to read its version fields.
//
// A document that starts with '{' or '[' is decoded as JSON so that malformed
// JSON is still reported as malformed: YAML is a superset of JSON and go-yaml
// is lenient enough to accept things like a trailing comma, which would turn a
// hand-edited broken spec into a spec with silently missing content. Anything
// else is decoded as YAML, which is the format merchant onboarding produces.
func probeSpecDocument(data []byte) (map[string]interface{}, error) {
	trimmed := bytes.TrimSpace(data)
	var out map[string]interface{}
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		if err := json.Unmarshal(trimmed, &out); err != nil {
			return nil, fmt.Errorf("invalid JSON in OpenAPI spec: %w", err)
		}
		return out, nil
	}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("invalid YAML in OpenAPI spec: %w", err)
	}
	return out, nil
}

// detectAndParseOpenAPI attempts to parse data as either OpenAPI 2.0 or 3.0
func (p *SwaggerParser) detectAndParseOpenAPI(data []byte) error {
	jsonObj, err := probeSpecDocument(data)
	if err != nil {
		return err
	}

	// Check for version fields
	swaggerVersion, hasSwagger := jsonObj["swagger"]
	openapiVersion, hasOpenAPI := jsonObj["openapi"]

	if !hasSwagger && !hasOpenAPI {
		return fmt.Errorf("document is missing 'swagger' or 'openapi' version field")
	}

	// Try to unmarshal as OpenAPI 2.0
	if hasSwagger {
		// openapi2 has no YAML decoder, so re-encode the probed document.
		jsonData, marshalErr := json.Marshal(jsonObj)
		if marshalErr != nil {
			return fmt.Errorf("failed to normalise OpenAPI 2.0 spec to JSON: %w", marshalErr)
		}
		convertedDoc, err := p.convertOpenAPI2to3(jsonData, swaggerVersion)
		if err != nil {
			return err
		}
		p.doc = convertedDoc
		return nil
	}

	// Try to parse as OpenAPI 3.0
	if hasOpenAPI {
		if ver, ok := openapiVersion.(string); !ok || !strings.HasPrefix(ver, "3.") {
			return fmt.Errorf("unsupported OpenAPI version: %v", openapiVersion)
		}
	}

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(data)
	if err != nil {
		logger.Error("Failed to parse OpenAPI 3.0 spec", zap.Error(err))
		return fmt.Errorf("failed to parse OpenAPI spec: %w", err)
	}

	if doc == nil {
		return fmt.Errorf("failed to parse OpenAPI spec: document is empty")
	}

	logger.Info("Successfully parsed OpenAPI 3.0 spec")
	p.doc = doc
	return nil
}

// convertOpenAPI2to3 converts an OpenAPI 2.0 specification to OpenAPI 3.0
func (p *SwaggerParser) convertOpenAPI2to3(data []byte, swaggerVersion interface{}) (*openapi3.T, error) {
	var swagger2Doc openapi2.T
	if err := json.Unmarshal(data, &swagger2Doc); err != nil {
		return nil, fmt.Errorf("failed to parse OpenAPI 2.0 spec: %w", err)
	}

	if swagger2Doc.Swagger != "2.0" {
		return nil, fmt.Errorf("unsupported Swagger version: %s", swaggerVersion)
	}

	logger.Info("Detected OpenAPI 2.0 spec, converting to OpenAPI 3.0")
	convertedDoc, err := openapi2conv.ToV3(&swagger2Doc)
	if err != nil {
		logger.Error("Failed to convert OpenAPI 2.0 to 3.0", zap.Error(err))
		return nil, fmt.Errorf("failed to convert OpenAPI 2.0 to 3.0: %w", err)
	}

	logger.Info("Successfully converted OpenAPI 2.0 to 3.0")
	return convertedDoc, nil
}

// Init parses a Swagger/OpenAPI specification from a file
func (p *SwaggerParser) Init(openAPISpec string, adjustmentsFile string) error {
	data, err := os.ReadFile(openAPISpec)
	if err != nil {
		return fmt.Errorf("failed to read spec file: %w", err)
	}
	if adjustmentsFile != "" {
		err = p.adjuster.Load(adjustmentsFile)
	}
	if err != nil {
		return fmt.Errorf("failed to load adjustments file: %w", err)
	}

	if err := p.detectAndParseOpenAPI(data); err != nil {
		return err
	}

	return p.processOperations()
}

// ParseReader parses a Swagger/OpenAPI specification from a reader
func (p *SwaggerParser) ParseReader(reader io.Reader) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("failed to read swagger spec: %w", err)
	}

	if err := p.detectAndParseOpenAPI(data); err != nil {
		return err
	}

	return p.processOperations()
}

// processOperations iterates through paths and operations in the spec
func (p *SwaggerParser) processOperations() error {
	p.usedToolNames = map[string]bool{}
	for path, pathItem := range p.doc.Paths.Map() {
		httpMethods := []struct {
			Method    string
			Operation *openapi3.Operation
		}{
			{"GET", pathItem.Get},
			{"POST", pathItem.Post},
			{"PUT", pathItem.Put},
			{"DELETE", pathItem.Delete},
			{"PATCH", pathItem.Patch},
		}

		for _, httpMethod := range httpMethods {
			if httpMethod.Operation != nil {
				routeConfig := p.createRouteConfig(path, httpMethod.Method, httpMethod.Operation)
				if p.adjuster.ExistsInMCP(routeConfig.Path, routeConfig.Method) {
					tool := p.generateTool(routeConfig)
					p.routeTools = append(p.routeTools, &RouteTool{
						RouteConfig: routeConfig,
						Tool:        tool,
					})
				}
			}
		}
	}

	return nil
}

// createRouteConfig creates a route configuration from a path and operation
func (p *SwaggerParser) createRouteConfig(path, method string, operation *openapi3.Operation) *requester.RouteConfig {
	routeConfig := &requester.RouteConfig{
		Path:   path,
		Method: method,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}
	var desc string
	// Add operation description if available
	if operation.Description != "" {
		desc = operation.Description
	} else if operation.Summary != "" {
		// Fallback to summary if description is not available
		desc = operation.Summary
	}
	routeConfig.Description = p.adjuster.GetDescription(routeConfig.Path, routeConfig.Method, desc)

	// Add operation-specific headers
	if operation.Responses != nil {
		// Get the first response's content type
		for _, response := range operation.Responses.Map() {
			if response.Value != nil && response.Value.Content != nil {
				for contentType := range response.Value.Content {
					routeConfig.Headers["Accept"] = contentType
					break
				}
				break
			}
		}
	}

	// Record every declared parameter with its location so the request builder
	// does not have to infer it from the argument name.
	routeConfig.MethodConfig = requester.MethodConfig{
		QueryParams: make([]string, 0),
		Params:      make([]requester.ParamConfig, 0),
	}

	for _, param := range p.operationParameters(routeConfig, operation) {
		if param.Value == nil || param.Value.Name == "" {
			continue
		}
		value := param.Value
		cfg := requester.ParamConfig{
			Name:    value.Name,
			In:      requester.ParamLocation(value.In),
			Type:    schemaTypeOf(value.Schema),
			Explode: value.Explode == nil || *value.Explode,
		}
		routeConfig.MethodConfig.Params = append(routeConfig.MethodConfig.Params, cfg)
		if cfg.In == requester.ParamInQuery {
			routeConfig.MethodConfig.QueryParams = append(routeConfig.MethodConfig.QueryParams, value.Name)
		}
	}

	// Undeclared path placeholders still have to be substituted.
	declared := make(map[string]bool, len(routeConfig.MethodConfig.Params))
	for _, cfg := range routeConfig.MethodConfig.Params {
		declared[cfg.Name] = true
	}
	for _, name := range extractPathParams(path) {
		if declared[name] {
			continue
		}
		routeConfig.MethodConfig.Params = append(routeConfig.MethodConfig.Params, requester.ParamConfig{
			Name: name, In: requester.ParamInPath, Type: "string",
		})
	}

	return routeConfig
}

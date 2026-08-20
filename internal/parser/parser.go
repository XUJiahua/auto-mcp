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
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
func (p *SwaggerParser) generateTool(route *requester.RouteConfig) *mcp.Tool {
	operation := p.findOperation(route)

	schema := newInputSchemaBuilder()
	p.addParameters(schema, route, operation)

	if route.MethodConfig.FileUpload != nil {
		schema.add("file", map[string]any{
			"type":        "string",
			"description": "File to upload",
		}, true)
	}
	for _, field := range route.MethodConfig.FormFields {
		schema.add(field, map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Form field: %s", field),
		}, false)
	}

	if route.Method == http.MethodPost || route.Method == http.MethodPut || route.Method == http.MethodPatch {
		if body, required := p.bodyParameter(route, operation); body != nil {
			schema.add(bodyArgName, body, required)
		}
	}

	return &mcp.Tool{
		Name:         p.toolName(route, operation),
		Title:        toolTitle(operation),
		Description:  toolDescription(route, operation),
		InputSchema:  schema.build(),
		OutputSchema: responseSchema(operation),
		Annotations:  methodAnnotations(route.Method),
	}
}

// maxTitleLength keeps the display name short enough to be a display name.
const maxTitleLength = 64

// bodyArgName is the tool argument that carries the request body.
const bodyArgName = "body"

// toolTitle puts the summary where MCP expects a human-readable display name.
// The spec's display precedence is title, then annotations.title, then name.
func toolTitle(operation *openapi3.Operation) string {
	if operation == nil {
		return ""
	}
	summary := strings.TrimSpace(operation.Summary)
	if summary == "" {
		return ""
	}
	return truncateRunes(summary, maxTitleLength)
}

// toolDescription is what the document says about the operation.
//
// The method and path used to be prefixed onto every description. They are
// addressing details the caller cannot act on, and they pushed the actual text
// behind a line the model has to read past on every tool. They remain the
// fallback for an operation the document says nothing about, where knowing that
// much is better than knowing nothing.
func toolDescription(route *requester.RouteConfig, operation *openapi3.Operation) string {
	if described := strings.TrimSpace(route.Description); described != "" {
		return described
	}
	if operation != nil {
		if summary := strings.TrimSpace(operation.Summary); summary != "" {
			return summary
		}
	}
	return fmt.Sprintf("%s %s", route.Method, route.Path)
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// methodAnnotations states only what the HTTP method actually establishes.
//
// A nil return means "no annotations", which is deliberate for POST and PATCH:
// plenty of these APIs serve reads over POST, and because the SDK marshals
// ReadOnlyHint as a bare bool, any non-nil annotation on a POST would publish
// readOnlyHint=false — a claim that the operation writes, which the spec does
// not support. A consumer that gates confirmation on the hint would then either
// wave writes through or demand confirmation for every read.
//
// The methods below do carry guarantees, and they come from HTTP itself rather
// than from the document:
//
//   - GET reads, does not destroy, and is idempotent.
//   - PUT and DELETE are idempotent; DELETE destroys.
func methodAnnotations(method string) *mcp.ToolAnnotations {
	no, yes := false, true
	switch method {
	case http.MethodGet:
		return &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: &no,
			IdempotentHint:  true,
		}
	case http.MethodDelete:
		return &mcp.ToolAnnotations{
			DestructiveHint: &yes,
			IdempotentHint:  true,
		}
	case http.MethodPut:
		return &mcp.ToolAnnotations{IdempotentHint: true}
	default:
		return nil
	}
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

// addParameters exposes path, query, header and cookie parameters with the
// type, enum, default and example the spec declared for them.
func (p *SwaggerParser) addParameters(schema *inputSchemaBuilder, route *requester.RouteConfig, operation *openapi3.Operation) {
	params := p.operationParameters(route, operation)
	argNames := assignArgNames(params, hasRequestBody(operation))

	for _, param := range params {
		if param.Value == nil || param.Value.Name == "" {
			continue
		}
		value := param.Value

		paramSchema := jsonSchemaFor(value.Schema)
		if value.Description != "" {
			paramSchema["description"] = value.Description
		}
		if example := parameterExample(value); example != nil {
			paramSchema["example"] = example
		}
		// Path parameters are always required regardless of what the spec says:
		// the URL cannot be built without them.
		required := value.Required || value.In == string(requester.ParamInPath)

		argName := argNames[paramKey{name: value.Name, in: value.In}]
		if argName != value.Name {
			paramSchema["description"] = collisionNote(paramSchema["description"], value.Name, value.In)
		}
		schema.add(argName, paramSchema, required)
	}

	// Path placeholders that the spec forgot to declare still have to be
	// fillable, otherwise the URL keeps its braces and the call 404s.
	for _, name := range extractPathParams(route.Path) {
		if schema.has(name) {
			continue
		}
		schema.add(name, map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Path parameter: %s (not declared in the spec)", name),
		}, true)
	}
}

// paramKey identifies a parameter. Location is part of the identity because the
// same name may appear in more than one location.
type paramKey struct {
	name string
	in   string
}

// assignArgNames maps each parameter onto a unique tool argument name.
//
// Tool arguments share one flat namespace, while parameters are only unique per
// location, and the request body occupies the name "body". Two parameters that
// wanted the same argument name used to overwrite each other in that namespace,
// so one of them vanished from the tool with no indication — as did any
// parameter actually named "body". The colliding one is renamed to
// "<location>_<name>" instead; the wire name is unaffected.
//
// Assignment follows the parameter order, which is path-item declarations before
// operation ones, so it is stable for a given document.
func assignArgNames(params openapi3.Parameters, bodyTaken bool) map[paramKey]string {
	taken := map[string]bool{}
	if bodyTaken {
		taken[bodyArgName] = true
	}
	out := make(map[paramKey]string, len(params))

	for _, param := range params {
		if param == nil || param.Value == nil || param.Value.Name == "" {
			continue
		}
		name, in := param.Value.Name, param.Value.In
		candidate := name
		if taken[candidate] {
			candidate = in + "_" + name
		}
		for suffix := 2; taken[candidate]; suffix++ {
			candidate = fmt.Sprintf("%s_%s_%d", in, name, suffix)
		}
		taken[candidate] = true
		out[paramKey{name: name, in: in}] = candidate
	}
	return out
}

// collisionNote records the upstream name a renamed argument maps to, so the
// rename is visible to whoever reads the tool rather than only in the config.
func collisionNote(existing any, name, in string) string {
	note := fmt.Sprintf("sent as the %s parameter %q", in, name)
	if text, ok := existing.(string); ok && text != "" {
		return text + "\n" + note
	}
	return note
}

func hasRequestBody(operation *openapi3.Operation) bool {
	return operation != nil && operation.RequestBody != nil &&
		operation.RequestBody.Value != nil && len(operation.RequestBody.Value.Content) > 0
}

// operationParameters merges path-level parameters into the operation's own.
//
// The spec allows declaring shared parameters once per path item and overriding
// them per operation, and identity is the (name, location) pair rather than the
// name alone — the same name may legitimately appear as both a query and a
// header parameter. Concatenating the two lists instead of merging them left the
// argument recorded twice, which put a duplicate in the tool's required set and
// made the builder send a duplicated query parameter twice on the wire.
func (p *SwaggerParser) operationParameters(route *requester.RouteConfig, operation *openapi3.Operation) openapi3.Parameters {
	type key struct {
		name string
		in   string
	}
	var out openapi3.Parameters
	indexes := map[key]int{}

	add := func(params openapi3.Parameters) {
		for _, param := range params {
			if param == nil || param.Value == nil || param.Value.Name == "" {
				continue
			}
			id := key{name: param.Value.Name, in: param.Value.In}
			if index, seen := indexes[id]; seen {
				// A later declaration is the more specific one.
				out[index] = param
				continue
			}
			indexes[id] = len(out)
			out = append(out, param)
		}
	}

	if p.doc != nil && p.doc.Paths != nil {
		if pathItem := p.doc.Paths.Find(route.Path); pathItem != nil {
			add(pathItem.Parameters)
		}
	}
	if operation != nil {
		add(operation.Parameters)
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

// bodyParameter returns the request body schema and whether it is required, and
// records the media type the body has to be encoded as.
func (p *SwaggerParser) bodyParameter(route *requester.RouteConfig, operation *openapi3.Operation) (map[string]any, bool) {
	if operation == nil {
		logger.Debug("No operation found",
			zap.String("path", route.Path),
			zap.String("method", route.Method))
		return nil, false
	}
	schema, mediaType, required := selectBodySchema(operation)
	if schema == nil {
		return nil, false
	}
	route.MethodConfig.BodyContentType = mediaType
	return bodySchema(schema), required
}

// responseSchema exposes the success response schema as the tool's
// outputSchema, so a caller can see what it can read back instead of having to
// call the tool to find out.
//
// MCP requires the top level of an outputSchema to be an object. A response
// that is an array or a scalar is therefore skipped rather than misreported:
// declaring the wrong shape is worse than declaring none, because a client that
// validates would reject every successful call.
func responseSchema(operation *openapi3.Operation) map[string]any {
	if operation == nil || operation.Responses == nil {
		return nil
	}
	for _, code := range []string{"200", "201", "default"} {
		response := operation.Responses.Value(code)
		if response == nil || response.Value == nil {
			continue
		}
		for _, mediaType := range sortedContentTypes(response.Value.Content) {
			schema := response.Value.Content[mediaType].Schema
			if schema == nil || schema.Value == nil {
				continue
			}
			converted := jsonSchemaFor(schema)
			if converted["type"] != "object" {
				continue
			}
			return converted
		}
	}
	return nil
}

// sortedContentTypes keeps the choice of response media type deterministic;
// Go map iteration order would otherwise leak into the published schema.
func sortedContentTypes(content openapi3.Content) []string {
	out := make([]string, 0, len(content))
	for name, mediaType := range content {
		if mediaType != nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// bodyMediaTypePreference orders the media types this server can actually
// encode. Anything outside the list is still accepted, but only after these.
var bodyMediaTypePreference = []string{
	"application/json",
	"application/x-www-form-urlencoded",
	"multipart/form-data",
}

// selectBodySchema picks one request body media type and returns its schema.
//
// The choice is deterministic — preferred types first, then the remaining ones
// in name order — because Go map iteration would otherwise make the generated
// tool depend on which media type happened to come out first.
func selectBodySchema(operation *openapi3.Operation) (*openapi3.SchemaRef, string, bool) {
	if operation.RequestBody == nil || operation.RequestBody.Value == nil {
		return nil, "", false
	}
	body := operation.RequestBody.Value
	if len(body.Content) == 0 {
		return nil, "", false
	}

	for _, name := range bodyMediaTypePreference {
		if mediaType, ok := body.Content[name]; ok && mediaType != nil {
			return mediaType.Schema, name, body.Required
		}
	}
	// A JSON-flavoured type such as application/merge-patch+json is encoded the
	// same way as application/json.
	for _, name := range sortedContentTypes(body.Content) {
		if strings.HasSuffix(name, "+json") {
			return body.Content[name].Schema, name, body.Required
		}
	}
	for _, name := range sortedContentTypes(body.Content) {
		return body.Content[name].Schema, name, body.Required
	}
	return nil, "", false
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
	// Content-Type is left unset here. It is added only when a request body is
	// actually produced, and then from the encoding performed rather than from
	// the declaration, so the header never describes bytes that were not sent.
	routeConfig := &requester.RouteConfig{
		Path:    path,
		Method:  method,
		Headers: map[string]string{},
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

	specParams := p.operationParameters(routeConfig, operation)
	argNames := assignArgNames(specParams, hasRequestBody(operation))
	for _, param := range specParams {
		if param.Value == nil || param.Value.Name == "" {
			continue
		}
		value := param.Value
		cfg := requester.ParamConfig{
			Name:    value.Name,
			ArgName: argNames[paramKey{name: value.Name, in: value.In}],
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

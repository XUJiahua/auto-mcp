package requester

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	urlpkg "net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/logger"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

// HTTPRequestBuilderParams holds the parameters for creating an HTTPRequestBuilder
type HTTPRequestBuilderParams struct {
	fx.In
	EndpointConfig *config.EndpointConfig
	AuthManager    AuthManager
	RouteConfig    *RouteConfig
}

// HTTPRequestBuilder implements the RequestBuilder interface
type HTTPRequestBuilder struct {
	serviceCfg  *config.EndpointConfig
	authMgr     AuthManager
	routeConfig *RouteConfig
}

// NewHTTPRequestBuilder creates a new HTTPRequestBuilder
func NewHTTPRequestBuilder(params HTTPRequestBuilderParams) *HTTPRequestBuilder {
	return &HTTPRequestBuilder{
		serviceCfg:  params.EndpointConfig,
		authMgr:     params.AuthManager,
		routeConfig: params.RouteConfig,
	}
}

// BuildRequest builds a request from a route name and parameters.
//
// Every argument is placed according to the location the spec declared for it
// (path, query, header, cookie or body). Arguments the spec never declared fall
// back to the query string, which is what this builder used to do for all of
// them.
func (b *HTTPRequestBuilder) BuildRequest(ctx context.Context, params map[string]interface{}) (*Request, error) {
	if b.routeConfig == nil {
		return nil, fmt.Errorf("route config is nil")
	}

	byLocation := b.paramsByLocation()

	// Build URL, consuming the path parameters.
	url, consumed, err := b.buildURL(b.routeConfig.Path, params, byLocation)
	if err != nil {
		return nil, err
	}
	url = b.addQueryParams(url, params, byLocation, consumed)

	// Create request body
	body, contentType, err := b.createRequestBody(b.routeConfig, params)
	if err != nil {
		return nil, fmt.Errorf("failed to create request body: %w", err)
	}

	// Merge headers
	headers := make(map[string]string)
	for k, v := range b.serviceCfg.Headers {
		headers[k] = v
	}
	for k, v := range b.routeConfig.Headers {
		headers[k] = v
	}
	// Declared header parameters come from the caller's arguments and take
	// precedence over the static configuration for the same name.
	for _, cfg := range byLocation[ParamInHeader] {
		value, present := params[cfg.Arg()]
		if !present || value == nil {
			continue
		}
		items, ok := serializeParam(value, true)
		if !ok {
			logger.Warn("Header parameter value cannot be serialised; skipping",
				zap.String("param", cfg.Name))
			continue
		}
		headers[cfg.Name] = strings.Join(items, ",")
	}

	// Create the HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, b.routeConfig.Method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Add headers
	for key, value := range headers {
		httpReq.Header.Set(key, value)
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	for _, cfg := range byLocation[ParamInCookie] {
		value, present := params[cfg.Arg()]
		if !present || value == nil {
			continue
		}
		items, ok := serializeParam(value, true)
		if !ok {
			logger.Warn("Cookie parameter value cannot be serialised; skipping",
				zap.String("param", cfg.Name))
			continue
		}
		httpReq.AddCookie(&http.Cookie{Name: cfg.Name, Value: strings.Join(items, ",")})
	}

	// Apply authentication
	if err := b.authMgr.ApplyAuth(httpReq); err != nil {
		return nil, fmt.Errorf("failed to apply authentication: %w", err)
	}

	return &Request{
		URL:         url,
		Method:      b.routeConfig.Method,
		Body:        body,
		Headers:     headers,
		ContentType: contentType,
		HttpRequest: httpReq,
	}, nil
}

// paramsByLocation groups the declared parameters so each stage can pick up its
// own without inspecting names.
func (b *HTTPRequestBuilder) paramsByLocation() map[ParamLocation][]ParamConfig {
	out := map[ParamLocation][]ParamConfig{}
	for _, cfg := range b.routeConfig.MethodConfig.Params {
		location := cfg.In
		if location == "" {
			location = ParamInQuery
		}
		out[location] = append(out[location], cfg)
	}
	return out
}

// buildURL substitutes path placeholders and reports which arguments it used, so
// they are not repeated in the query string.
func (b *HTTPRequestBuilder) buildURL(path string, params map[string]interface{},
	byLocation map[ParamLocation][]ParamConfig) (string, map[string]bool, error) {

	consumed := map[string]bool{}
	url := b.serviceCfg.BaseURL + path

	substitute := func(upstreamName, argName string) {
		value, ok := params[argName]
		if !ok || value == nil {
			return
		}
		placeholder := fmt.Sprintf("{%s}", upstreamName)
		if !strings.Contains(url, placeholder) {
			return
		}
		url = strings.ReplaceAll(url, placeholder, urlpkg.PathEscape(fmt.Sprintf("%v", value)))
		consumed[argName] = true
	}

	for _, cfg := range byLocation[ParamInPath] {
		substitute(cfg.Name, cfg.Arg())
	}
	// Placeholders the spec never declared still have to be filled, otherwise
	// the braces reach the upstream verbatim.
	for name := range params {
		substitute(name, name)
	}

	// A placeholder that survived was not supplied. Sending it would percent-encode
	// the braces into the path, and the upstream would answer with a routing error
	// about a URL nobody meant to request. The value is what makes the URL
	// addressable, so its absence is reported here where the name is known.
	if missing := pathPlaceholders(url); len(missing) > 0 {
		return "", nil, fmt.Errorf("missing required path parameter(s): %s",
			strings.Join(missing, ", "))
	}
	return url, consumed, nil
}

// pathPlaceholderPattern matches an unsubstituted {name} in a path.
var pathPlaceholderPattern = regexp.MustCompile(`\{([^{}/]+)\}`)

func pathPlaceholders(url string) []string {
	matches := pathPlaceholderPattern.FindAllStringSubmatch(url, -1)
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match[1])
	}
	return names
}

// addQueryParams appends query parameters for every method, not just GET: a
// POST that takes both a body and a query parameter is common, and dropping the
// query parameter makes the call fail in a way that looks like an upstream bug.
func (b *HTTPRequestBuilder) addQueryParams(baseURL string, params map[string]interface{},
	byLocation map[ParamLocation][]ParamConfig, consumed map[string]bool) string {

	u, err := urlpkg.Parse(baseURL)
	if err != nil {
		return baseURL
	}

	// Keyed by argument name: this decides which of the caller's arguments have
	// already been placed somewhere, not which upstream names exist.
	declared := map[string]bool{}
	for _, cfgs := range byLocation {
		for _, cfg := range cfgs {
			declared[cfg.Arg()] = true
		}
	}

	q := u.Query()
	add := func(name string, value any, explode bool) {
		if value == nil {
			return
		}
		items, ok := serializeParam(value, explode)
		if !ok {
			// An object has no single correct query serialisation. Encoding one
			// anyway looks like the value was sent and fails somewhere far from
			// here, so it is dropped and reported instead.
			logger.Warn("Query parameter value cannot be serialised; skipping",
				zap.String("param", name))
			return
		}
		for _, item := range items {
			q.Add(name, item)
		}
	}

	for _, cfg := range byLocation[ParamInQuery] {
		if value, ok := params[cfg.Arg()]; ok {
			add(cfg.Name, value, cfg.Explode)
		}
	}
	// Undeclared arguments keep the previous behaviour of becoming query
	// parameters; body and file are structural and never belong there.
	for name, value := range params {
		if declared[name] || consumed[name] || name == "body" || name == "file" {
			continue
		}
		add(name, value, true)
	}

	u.RawQuery = q.Encode()
	return u.String()
}

// serializeParam renders one argument as query/header values, reporting whether
// the value has a serialisation at all.
//
// An array becomes repeated values when exploded and one comma-joined value
// otherwise; formatting the slice with %v would emit Go syntax ("[4 5]"). An
// object is reported as unserialisable rather than being turned into a JSON blob:
// OpenAPI's object serialisation styles are not implemented here, and a guessed
// encoding is indistinguishable from a correct one until the upstream rejects it.
func serializeParam(value any, explode bool) ([]string, bool) {
	if containsObject(value) {
		return nil, false
	}
	items := flattenParamValue(value)
	if len(items) == 0 {
		return nil, true
	}
	if explode || len(items) == 1 {
		return items, true
	}
	return []string{strings.Join(items, ",")}, true
}

// containsObject reports whether a value is, or contains, a JSON object.
func containsObject(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		return true
	case []any:
		for _, item := range v {
			if containsObject(item) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func flattenParamValue(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, flattenParamValue(item)...)
		}
		return out
	case []string:
		return v
	case string:
		return []string{v}
	case float64:
		return []string{strconv.FormatFloat(v, 'f', -1, 64)}
	case bool:
		return []string{strconv.FormatBool(v)}
	default:
		rv := reflect.ValueOf(value)
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			out := make([]string, 0, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				out = append(out, flattenParamValue(rv.Index(i).Interface())...)
			}
			return out
		}
		return []string{fmt.Sprintf("%v", value)}
	}
}

func (b *HTTPRequestBuilder) createRequestBody(routeConfig *RouteConfig, params map[string]interface{}) (io.Reader, string, error) {
	switch routeConfig.Method {
	case "GET":
		return nil, "", nil

	case "POST", "PUT", "PATCH":
		// Handle multipart/form-data
		if routeConfig.MethodConfig.FileUpload != nil {
			return b.createMultipartBody(routeConfig, params)
		}

		body, ok := params["body"]
		if !ok {
			return nil, "", nil
		}
		return encodeBody(body, routeConfig.MethodConfig.BodyContentType)

	default:
		// For other methods, just send the params as JSON if not nil
		if params != nil {
			jsonData, err := json.Marshal(params)
			if err != nil {
				return nil, "", fmt.Errorf("failed to marshal request body: %w", err)
			}
			return bytes.NewBuffer(jsonData), "application/json", nil
		}
		return nil, "", nil
	}
}

// encodeBody serialises the body argument for the media type the spec declared.
//
// The returned content type describes the bytes actually produced, never the
// declaration. Claiming a media type we did not produce is the failure this
// replaces: every request body used to go out as JSON under a hardcoded
// Content-Type: application/json, so a form-encoded endpoint received JSON while
// being told it was form data, and the upstream rejected it for reasons that
// pointed nowhere near the cause.
func encodeBody(body any, mediaType string) (io.Reader, string, error) {
	switch {
	case mediaType == "application/x-www-form-urlencoded":
		fields, ok := body.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("a %s body must be an object, got %T", mediaType, body)
		}
		values := urlpkg.Values{}
		for _, name := range sortedKeys(fields) {
			for _, item := range flattenParamValue(fields[name]) {
				values.Add(name, item)
			}
		}
		return strings.NewReader(values.Encode()), mediaType, nil

	case isTextualMediaType(mediaType):
		// A textual body given as a string is sent as written; wrapping it in JSON
		// quotes would change the bytes the upstream reads.
		if text, ok := body.(string); ok {
			return strings.NewReader(text), mediaType, nil
		}
	}

	jsonData, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal request body: %w", err)
	}
	if mediaType != "" && !isJSONMediaType(mediaType) {
		// We cannot produce this media type. JSON is sent and declared as JSON so
		// that the bytes and the header agree; the disagreement with the spec is
		// reported here rather than left for the upstream to discover.
		logger.Warn("Request body media type is not supported; sending JSON",
			zap.String("declared", mediaType))
		return bytes.NewBuffer(jsonData), "application/json", nil
	}
	if mediaType == "" {
		mediaType = "application/json"
	}
	return bytes.NewBuffer(jsonData), mediaType, nil
}

func isJSONMediaType(mediaType string) bool {
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func isTextualMediaType(mediaType string) bool {
	return strings.HasPrefix(mediaType, "text/")
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (b *HTTPRequestBuilder) createMultipartBody(routeConfig *RouteConfig, params map[string]interface{}) (io.Reader, string, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add file if present
	if file, ok := params[routeConfig.MethodConfig.FileUpload.FieldName].(multipart.File); ok {
		part, err := writer.CreateFormFile(routeConfig.MethodConfig.FileUpload.FieldName, "file")
		if err != nil {
			return nil, "", fmt.Errorf("failed to create form file: %w", err)
		}
		if _, err := io.Copy(part, file); err != nil {
			return nil, "", fmt.Errorf("failed to copy file: %w", err)
		}
	}

	// Add other form fields
	for _, field := range routeConfig.MethodConfig.FormFields {
		if value, exists := params[field]; exists {
			if err := writer.WriteField(field, fmt.Sprintf("%v", value)); err != nil {
				return nil, "", fmt.Errorf("failed to write form field: %w", err)
			}
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("failed to close multipart writer: %w", err)
	}

	return body, writer.FormDataContentType(), nil
}

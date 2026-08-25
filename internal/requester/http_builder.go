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
	// Signer computes per-request credentials. Optional.
	Signer Signer `optional:"true"`
	// URLBuilder and BodyBuilder let the host rewrite what gets sent. Optional.
	URLBuilder  URLBuilder  `optional:"true"`
	BodyBuilder BodyBuilder `optional:"true"`
}

// HTTPRequestBuilder implements the RequestBuilder interface
type HTTPRequestBuilder struct {
	serviceCfg  *config.EndpointConfig
	authMgr     AuthManager
	routeConfig *RouteConfig
	// signer computes per-request credentials, nil when the upstream needs none.
	signer Signer
	// urlBuilder and bodyBuilder are host hooks, nil when the document is enough.
	urlBuilder  URLBuilder
	bodyBuilder BodyBuilder
}

// NewHTTPRequestBuilder creates a new HTTPRequestBuilder
func NewHTTPRequestBuilder(params HTTPRequestBuilderParams) *HTTPRequestBuilder {
	return &HTTPRequestBuilder{
		serviceCfg:  params.EndpointConfig,
		authMgr:     params.AuthManager,
		routeConfig: params.RouteConfig,
		signer:      params.Signer,
		urlBuilder:  params.URLBuilder,
		bodyBuilder: params.BodyBuilder,
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

	if err := b.refuseUndeclared(params, byLocation); err != nil {
		return nil, err
	}

	// Build URL, consuming the path parameters.
	// The consumed set is no longer needed to keep path parameters out of the
	// query string: undeclared arguments are refused above, and declared ones are
	// placed only where their location says.
	url, _, err := b.buildURL(b.routeConfig.Path, params, byLocation)
	if err != nil {
		return nil, err
	}
	url = b.addQueryParams(url, params, byLocation)

	// Create request body. The bytes are kept, not just a reader: a signature is
	// computed over exactly these bytes, and a reader is consumed by the first read.
	bodyBytes, contentType, err := b.createRequestBody(b.routeConfig, params)
	if err != nil {
		return nil, fmt.Errorf("failed to create request body: %w", err)
	}

	// Host hooks get to rewrite the URL and the body.
	//
	// Both run before signing, so a signature covers exactly what will be sent.
	// The body hook runs after the default one so that it sees the default output:
	// most upstreams want that wrapped or renamed, not built from nothing.
	hookReq := BuildRequest{
		Method: b.routeConfig.Method, Path: b.routeConfig.Path, URL: url,
		Args: params, Body: bodyBytes, ContentType: contentType,
	}
	if b.urlBuilder != nil {
		rewritten, err := b.urlBuilder.BuildURL(ctx, hookReq)
		if err != nil {
			return nil, fmt.Errorf("url builder: %w", err)
		}
		url = rewritten
		hookReq.URL = rewritten
	}
	if b.bodyBuilder != nil {
		rewritten, ct, err := b.bodyBuilder.BuildBody(ctx, hookReq)
		if err != nil {
			return nil, fmt.Errorf("body builder: %w", err)
		}
		bodyBytes = rewritten
		if ct != "" {
			contentType = ct
		}
	}
	var body io.Reader
	if len(bodyBytes) > 0 {
		body = bytes.NewReader(bodyBytes)
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

	// Sign last, over the request as it will actually be sent. Anything added
	// after this point would not be covered by the signature, and the mismatch
	// would only show up as a rejection from the upstream.
	if b.signer != nil {
		signed, err := b.signer.SignRequest(ctx, SigningRequest{
			Method:      httpReq.Method,
			Path:        httpReq.URL.Path,
			Query:       httpReq.URL.RawQuery,
			Body:        bodyBytes,
			ContentType: contentType,
			Tool:        b.routeConfig.MethodConfig.ToolName,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to sign request: %w", err)
		}
		for key, value := range signed {
			httpReq.Header.Set(key, value)
			headers[key] = value
		}
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

// refuseUndeclared rejects arguments the document never declared.
//
// They used to be appended to the query string. That turned a caller's mistake
// into a request that quietly went somewhere else: a model inventing a parameter
// name got a 200 from an upstream that ignored it, or a rejection whose reason
// pointed nowhere near the invented name. Refusing here is the only place that
// still knows both what was asked for and what the document allows.
//
// The message lists the accepted names because the caller is usually a model:
// it can correct itself from that list, and cannot from "unknown argument".
func (b *HTTPRequestBuilder) refuseUndeclared(params map[string]interface{},
	byLocation map[ParamLocation][]ParamConfig) error {

	accepted := map[string]bool{}
	for _, cfgs := range byLocation {
		for _, cfg := range cfgs {
			accepted[cfg.Arg()] = true
		}
	}
	// The body is an argument only where the operation actually reads one.
	// Accepting it everywhere would move the silent drop rather than remove it:
	// a body handed to a GET was discarded without a word.
	if b.acceptsBody() {
		accepted["body"] = true
	}
	if upload := b.routeConfig.MethodConfig.FileUpload; upload != nil {
		accepted[upload.FieldName] = true
		for _, field := range b.routeConfig.MethodConfig.FormFields {
			accepted[field] = true
		}
	}

	var unknown []string
	for name := range params {
		if !accepted[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)

	allowed := make([]string, 0, len(accepted))
	for name := range accepted {
		allowed = append(allowed, name)
	}
	sort.Strings(allowed)
	if len(allowed) == 0 {
		return fmt.Errorf("%s %s accepts no arguments, but %s were given",
			b.routeConfig.Method, b.routeConfig.Path, strings.Join(unknown, ", "))
	}
	return fmt.Errorf("%s %s does not declare %s; accepted arguments are %s",
		b.routeConfig.Method, b.routeConfig.Path,
		strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

// acceptsBody reports whether this operation reads a "body" argument.
//
// The authority is the document, not the method name: OpenAPI permits a request
// body on DELETE, and forbids nothing on POST. BodyContentType is set exactly
// when the spec declared one, so it answers the question directly.
func (b *HTTPRequestBuilder) acceptsBody() bool {
	return b.routeConfig.MethodConfig.FileUpload == nil &&
		b.routeConfig.MethodConfig.BodyContentType != ""
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
	byLocation map[ParamLocation][]ParamConfig) string {

	u, err := urlpkg.Parse(baseURL)
	if err != nil {
		return baseURL
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

// createRequestBody produces the request body the document declared, if any.
//
// A body goes out only where the spec declares one. The method name is not the
// judge: OpenAPI permits a request body on DELETE and requires none on POST.
//
// This replaces a fallback that marshalled *every* argument as JSON for any
// method outside GET/POST/PUT/PATCH. On DELETE /pet/{petId} that shipped
// {"petId":10} as a JSON body — a path parameter already substituted into the
// URL, resent as a body the document never mentioned, under a Content-Type
// claiming JSON. Upstreams that reject unexpected DELETE bodies failed for a
// reason pointing nowhere near the cause, and intermediaries are free to drop
// such a body entirely.
func (b *HTTPRequestBuilder) createRequestBody(routeConfig *RouteConfig, params map[string]interface{}) ([]byte, string, error) {
	if routeConfig.MethodConfig.FileUpload != nil {
		return b.createMultipartBody(routeConfig, params)
	}
	if routeConfig.MethodConfig.BodyContentType == "" {
		return nil, "", nil
	}
	body, ok := params["body"]
	if !ok {
		return nil, "", nil
	}
	return encodeBody(body, routeConfig.MethodConfig.BodyContentType)
}

// encodeBody serialises the body argument for the media type the spec declared.
//
// The returned content type describes the bytes actually produced, never the
// declaration. Claiming a media type we did not produce is the failure this
// replaces: every request body used to go out as JSON under a hardcoded
// Content-Type: application/json, so a form-encoded endpoint received JSON while
// being told it was form data, and the upstream rejected it for reasons that
// pointed nowhere near the cause.
func encodeBody(body any, mediaType string) ([]byte, string, error) {
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
		return []byte(values.Encode()), mediaType, nil

	case isTextualMediaType(mediaType):
		// A textual body given as a string is sent as written; wrapping it in JSON
		// quotes would change the bytes the upstream reads.
		if text, ok := body.(string); ok {
			return []byte(text), mediaType, nil
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
		return jsonData, "application/json", nil
	}
	if mediaType == "" {
		mediaType = "application/json"
	}
	return jsonData, mediaType, nil
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

func (b *HTTPRequestBuilder) createMultipartBody(routeConfig *RouteConfig, params map[string]interface{}) ([]byte, string, error) {
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

	return body.Bytes(), writer.FormDataContentType(), nil
}

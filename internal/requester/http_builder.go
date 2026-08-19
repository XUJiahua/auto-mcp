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
	"strconv"
	"strings"

	"github.com/brizzai/auto-mcp/internal/config"

	"go.uber.org/fx"
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
	url, consumed := b.buildURL(b.routeConfig.Path, params, byLocation)
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
		if value, ok := params[cfg.Name]; ok && value != nil {
			headers[cfg.Name] = strings.Join(serializeParam(value, true), ",")
		}
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
		if value, ok := params[cfg.Name]; ok && value != nil {
			httpReq.AddCookie(&http.Cookie{
				Name:  cfg.Name,
				Value: strings.Join(serializeParam(value, true), ","),
			})
		}
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
	byLocation map[ParamLocation][]ParamConfig) (string, map[string]bool) {

	consumed := map[string]bool{}
	url := b.serviceCfg.BaseURL + path

	substitute := func(name string) {
		value, ok := params[name]
		if !ok || value == nil {
			return
		}
		placeholder := fmt.Sprintf("{%s}", name)
		if !strings.Contains(url, placeholder) {
			return
		}
		url = strings.ReplaceAll(url, placeholder, urlpkg.PathEscape(fmt.Sprintf("%v", value)))
		consumed[name] = true
	}

	for _, cfg := range byLocation[ParamInPath] {
		substitute(cfg.Name)
	}
	// Placeholders the spec never declared still have to be filled, otherwise
	// the braces reach the upstream verbatim.
	for name := range params {
		substitute(name)
	}
	return url, consumed
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

	declared := map[string]bool{}
	for _, cfgs := range byLocation {
		for _, cfg := range cfgs {
			declared[cfg.Name] = true
		}
	}

	q := u.Query()
	add := func(name string, value any, explode bool) {
		if value == nil {
			return
		}
		for _, item := range serializeParam(value, explode) {
			q.Add(name, item)
		}
	}

	for _, cfg := range byLocation[ParamInQuery] {
		if value, ok := params[cfg.Name]; ok {
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

// serializeParam renders one argument as query/header values.
//
// An array becomes repeated values when exploded and one comma-joined value
// otherwise; formatting the slice with %v would emit Go syntax ("[4 5]").
func serializeParam(value any, explode bool) []string {
	items := flattenParamValue(value)
	if len(items) == 0 {
		return nil
	}
	if explode || len(items) == 1 {
		return items
	}
	return []string{strings.Join(items, ",")}
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
	case map[string]any:
		// An object in a query position has no single correct serialisation;
		// JSON is at least reversible and visible in logs.
		encoded, err := json.Marshal(v)
		if err != nil {
			return []string{fmt.Sprintf("%v", v)}
		}
		return []string{string(encoded)}
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

		// Handle regular JSON body
		if body, ok := params["body"]; ok {
			jsonData, err := json.Marshal(body)
			if err != nil {
				return nil, "", fmt.Errorf("failed to marshal request body: %w", err)
			}
			return bytes.NewBuffer(jsonData), "application/json", nil
		}
		return nil, "", nil

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

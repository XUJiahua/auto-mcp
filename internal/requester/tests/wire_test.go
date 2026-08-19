package tests

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/requester"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func noopAuth() requester.AuthManager {
	return &mockAuthManager{applyAuthFunc: func(*http.Request) error { return nil }}
}

func buildWith(t *testing.T, route *requester.RouteConfig, params map[string]any) *requester.Request {
	t.Helper()
	builder := requester.NewHTTPRequestBuilder(requester.HTTPRequestBuilderParams{
		EndpointConfig: &config.EndpointConfig{BaseURL: "http://api.example.com"},
		AuthManager:    noopAuth(),
		RouteConfig:    route,
	})
	req, err := builder.BuildRequest(context.Background(), params)
	require.NoError(t, err)
	return req
}

// A path parameter belongs in the URL path only. Appending it to the query as
// well sends the caller's order id twice, and some upstreams reject the extra.
func TestWire_PathParamDoesNotLeakIntoQuery(t *testing.T) {
	req := buildWith(t, &requester.RouteConfig{
		Method: http.MethodGet,
		Path:   "/api/order/{orderId}",
		MethodConfig: requester.MethodConfig{Params: []requester.ParamConfig{
			{Name: "orderId", In: requester.ParamInPath, Type: "string"},
		}},
	}, map[string]any{"orderId": "SO2026"})

	assert.Equal(t, "/api/order/SO2026", req.HttpRequest.URL.Path)
	assert.Empty(t, req.HttpRequest.URL.Query().Get("orderId"), "path parameter must not also be a query parameter")
}

// An array query parameter has to become repeated keys. Formatting the Go slice
// yields `?starRates=[4 5]`, which no upstream parses.
func TestWire_ArrayQueryParamRepeatsKeys(t *testing.T) {
	req := buildWith(t, &requester.RouteConfig{
		Method: http.MethodGet,
		Path:   "/api/search",
		MethodConfig: requester.MethodConfig{Params: []requester.ParamConfig{
			{Name: "starRates", In: requester.ParamInQuery, Type: "array", Explode: true},
		}},
	}, map[string]any{"starRates": []any{4, 5}})

	assert.Equal(t, []string{"4", "5"}, req.HttpRequest.URL.Query()["starRates"])
}

// With explode=false the same array is comma-joined, per OpenAPI's form style.
func TestWire_NonExplodedArrayIsCommaJoined(t *testing.T) {
	req := buildWith(t, &requester.RouteConfig{
		Method: http.MethodGet,
		Path:   "/api/search",
		MethodConfig: requester.MethodConfig{Params: []requester.ParamConfig{
			{Name: "tags", In: requester.ParamInQuery, Type: "array", Explode: false},
		}},
	}, map[string]any{"tags": []any{"a", "b"}})

	assert.Equal(t, []string{"a,b"}, req.HttpRequest.URL.Query()["tags"])
}

// Header parameters were parsed out of the spec and then never sent.
func TestWire_HeaderParamIsSent(t *testing.T) {
	req := buildWith(t, &requester.RouteConfig{
		Method: http.MethodGet,
		Path:   "/api/search",
		MethodConfig: requester.MethodConfig{Params: []requester.ParamConfig{
			{Name: "X-Trace-Id", In: requester.ParamInHeader, Type: "string"},
		}},
	}, map[string]any{"X-Trace-Id": "T-1"})

	assert.Equal(t, "T-1", req.HttpRequest.Header.Get("X-Trace-Id"))
	assert.Empty(t, req.HttpRequest.URL.Query().Get("X-Trace-Id"), "a header must not also be sent as a query parameter")
}

// Query parameters used to be added for GET only, so a POST that takes both a
// body and a query parameter silently dropped the query parameter.
func TestWire_PostSendsBodyAndQueryParams(t *testing.T) {
	req := buildWith(t, &requester.RouteConfig{
		Method: http.MethodPost,
		Path:   "/api/createOrder",
		MethodConfig: requester.MethodConfig{Params: []requester.ParamConfig{
			{Name: "channel", In: requester.ParamInQuery, Type: "string", Explode: true},
		}},
	}, map[string]any{
		"channel": "app",
		"body":    map[string]any{"header": map[string]any{"sign": "S"}},
	})

	assert.Equal(t, "app", req.HttpRequest.URL.Query().Get("channel"))
	assert.NotNil(t, req.Body, "body must still be sent")
	assert.Equal(t, "application/json", req.ContentType)
	assert.Empty(t, req.HttpRequest.URL.Query().Get("body"), "the body must not leak into the query string")
}

// The two-part JSON body must reach the wire byte-for-byte: the caller's
// credential engine writes the signature into body.header, and re-shaping the
// body here would invalidate it.
func TestWire_NestedBodyIsPreservedVerbatim(t *testing.T) {
	body := map[string]any{
		"header":          map[string]any{"partnerCode": "P1", "sign": "SIG"},
		"businessRequest": map[string]any{"hotelId": "H1"},
	}
	req := buildWith(t, &requester.RouteConfig{
		Method:       http.MethodPost,
		Path:         "/api/queryHotelInfo",
		MethodConfig: requester.MethodConfig{},
	}, map[string]any{"body": body})

	require.NotNil(t, req.HttpRequest.Body)
	assert.JSONEq(t,
		`{"header":{"partnerCode":"P1","sign":"SIG"},"businessRequest":{"hotelId":"H1"}}`,
		readAllBody(t, req))
}

func readAllBody(t *testing.T, req *requester.Request) string {
	t.Helper()
	data, err := io.ReadAll(req.HttpRequest.Body)
	require.NoError(t, err)
	return string(data)
}

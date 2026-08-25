package tests

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/requester"
)

// builderFor assembles a builder for one route.
func builderFor(rc *requester.RouteConfig) *requester.HTTPRequestBuilder {
	return requester.NewHTTPRequestBuilder(requester.HTTPRequestBuilderParams{
		RouteConfig:    rc,
		EndpointConfig: &config.EndpointConfig{BaseURL: "http://api.example.com"},
		AuthManager:    &mockAuthManager{applyAuthFunc: func(*http.Request) error { return nil }},
	})
}

func bodyOf(t *testing.T, req *requester.Request) string {
	t.Helper()
	if req.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

// An argument the document never declared used to be appended to the query
// string. That turned a caller's mistake into a request that quietly went
// somewhere else: a model inventing a parameter name got a 200 from an upstream
// that ignored it, or a rejection whose reason pointed nowhere near the invented
// name. The caller is usually a model, so the message lists what is accepted —
// it can correct itself from that, and cannot from "unknown argument".
func TestBuildRequestRefusesUndeclaredArgument(t *testing.T) {
	b := builderFor(&requester.RouteConfig{
		Method: "GET", Path: "/pet/findByStatus",
		MethodConfig: requester.MethodConfig{Params: []requester.ParamConfig{
			{Name: "status", In: requester.ParamInQuery, Explode: true}}},
	})

	_, err := b.BuildRequest(context.Background(), map[string]interface{}{
		"status": "available", "statuz": "typo"})
	if err == nil {
		t.Fatal("an undeclared argument must be refused, not silently forwarded")
	}
	if !strings.Contains(err.Error(), "statuz") {
		t.Errorf("error does not name the offending argument: %v", err)
	}
	if !strings.Contains(err.Error(), "status") {
		t.Errorf("error does not list the accepted arguments: %v", err)
	}
}

// An operation that declares nothing accepts nothing, and says so plainly.
func TestBuildRequestRefusesArgumentsWhenNoneAreDeclared(t *testing.T) {
	b := builderFor(&requester.RouteConfig{Method: "GET", Path: "/pets"})

	_, err := b.BuildRequest(context.Background(), map[string]interface{}{"anything": 1})
	if err == nil {
		t.Fatal("an operation with no parameters must refuse arguments")
	}
	if !strings.Contains(err.Error(), "accepts no arguments") {
		t.Errorf("message should say the operation takes nothing: %v", err)
	}
	if _, err := b.BuildRequest(context.Background(), nil); err != nil {
		t.Fatalf("no arguments at all must be fine: %v", err)
	}
}

// A body handed to an operation that declares none is refused. Ignoring it would
// move the silent drop rather than remove it.
func TestBuildRequestRefusesBodyWhenNoneIsDeclared(t *testing.T) {
	b := builderFor(&requester.RouteConfig{
		Method: "POST", Path: "/pet/{petId}/adopt",
		MethodConfig: requester.MethodConfig{Params: []requester.ParamConfig{
			{Name: "petId", In: requester.ParamInPath}}},
	})

	_, err := b.BuildRequest(context.Background(), map[string]interface{}{
		"petId": 10, "body": map[string]any{"name": "dog"}})
	if err == nil {
		t.Fatal("a body must be refused where the operation declares none")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("error does not mention the body: %v", err)
	}
}

// A declared body still goes out, encoded as the spec asked.
func TestBuildRequestSendsDeclaredBody(t *testing.T) {
	b := builderFor(&requester.RouteConfig{
		Method: "POST", Path: "/pet",
		MethodConfig: requester.MethodConfig{BodyContentType: "application/json"},
	})

	req, err := b.BuildRequest(context.Background(), map[string]interface{}{
		"body": map[string]any{"name": "dog"}})
	if err != nil {
		t.Fatalf("a declared body must be accepted: %v", err)
	}
	if got := bodyOf(t, req); got != `{"name":"dog"}` {
		t.Errorf("body = %q", got)
	}
	if req.ContentType != "application/json" {
		t.Errorf("content type = %q", req.ContentType)
	}
}

// DELETE /pet/{petId} used to ship {"petId":10} as a JSON body: a path parameter
// already substituted into the URL, resent as a body the document never
// mentioned, under a Content-Type claiming JSON. Upstreams that reject an
// unexpected DELETE body failed for a reason pointing nowhere near the cause,
// and intermediaries may drop such a body entirely.
func TestBuildRequestSendsNoBodyWhereNoneIsDeclared(t *testing.T) {
	b := builderFor(&requester.RouteConfig{
		Method: "DELETE", Path: "/pet/{petId}",
		MethodConfig: requester.MethodConfig{Params: []requester.ParamConfig{
			{Name: "petId", In: requester.ParamInPath}}},
	})

	req, err := b.BuildRequest(context.Background(), map[string]interface{}{"petId": 10})
	if err != nil {
		t.Fatal(err)
	}
	if req.URL != "http://api.example.com/pet/10" {
		t.Errorf("url = %q", req.URL)
	}
	if got := bodyOf(t, req); got != "" {
		t.Errorf("body = %q, a path parameter must not be resent as a body", got)
	}
	if req.ContentType != "" {
		t.Errorf("content type = %q, no body means no Content-Type", req.ContentType)
	}
}

// A DELETE that really declares a body keeps working: the document is the judge,
// not the method name.
func TestBuildRequestSendsDeclaredBodyOnDelete(t *testing.T) {
	b := builderFor(&requester.RouteConfig{
		Method: "DELETE", Path: "/pets",
		MethodConfig: requester.MethodConfig{BodyContentType: "application/json"},
	})

	req, err := b.BuildRequest(context.Background(), map[string]interface{}{
		"body": map[string]any{"ids": []int{1, 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := bodyOf(t, req); !strings.Contains(got, `"ids"`) {
		t.Errorf("body = %q", got)
	}
}

// Multipart uploads declare their file field and form fields; those are accepted
// names too, and anything else is not.
func TestBuildRequestMultipartAcceptsDeclaredFieldsOnly(t *testing.T) {
	rc := &requester.RouteConfig{
		Method: "POST", Path: "/pet/{petId}/uploadImage",
		MethodConfig: requester.MethodConfig{
			Params:     []requester.ParamConfig{{Name: "petId", In: requester.ParamInPath}},
			FileUpload: &requester.FileUploadConfig{FieldName: "file"},
			FormFields: []string{"additionalMetadata"},
		},
	}

	if _, err := builderFor(rc).BuildRequest(context.Background(),
		map[string]interface{}{"petId": 10, "additionalMetadata": "x"}); err != nil {
		t.Fatalf("declared form fields must be accepted: %v", err)
	}
	if _, err := builderFor(rc).BuildRequest(context.Background(),
		map[string]interface{}{"petId": 10, "notAField": "x"}); err == nil {
		t.Fatal("an undeclared form field must be refused")
	}
}

package automcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/brizzai/auto-mcp/automcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const signingSpec = `{
  "openapi": "3.0.1",
  "info": {"title": "Flights", "version": "1.0"},
  "paths": {
    "/api/v1/{lang}/flight/search": {
      "post": {
        "operationId": "searchFlight",
        "parameters": [
          {"name": "lang", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "channel", "in": "query", "schema": {"type": "string"}}
        ],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {
          "type": "object", "properties": {"origin": {"type": "string"}}}}}},
        "responses": {"200": {"description": "OK"}}}}}}`

// serve builds a service with a signer and returns a live client session plus the
// requests the upstream actually received.
func serve(t *testing.T, sign automcp.Signer) (*mcp.ClientSession, *[]recorded, func()) {
	t.Helper()
	var mu sync.Mutex
	got := &[]recorded{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		*got = append(*got, recorded{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Body: string(raw), Headers: r.Header.Clone(),
		})
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(signingSpec), BaseURL: upstream.URL, Signer: sign})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "flights", Version: "1"}, nil)
	svc.Register(srv)

	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	return session, got, func() { _ = session.Close(); upstream.Close() }
}

type recorded struct {
	Method, Path, Query, Body string
	Headers                   http.Header
}

func call(t *testing.T, s *mcp.ClientSession, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(),
		&mcp.CallToolParams{Name: "searchFlight", Arguments: args})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return res
}

// A signer sees the request it is signing and its headers reach the wire.
//
// This is the shape enterprise gateways use: HMAC over a canonical request
// (method, path, sorted query, body hash, timestamp, nonce, key). None of it can
// be prepared ahead of time, because it depends on the very bytes being sent.
func TestSignerSeesTheRequestAndItsHeadersAreSent(t *testing.T) {
	var seen automcp.SigningRequest
	session, got, done := serve(t, automcp.SignerFunc(
		func(_ context.Context, req automcp.SigningRequest) (map[string]string, error) {
			seen = req
			sum := sha256.Sum256(req.Body)
			return map[string]string{
				"X-OP-Sign":   hex.EncodeToString(sum[:]),
				"X-OP-Method": req.Method,
			}, nil
		}))
	defer done()

	call(t, session, map[string]any{
		"lang": "en", "channel": "app", "body": map[string]any{"origin": "SIN"}})

	if len(*got) != 1 {
		t.Fatalf("上游收到 %d 个请求", len(*got))
	}
	r := (*got)[0]

	if seen.Method != "POST" {
		t.Errorf("签名器看到的方法 = %q", seen.Method)
	}
	// 路径要是已经替换过占位符的那一个：签的是 /api/v1/en/... 而不是 /{lang}/...，
	// 否则签名与实际请求对不上，而上游只会回一句 SIGN_ERROR。
	if seen.Path != "/api/v1/en/flight/search" {
		t.Errorf("签名器看到的路径 = %q，应当是替换过占位符的实际路径", seen.Path)
	}
	if seen.Query != "channel=app" {
		t.Errorf("签名器看到的查询串 = %q", seen.Query)
	}
	if r.Headers.Get("X-OP-Sign") == "" || r.Headers.Get("X-OP-Method") != "POST" {
		t.Errorf("签名头没有到线上: %v", r.Headers)
	}
}

// 签名器看到的 body 必须与线上那份逐字节相同。
//
// 这是这类集成最典型的失败：签了一份重新序列化过的 body（键序变了、空格变了），
// 发出去的是另一份，于是每次调用都被上游判 SIGN_ERROR，而错误信息指不出哪里不一致。
func TestSignerBodyIsByteIdenticalToTheWire(t *testing.T) {
	var signed []byte
	session, got, done := serve(t, automcp.SignerFunc(
		func(_ context.Context, req automcp.SigningRequest) (map[string]string, error) {
			signed = append([]byte(nil), req.Body...)
			return nil, nil
		}))
	defer done()

	call(t, session, map[string]any{
		"lang": "en", "body": map[string]any{"origin": "SIN", "alpha": 1, "zulu": true}})

	if len(*got) != 1 {
		t.Fatalf("上游收到 %d 个请求", len(*got))
	}
	if string(signed) != (*got)[0].Body {
		t.Errorf("签的与发的不是同一份字节：\n签 = %s\n发 = %s", signed, (*got)[0].Body)
	}
	if !json.Valid(signed) {
		t.Errorf("签名器看到的 body 不是合法 JSON: %s", signed)
	}
}

// 每次请求都要重新签一次：nonce 与时间戳的意义就在于不复用。
func TestSignerRunsPerRequest(t *testing.T) {
	var n int
	session, got, done := serve(t, automcp.SignerFunc(
		func(_ context.Context, _ automcp.SigningRequest) (map[string]string, error) {
			n++
			return map[string]string{"X-OP-Nonce": fmt.Sprintf("nonce-%d", n)}, nil
		}))
	defer done()

	for i := 0; i < 3; i++ {
		call(t, session, map[string]any{"lang": "en", "body": map[string]any{"origin": "SIN"}})
	}
	if n != 3 {
		t.Fatalf("签名器被调用 %d 次，期望 3", n)
	}
	for i, r := range *got {
		want := fmt.Sprintf("nonce-%d", i+1)
		if r.Headers.Get("X-OP-Nonce") != want {
			t.Errorf("第 %d 个请求的 nonce = %q，期望 %q", i+1, r.Headers.Get("X-OP-Nonce"), want)
		}
	}
}

// 签不出来就不发。发一个未签名的请求只会换来上游一句 SIGN_ERROR，
// 而真正的原因（凭证取不到、时钟不可用）就此丢失。
func TestSignerFailureStopsTheRequest(t *testing.T) {
	session, got, done := serve(t, automcp.SignerFunc(
		func(_ context.Context, _ automcp.SigningRequest) (map[string]string, error) {
			return nil, fmt.Errorf("凭证取不到")
		}))
	defer done()

	res, err := session.CallTool(context.Background(),
		&mcp.CallToolParams{Name: "searchFlight", Arguments: map[string]any{"lang": "en"}})
	if err == nil && !res.IsError {
		t.Fatal("签名失败时不该把请求发出去")
	}
	if len(*got) != 0 {
		t.Fatalf("签名失败却发了 %d 个请求", len(*got))
	}
	text := ""
	if res != nil && len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	if err != nil {
		text = err.Error()
	}
	if !strings.Contains(text, "凭证取不到") {
		t.Errorf("错误没有带上真正的原因: %q", text)
	}
}

// 没有签名器时行为不变。
func TestWithoutSignerNothingChanges(t *testing.T) {
	session, got, done := serve(t, nil)
	defer done()

	call(t, session, map[string]any{"lang": "en", "body": map[string]any{"origin": "SIN"}})
	if len(*got) != 1 {
		t.Fatalf("上游收到 %d 个请求", len(*got))
	}
	if (*got)[0].Path != "/api/v1/en/flight/search" {
		t.Errorf("path = %q", (*got)[0].Path)
	}
}

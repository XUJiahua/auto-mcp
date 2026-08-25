package automcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/brizzai/auto-mcp/automcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const buildingSpec = `{
  "openapi": "3.0.1",
  "info": {"title": "Dict", "version": "1.0"},
  "paths": {
    "/api/{lang}/nationality/list": {
      "post": {
        "operationId": "listNationality",
        "parameters": [
          {"name": "lang", "in": "path", "required": true,
           "schema": {"type": "string"}}
        ],
        "requestBody": {"content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"keyword": {"type": "string"}}}}}},
        "responses": {"200": {"description": "OK"}}
      }
    }
  }
}`

// recorder 记下上游真正收到的东西。
type recorder struct {
	mu     sync.Mutex
	path   string
	query  string
	body   string
	header http.Header
}

func (r *recorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body := make([]byte, req.ContentLength)
		if req.ContentLength > 0 {
			_, _ = req.Body.Read(body)
		}
		r.mu.Lock()
		r.path, r.query, r.body, r.header = req.URL.Path, req.URL.RawQuery,
			string(body), req.Header.Clone()
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}
}

// URLBuilder 能改写这次请求打到哪个地址。
//
// 存在的理由是真实的：事信机票的路径规则由不到文档里 —— 字典走 /api/{lang}/…，
// 航班走 /api/v2/{lang}/…，而 fields 是字面量 /api/lang/fields。此前只能靠校订文件
// 一条条硬写，那是把商户的适配逻辑塞进一个不该承载它的地方。
func TestURLBuilderRewritesThePath(t *testing.T) {
	rec := &recorder{}
	upstream := httptest.NewServer(rec.handler())
	defer upstream.Close()

	var sawMethod, sawPath, sawDefault string
	svc := build(t, automcp.Options{
		Spec:    strings.NewReader(buildingSpec),
		BaseURL: upstream.URL,
		URLBuilder: automcp.URLBuilderFunc(func(_ context.Context,
			req automcp.BuildRequest) (string, error) {
			sawMethod, sawPath, sawDefault = req.Method, req.Path, req.URL
			// 把 v2 前缀加进去，模拟"文档里没写的路径规则"。
			return strings.Replace(req.URL, "/api/", "/api/v2/", 1), nil
		}),
	})
	callTool(t, svc, "listNationality", map[string]any{"lang": "zh_CN"})

	if rec.path != "/api/v2/zh_CN/nationality/list" {
		t.Errorf("上游收到的路径 = %q，期望改写后的 /api/v2/…", rec.path)
	}
	// 钩子要看得见它在改什么：方法、文档声明的路径模板、以及平台算出的默认 URL。
	if sawMethod != "POST" {
		t.Errorf("钩子看到的方法 = %q", sawMethod)
	}
	if sawPath != "/api/{lang}/nationality/list" {
		t.Errorf("钩子看到的路径模板 = %q（应当是文档里的原样）", sawPath)
	}
	if !strings.Contains(sawDefault, "/api/zh_CN/nationality/list") {
		t.Errorf("钩子看到的默认 URL = %q", sawDefault)
	}
}

// BodyBuilder 能改写请求体，而且**签名签的是改写后的字节**。
//
// 顺序错了的症状是每次调用都被上游拒，而错误信息指不出哪里不一致 —— 所以这一条
// 必须钉住，不能只测"体改了"。
func TestBodyBuilderRunsBeforeSigning(t *testing.T) {
	rec := &recorder{}
	upstream := httptest.NewServer(rec.handler())
	defer upstream.Close()

	var signedBody string
	svc := build(t, automcp.Options{
		Spec:    strings.NewReader(buildingSpec),
		BaseURL: upstream.URL,
		BodyBuilder: automcp.BodyBuilderFunc(func(_ context.Context,
			req automcp.BuildRequest) ([]byte, string, error) {
			// 上游要求把业务参数裹一层，而文档没这么写。
			wrapped, err := json.Marshal(map[string]any{"payload": req.Args})
			return wrapped, "application/json", err
		}),
		Signer: automcp.SignerFunc(func(_ context.Context,
			req automcp.SigningRequest) (map[string]string, error) {
			signedBody = string(req.Body)
			return map[string]string{"X-Sign": "ok"}, nil
		}),
	})
	callTool(t, svc, "listNationality",
		map[string]any{"lang": "zh_CN", "body": map[string]any{"keyword": "中"}})

	if !strings.Contains(rec.body, `"payload"`) {
		t.Errorf("上游收到的请求体 = %q，期望被裹了一层", rec.body)
	}
	// 签名看到的必须是改写后的字节，而不是改写前的。
	if signedBody != rec.body {
		t.Errorf("签名签的不是发出去的字节\n  签的   %q\n  发出去 %q", signedBody, rec.body)
	}
	if rec.header.Get("X-Sign") != "ok" {
		t.Error("签名头没加上")
	}
}

// 钩子报错就停下来，不发一个半成品请求。
func TestBuilderErrorStopsTheRequest(t *testing.T) {
	rec := &recorder{}
	upstream := httptest.NewServer(rec.handler())
	defer upstream.Close()

	svc := build(t, automcp.Options{
		Spec:    strings.NewReader(buildingSpec),
		BaseURL: upstream.URL,
		URLBuilder: automcp.URLBuilderFunc(func(_ context.Context,
			_ automcp.BuildRequest) (string, error) {
			return "", errBoom
		}),
	})
	if _, err := callRaw(svc, "listNationality", map[string]any{"lang": "zh_CN"}); err == nil {
		t.Fatal("钩子报错却仍然发了请求")
	}
	if rec.path != "" {
		t.Errorf("上游被打到了：%q", rec.path)
	}
}

// 没给钩子时行为与从前完全一致 —— 这两个扩展点是可选的。
func TestWithoutBuildersBehaviourIsUnchanged(t *testing.T) {
	rec := &recorder{}
	upstream := httptest.NewServer(rec.handler())
	defer upstream.Close()

	svc := build(t, automcp.Options{
		Spec: strings.NewReader(buildingSpec), BaseURL: upstream.URL,
	})
	callTool(t, svc, "listNationality",
		map[string]any{"lang": "en", "body": map[string]any{"keyword": "x"}})

	if rec.path != "/api/en/nationality/list" {
		t.Errorf("路径 = %q", rec.path)
	}
	if !strings.Contains(rec.body, `"keyword"`) {
		t.Errorf("请求体 = %q", rec.body)
	}
}

var errBoom = errors.New("钩子刻意报错")

// build 起一个服务并连上一个内存里的客户端。
func build(t *testing.T, opts automcp.Options) *mcp.ClientSession {
	t.Helper()
	svc, err := automcp.Build(opts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "dict", Version: "1"}, nil)
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
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callTool(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any) {
	t.Helper()
	res, err := callRaw(s, tool, args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("工具报错: %+v", res.Content)
	}
}

// callRaw 不断言成败，供"钩子报错应当停下来"那条用例用。
func callRaw(s *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	res, err := s.CallTool(context.Background(),
		&mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return res, errBoom
	}
	return res, nil
}

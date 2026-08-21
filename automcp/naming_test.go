package automcp_test

import (
	"strings"
	"testing"

	"github.com/brizzai/auto-mcp/automcp"
)

// 没有 operationId 时，用 summary 派生名字，而不是用路径。
//
// 路径派生出的是 post_api_v2_lang_flight_order_cancel —— 又长，又把 HTTP 方法和版本号
// 这些与业务无关的东西塞进了工具名。而工具名是模型看到的东西，也是 capability_id 的
// 一段，客户端会一直拿着它。
//
// 更要紧的是读写判定看的就是名字：从 summary 派生的 cancelStandardOrder 会直接命中
// "cancel"，而路径派生的那个要靠 path 里恰好有 cancel 这一段才蒙对。
//
// 真实文档里 summary 很常见（本次那份 22 个操作全都有），而 operationId 常常缺。
func TestToolNameFromSummaryWhenOperationIDIsAbsent(t *testing.T) {
	const spec = `{
      "openapi": "3.1.0", "info": {"title": "T", "version": "1"},
      "paths": {
        "/api/v2/{lang}/flight/order/cancel": {"post": {
          "summary": "Cancel Standard Order",
          "parameters": [{"name": "lang", "in": "path", "required": true,
            "schema": {"type": "string"}}],
          "responses": {"200": {"description": "OK"}}}},
        "/api/{lang}/nationality/list": {"post": {
          "summary": "Query Nationality List",
          "parameters": [{"name": "lang", "in": "path", "required": true,
            "schema": {"type": "string"}}],
          "responses": {"200": {"description": "OK"}}}}}}`

	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(spec), BaseURL: "https://x.test"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tool := range svc.Tools() {
		got[tool.Path] = tool.Tool.Name
	}
	for path, want := range map[string]string{
		"/api/v2/{lang}/flight/order/cancel": "cancelStandardOrder",
		"/api/{lang}/nationality/list":       "queryNationalityList",
	} {
		if got[path] != want {
			t.Errorf("%s 的工具名 = %q，期望 %q", path, got[path], want)
		}
	}
}

// operationId 存在时仍以它为准：那是文档作者明确指定的名字。
func TestOperationIDStillWins(t *testing.T) {
	const spec = `{
      "openapi": "3.1.0", "info": {"title": "T", "version": "1"},
      "paths": {"/x": {"post": {
        "operationId": "theChosenName", "summary": "Some Summary",
        "responses": {"200": {"description": "OK"}}}}}}`

	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(spec), BaseURL: "https://x.test"})
	if err != nil {
		t.Fatal(err)
	}
	if name := svc.Tools()[0].Tool.Name; name != "theChosenName" {
		t.Errorf("工具名 = %q，operationId 应当优先", name)
	}
}

// summary 也没有时才退回路径 —— 那仍然好过没有名字。
func TestFallsBackToThePathWithoutSummary(t *testing.T) {
	const spec = `{
      "openapi": "3.1.0", "info": {"title": "T", "version": "1"},
      "paths": {"/api/v1/thing": {"post": {
        "responses": {"200": {"description": "OK"}}}}}}`

	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(spec), BaseURL: "https://x.test"})
	if err != nil {
		t.Fatal(err)
	}
	if name := svc.Tools()[0].Tool.Name; !strings.Contains(name, "thing") {
		t.Errorf("工具名 = %q，没有 summary 时该退回路径", name)
	}
}

// summary 重复时要去重，否则两个操作抢同一个名字。
func TestDuplicateSummariesGetDistinctNames(t *testing.T) {
	const spec = `{
      "openapi": "3.1.0", "info": {"title": "T", "version": "1"},
      "paths": {
        "/a/fields": {"post": {"summary": "Query Fields",
          "responses": {"200": {"description": "OK"}}}},
        "/b/fields": {"post": {"summary": "Query Fields",
          "responses": {"200": {"description": "OK"}}}}}}`

	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(spec), BaseURL: "https://x.test"})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range svc.Tools() {
		if names[tool.Tool.Name] {
			t.Fatalf("两个操作用了同一个名字 %q", tool.Tool.Name)
		}
		names[tool.Tool.Name] = true
	}
	if len(names) != 2 {
		t.Fatalf("名字数 = %d", len(names))
	}
}

// 中文 summary 不能产出非法或空的名字。
func TestNonASCIISummaryStillYieldsAUsableName(t *testing.T) {
	const spec = `{
      "openapi": "3.1.0", "info": {"title": "T", "version": "1"},
      "paths": {"/api/lang/fields": {"post": {"summary": "查询翻译信息接口",
        "responses": {"200": {"description": "OK"}}}}}}`

	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(spec), BaseURL: "https://x.test"})
	if err != nil {
		t.Fatal(err)
	}
	name := svc.Tools()[0].Tool.Name
	if name == "" {
		t.Fatal("名字为空")
	}
	for _, r := range name {
		if r > 127 {
			t.Errorf("名字含非 ASCII 字符：%q", name)
			break
		}
	}
	if !strings.Contains(name, "fields") {
		t.Errorf("中文 summary 派生不出名字时该退回路径，得到 %q", name)
	}
}

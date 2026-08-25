package automcp_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/brizzai/auto-mcp/automcp"
)

// badExamples 声明 type: object 却给了数组示例，且 enum 与示例矛盾。
// 真实文档里这类问题成片出现（本次那份有 54 处），而它们只影响示例值。
const badExamples = `{
  "openapi": "3.0.3",
  "info": {"title": "T", "version": "1"},
  "paths": {"/x": {"post": {
    "operationId": "doX",
    "requestBody": {"content": {"application/json": {"schema": {
      "type": "object",
      "properties": {
        "n": {"type": "string", "enum": ["a","b"], "example": 7},
        "d": {"type": "object", "example": [1,2,3]}
      }}}}},
    "responses": {"200": {"description": "OK"}}}}}}`

// nullType 用 3.1 的 type: "null"，却声明成 3.0.3。
const nullType = `{
  "openapi": "3.0.3",
  "info": {"title": "T", "version": "1"},
  "paths": {"/x": {"post": {
    "operationId": "doX",
    "requestBody": {"content": {"application/json": {"schema": {
      "type": "object",
      "properties": {"stop": {"type": "null"}}}}}},
    "responses": {"200": {"description": "OK"}}}}}}`

// 默认严格：不合规就拒。这条是保守侧的全部意义 —— 不显式开启就不改任何东西。
func TestStrictByDefault(t *testing.T) {
	for name, spec := range map[string]string{
		"示例与声明不符":          badExamples,
		"3.1 的 type: null": nullType,
	} {
		if _, err := automcp.Build(automcp.Options{
			Spec: strings.NewReader(spec), BaseURL: "https://x.test"}); err == nil {
			t.Errorf("%s：默认应当被拒", name)
		}
	}
}

// 被拒时要告诉运营有这个开关，否则每个真实文档都得先撞一次死路。
func TestRefusalPointsAtTheLenientOption(t *testing.T) {
	_, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(badExamples), BaseURL: "https://x.test"})
	if err == nil {
		t.Fatal("应当被拒")
	}
	if !strings.Contains(err.Error(), "lenient") && !strings.Contains(err.Error(), "宽松") {
		t.Errorf("错误没有指出可以开启宽松模式：%v", err)
	}
}

// 开启之后：示例的问题被忽略，但必须逐条报出来。
func TestLenientIgnoresExamplesAndReportsIt(t *testing.T) {
	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(badExamples), BaseURL: "https://x.test", Lenient: true})
	if err != nil {
		t.Fatalf("开启宽松后应当通过: %v", err)
	}
	if len(svc.Tools()) != 1 {
		t.Fatalf("工具数 = %d", len(svc.Tools()))
	}
	notices := svc.Notices()
	if len(notices) == 0 {
		t.Fatal("忽略了问题却什么都没报：那就是静默降级")
	}
	joined := strings.Join(notices, " | ")
	if !strings.Contains(joined, "example") {
		t.Errorf("报告没提到 example：%s", joined)
	}
}

// 版本声明按实际用法纠正，并说清是哪一处触发的。
//
// 一份用着 type: "null" 的文档实际上就是 3.1，openapi: 3.0.3 那一行才是错的。
// 按实际用法读它得到的信息更多；但这是"在两个矛盾的声明里选一个信"，所以必须说出来。
func TestLenientRedeclaresTheVersionAndSaysWhy(t *testing.T) {
	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(nullType), BaseURL: "https://x.test", Lenient: true})
	if err != nil {
		t.Fatalf("开启宽松后应当通过: %v", err)
	}
	joined := strings.Join(svc.Notices(), " | ")
	if !strings.Contains(joined, "3.1") {
		t.Errorf("报告没说清版本被按 3.1 读：%s", joined)
	}
	if !strings.Contains(joined, "null") {
		t.Errorf("报告没指出是哪一处触发的：%s", joined)
	}
}

// 合规的文档开启宽松也不该产生报告：没有改动就没有可报的。
func TestLenientIsSilentOnAConformantDocument(t *testing.T) {
	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(twoOperationSpec), BaseURL: "https://x.test", Lenient: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(svc.Notices()); n != 0 {
		t.Errorf("合规文档却报了 %d 条：%v", n, svc.Notices())
	}
}

// 宽松不等于什么都收：结构性问题仍然拒。
//
// 把 type 写成 types 会让那个字段的类型凭空消失，而生成出来的工具看起来完全正常 ——
// 错的 example 会被上游拒，错的类型不会，它一路通到线上。
func TestLenientStillRefusesStructuralProblems(t *testing.T) {
	const structural = `{
      "openapi": "3.0.3", "info": {"title": "T", "version": "1"},
      "paths": {"/x": {"get": {}}}}`
	if _, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(structural), BaseURL: "https://x.test", Lenient: true}); err == nil {
		t.Error("3.0 里缺 responses 是结构性问题，宽松模式也该拒")
	}
}

// 组件名用了中文（或其它非 ASCII）时，宽松模式改名而不是拒。
//
// OpenAPI 规定 components 下的键只能是 [a-zA-Z0-9._-]。而用中文命名 schema 在国内的
// 文档工具里很常见（apifox 导出就是这样），本次那份 21 个操作的文档里 7 个 schema
// 全是中文名。
//
// 名字只是 $ref 的锚点：它不进工具契约，也不出现在任何请求或响应里。改名只要把引用
// 一起改，语义分毫不动 —— 与"忽略示例值"同一类，都是字面不合规而实质无害。
const chineseComponentNames = `{
  "openapi": "3.1.0",
  "info": {"title": "T", "version": "1"},
  "paths": {"/x": {"post": {
    "operationId": "doX",
    "requestBody": {"required": true, "content": {"application/json": {"schema": {
      "type": "object",
      "properties": {"停靠": {"$ref": "#/components/schemas/经停信息"}}}}}},
    "responses": {"200": {"description": "OK"}}}}},
  "components": {"schemas": {"经停信息": {
    "type": "object",
    "properties": {"city": {"type": "string", "description": "经停城市"}}}}}}`

func TestStrictRefusesNonASCIIComponentNames(t *testing.T) {
	if _, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(chineseComponentNames), BaseURL: "https://x.test"}); err == nil {
		t.Error("默认严格：不合规的组件名应当被拒")
	}
}

func TestLenientRenamesNonASCIIComponents(t *testing.T) {
	svc, err := automcp.Build(automcp.Options{
		Spec: strings.NewReader(chineseComponentNames), BaseURL: "https://x.test", Lenient: true})
	if err != nil {
		t.Fatalf("开启宽松后应当通过: %v", err)
	}
	if len(svc.Tools()) != 1 {
		t.Fatalf("工具数 = %d", len(svc.Tools()))
	}

	// 改名不能丢内容：引用必须跟着改，被引用的字段要照样出现在工具契约里。
	schema := svc.Tools()[0].Tool.InputSchema
	encoded := fmt.Sprint(schema)
	if !strings.Contains(encoded, "经停城市") {
		t.Errorf("改名丢掉了被引用的内容：%.400s", encoded)
	}

	joined := strings.Join(svc.Notices(), " | ")
	if !strings.Contains(joined, "经停信息") {
		t.Errorf("报告没说清改了哪个名字：%s", joined)
	}
}

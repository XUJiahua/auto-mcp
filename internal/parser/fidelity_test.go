package parser

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// merchantSpecJSON is shaped after a real Chinese travel merchant API:
//
//   - every business call is POST with a two-part JSON body: a shared `header`
//     (credentials/signature, injected by the caller's credential engine) and a
//     per-operation `businessRequest`;
//   - the shared part is composed in via `allOf` + `$ref`, which is how specs
//     normally avoid repeating it;
//   - names carry the semantics (`queryX` reads, `createX` writes), the HTTP
//     method does not: reads are POST too;
//   - GET operations use path, query (including arrays) and header parameters.
//
// A downstream consumer flattens this inputSchema into a flag list and drives
// real calls from it, so anything dropped here becomes a call it cannot make.
const merchantSpecJSON = `{
  "openapi": "3.0.1",
  "info": { "title": "Merchant Hotel API", "version": "1.0" },
  "servers": [{ "url": "https://merchant.example.com" }],
  "paths": {
    "/api/queryHotelInfo": { "post": {
      "tags": ["hotel"], "operationId": "queryHotelInfo", "summary": "Query hotel detail",
      "requestBody": { "required": true, "content": { "application/json": {
        "schema": { "$ref": "#/components/schemas/HotelInfoRequest" } } } },
      "responses": { "200": { "description": "OK" } } } },
    "/api/createOrder": { "post": {
      "tags": ["order"], "operationId": "createOrder", "summary": "Create order",
      "requestBody": { "required": true, "content": { "application/json": { "schema": {
        "allOf": [
          { "$ref": "#/components/schemas/BaseRequest" },
          { "type": "object", "required": ["businessRequest"],
            "properties": { "businessRequest": { "$ref": "#/components/schemas/OrderBody" } } }
        ] } } } },
      "responses": { "200": { "description": "OK" } } } },
    "/api/payOrder": { "post": {
      "tags": ["order"], "operationId": "payOrder", "summary": "Pay order",
      "requestBody": { "content": { "application/json": { "schema": {
        "type": "object",
        "properties": { "payment": { "oneOf": [
          { "$ref": "#/components/schemas/CardPayment" },
          { "$ref": "#/components/schemas/WalletPayment" } ] } } } } } },
      "responses": { "200": { "description": "OK" } } } },
    "/api/order/{orderId}": { "get": {
      "tags": ["order"], "operationId": "getOrderDetail", "summary": "Query order detail",
      "parameters": [
        { "name": "orderId", "in": "path", "required": true,
          "schema": { "type": "string" }, "example": "SO2026" },
        { "name": "starRates", "in": "query",
          "schema": { "type": "array", "items": { "type": "integer" } } },
        { "name": "locale", "in": "query",
          "schema": { "type": "string", "enum": ["zh_CN", "en_US"], "default": "zh_CN" } },
        { "name": "X-Trace-Id", "in": "header", "schema": { "type": "string" } }
      ],
      "responses": { "200": { "description": "OK" } } } }
  },
  "components": { "schemas": {
    "BaseRequest": { "type": "object", "required": ["header"],
      "properties": { "header": { "$ref": "#/components/schemas/Header" } } },
    "Header": { "type": "object", "required": ["partnerCode", "sign"], "properties": {
      "partnerCode": { "type": "string", "description": "Partner code" },
      "sign": { "type": "string", "description": "Request signature" } } },
    "HotelInfoRequest": { "type": "object", "required": ["header", "businessRequest"], "properties": {
      "header": { "$ref": "#/components/schemas/Header" },
      "businessRequest": { "type": "object", "required": ["hotelId"], "properties": {
        "hotelId": { "type": "string", "description": "Hotel id", "example": "H12345" },
        "language": { "type": "string", "description": "Language", "default": "zh_CN" } } } } },
    "OrderBody": { "type": "object", "required": ["productToken", "guests"], "properties": {
      "productToken": { "type": "string", "example": "PT-abc" },
      "guests": { "type": "array", "items": { "type": "object", "required": ["name"], "properties": {
        "name": { "type": "string", "description": "Guest name" },
        "idNo": { "type": "string", "description": "ID number", "example": "3101" } } } } } },
    "CardPayment": { "type": "object", "required": ["cardNo"], "properties": {
      "cardNo": { "type": "string" }, "cvv": { "type": "string" } } },
    "WalletPayment": { "type": "object", "required": ["walletId"], "properties": {
      "walletId": { "type": "string" } } }
  } }
}`

// merchantSpecYAML is the same document in YAML, the format merchant onboarding
// actually produces.
const merchantSpecYAML = `
openapi: 3.0.1
info:
  title: Merchant Hotel API
  version: "1.0"
servers:
  - url: https://merchant.example.com
paths:
  /api/queryHotelInfo:
    post:
      operationId: queryHotelInfo
      summary: Query hotel detail
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [businessRequest]
              properties:
                businessRequest:
                  type: object
                  required: [hotelId]
                  properties:
                    hotelId: { type: string, example: "H12345" }
      responses:
        "200": { description: OK }
`

func parseSpec(t *testing.T, spec string) *SwaggerParser {
	t.Helper()
	p := NewSwaggerParser(NewAdjuster())
	require.NoError(t, p.ParseReader(strings.NewReader(spec)))
	return p
}

func toolsByName(t *testing.T, p *SwaggerParser) map[string]*RouteTool {
	t.Helper()
	out := map[string]*RouteTool{}
	for _, rt := range p.GetRouteTools() {
		out[rt.Tool.Name] = rt
	}
	return out
}

// dig walks a JSON-schema-shaped map, failing the test with the path it got
// stuck on. A nil return with a passing test is impossible by construction.
func dig(t *testing.T, node any, path ...string) map[string]any {
	t.Helper()
	cur, ok := node.(map[string]any)
	require.True(t, ok, "root is not an object")
	for i, key := range path {
		next, exists := cur[key]
		require.True(t, exists, "missing key %q at path %v", key, path[:i+1])
		cur, ok = next.(map[string]any)
		require.True(t, ok, "key %q at path %v is not an object: %#v", key, path[:i+1], next)
	}
	return cur
}

func prop(t *testing.T, tool mcp.Tool, name string) map[string]any {
	t.Helper()
	raw, ok := tool.InputSchema.Properties[name]
	require.True(t, ok, "tool %s has no property %q (has %v)", tool.Name, name, keysOf(tool.InputSchema.Properties))
	m, ok := raw.(map[string]any)
	require.True(t, ok, "property %q is not an object", name)
	return m
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func requiredList(t *testing.T, schema map[string]any) []string {
	t.Helper()
	raw, ok := schema["required"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			require.True(t, ok, "required entry is not a string: %#v", item)
			out = append(out, s)
		}
		return out
	default:
		t.Fatalf("required is neither []string nor []any: %#v", raw)
		return nil
	}
}

// Tool names must come from operationId. The name is the only place the
// read/write semantics of these APIs survives: every business call is POST, so
// a `post_api_...` name tells a consumer nothing and a conservative consumer
// has to treat every read as a write.
func TestFidelity_ToolNamesUseOperationID(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	for _, want := range []string{"queryHotelInfo", "createOrder", "payOrder", "getOrderDetail"} {
		assert.Contains(t, tools, want)
	}
}

// A body composed with allOf + $ref must be merged, not partially dropped.
func TestFidelity_AllOfRefIsMerged(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	rt := tools["createOrder"]
	require.NotNil(t, rt, "createOrder tool missing")

	body := prop(t, rt.Tool, "body")
	assert.ElementsMatch(t, []string{"header", "businessRequest"}, requiredList(t, body),
		"both allOf members must contribute their required lists")

	// The shared header branch survives with its own fields.
	header := dig(t, body, "properties", "header", "properties")
	assert.Contains(t, header, "partnerCode")
	assert.Contains(t, header, "sign")

	// The per-operation branch survives with declared examples.
	token := dig(t, body, "properties", "businessRequest", "properties", "productToken")
	assert.Equal(t, "PT-abc", token["example"])
}

// Nesting must survive to full depth, including array item schemas, with the
// description/example/default/required that the spec declared.
func TestFidelity_NestedBodyKeepsDepthAndMetadata(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	rt := tools["queryHotelInfo"]
	require.NotNil(t, rt, "queryHotelInfo tool missing")

	body := prop(t, rt.Tool, "body")
	business := dig(t, body, "properties", "businessRequest")
	assert.Equal(t, []string{"hotelId"}, requiredList(t, business))

	hotelID := dig(t, business, "properties", "hotelId")
	assert.Equal(t, "string", hotelID["type"])
	assert.Equal(t, "Hotel id", hotelID["description"])
	assert.Equal(t, "H12345", hotelID["example"])

	language := dig(t, business, "properties", "language")
	assert.Equal(t, "zh_CN", language["default"])

	// Array items keep their object schema, otherwise a caller cannot know what
	// goes into the array.
	order := prop(t, tools["createOrder"].Tool, "body")
	guestName := dig(t, order, "properties", "businessRequest", "properties", "guests", "items", "properties", "name")
	assert.Equal(t, "string", guestName["type"])
	assert.Equal(t, "Guest name", guestName["description"])
}

// oneOf/anyOf branches are merged into one object so their fields stay
// reachable, and the branch requirements are stated in the description rather
// than silently dropped.
func TestFidelity_OneOfBranchesStayReachable(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	rt := tools["payOrder"]
	require.NotNil(t, rt, "payOrder tool missing")

	payment := dig(t, prop(t, rt.Tool, "body"), "properties", "payment")
	props := dig(t, payment, "properties")
	assert.Contains(t, props, "cardNo")
	assert.Contains(t, props, "walletId")
	assert.Empty(t, requiredList(t, payment), "no branch field may be unconditionally required")
	assert.NotEmpty(t, payment["description"], "the branch structure must be described")
}

// Path/query/header parameters keep their declared type, enum, default and
// example; header parameters must exist at all.
func TestFidelity_ParametersKeepTypeAndLocation(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	rt := tools["getOrderDetail"]
	require.NotNil(t, rt, "getOrderDetail tool missing")

	orderID := prop(t, rt.Tool, "orderId")
	assert.Equal(t, "string", orderID["type"])
	assert.Equal(t, "SO2026", orderID["example"])
	assert.Contains(t, rt.Tool.InputSchema.Required, "orderId")

	starRates := prop(t, rt.Tool, "starRates")
	assert.Equal(t, "array", starRates["type"], "an array query parameter must not be flattened to string")
	assert.Equal(t, "integer", dig(t, starRates, "items")["type"])

	locale := prop(t, rt.Tool, "locale")
	assert.Equal(t, "zh_CN", locale["default"])
	assert.ElementsMatch(t, []any{"zh_CN", "en_US"}, locale["enum"])

	assert.NotNil(t, prop(t, rt.Tool, "X-Trace-Id"), "header parameters must be exposed")
}

// GET is safely read-only; POST is not asserted either way because these APIs
// use POST for reads as well, and a wrong readOnlyHint=false is worse than none.
func TestFidelity_ReadOnlyHintOnlyForGet(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	require.Contains(t, tools, "getOrderDetail")
	require.Contains(t, tools, "queryHotelInfo")

	get := tools["getOrderDetail"].Tool.Annotations.ReadOnlyHint
	require.NotNil(t, get, "GET must be annotated read-only")
	assert.True(t, *get)

	assert.False(t, *tools["getOrderDetail"].Tool.Annotations.DestructiveHint)

	// POST carries no hint at all: mcp-go's defaults would otherwise assert
	// "not read-only, destructive" for every operation, which is a guess.
	post := tools["queryHotelInfo"].Tool.Annotations
	assert.Nil(t, post.ReadOnlyHint, "POST must not claim to be a write or a read")
	assert.Nil(t, post.DestructiveHint, "POST must not claim to be destructive")
	assert.Nil(t, post.IdempotentHint)
	assert.Nil(t, post.OpenWorldHint)
}

// Onboarding hands over YAML, so YAML must load.
func TestFidelity_YAMLSpecLoads(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecYAML))
	rt := tools["queryHotelInfo"]
	require.NotNil(t, rt, "queryHotelInfo tool missing; got %v", keysOfTools(tools))
	hotelID := dig(t, prop(t, rt.Tool, "body"), "properties", "businessRequest", "properties", "hotelId")
	assert.Equal(t, "H12345", hotelID["example"])
}

func keysOfTools(m map[string]*RouteTool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

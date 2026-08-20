package parser

import (
	"strings"
	"testing"

	"github.com/brizzai/auto-mcp/internal/requester"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
      "responses": { "200": { "description": "OK", "content": { "application/json": { "schema": {
        "type": "object",
        "properties": {
          "returnCode": { "type": "string", "description": "Upstream status code", "example": "000" },
          "bussinessResponse": { "type": "object", "properties": {
            "hotelName": { "type": "string", "description": "Hotel name" },
            "starRate": { "type": "integer" } } } } } } } } } } },
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
      "responses": { "200": { "description": "OK", "content": { "application/json": { "schema": {
        "type": "array", "items": { "type": "string" } } } } } } } },
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

// inputSchema returns the tool's inputSchema as a map. The SDK types the field
// as `any` so that a server can publish a schema it built itself.
func inputSchema(t *testing.T, tool *mcp.Tool) map[string]any {
	t.Helper()
	m, ok := tool.InputSchema.(map[string]any)
	require.True(t, ok, "tool %s inputSchema is not a map: %#v", tool.Name, tool.InputSchema)
	return m
}

func prop(t *testing.T, tool *mcp.Tool, name string) map[string]any {
	t.Helper()
	props := dig(t, inputSchema(t, tool), "properties")
	raw, ok := props[name]
	require.True(t, ok, "tool %s has no property %q (has %v)", tool.Name, name, keysOf(props))
	m, ok := raw.(map[string]any)
	require.True(t, ok, "property %q is not an object", name)
	return m
}

// toolRequired lists the tool-level required argument names.
func toolRequired(t *testing.T, tool *mcp.Tool) []string {
	t.Helper()
	return requiredList(t, inputSchema(t, tool))
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
	assert.Contains(t, toolRequired(t, rt.Tool), "orderId")

	starRates := prop(t, rt.Tool, "starRates")
	assert.Equal(t, "array", starRates["type"], "an array query parameter must not be flattened to string")
	assert.Equal(t, "integer", dig(t, starRates, "items")["type"])

	locale := prop(t, rt.Tool, "locale")
	assert.Equal(t, "zh_CN", locale["default"])
	assert.ElementsMatch(t, []any{"zh_CN", "en_US"}, locale["enum"])

	assert.NotNil(t, prop(t, rt.Tool, "X-Trace-Id"), "header parameters must be exposed")
}

// GET carries the guarantees HTTP gives it. POST carries no annotation at all,
// because these APIs serve reads over POST and the SDK marshals ReadOnlyHint as
// a bare bool: any annotation on a POST would publish readOnlyHint=false, which
// asserts that the operation writes.
func TestFidelity_AnnotationsOnlyStateWhatTheMethodGuarantees(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	require.Contains(t, tools, "getOrderDetail")
	require.Contains(t, tools, "queryHotelInfo")

	get := tools["getOrderDetail"].Tool.Annotations
	require.NotNil(t, get, "GET must be annotated")
	assert.True(t, get.ReadOnlyHint, "GET is read-only")
	require.NotNil(t, get.DestructiveHint)
	assert.False(t, *get.DestructiveHint, "GET does not destroy")
	assert.True(t, get.IdempotentHint, "GET is idempotent")

	assert.Nil(t, tools["queryHotelInfo"].Tool.Annotations,
		"POST must not claim to be either a read or a write")
	assert.Nil(t, tools["createOrder"].Tool.Annotations)
}

// The success response schema is published as outputSchema, so a caller can see
// what it can read back without having to call the tool to find out.
func TestFidelity_ResponseSchemaBecomesOutputSchema(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	require.Contains(t, tools, "queryHotelInfo")

	out, ok := tools["queryHotelInfo"].Tool.OutputSchema.(map[string]any)
	require.True(t, ok, "outputSchema must be a converted schema map")
	assert.Equal(t, "object", out["type"])

	code := dig(t, out, "properties", "returnCode")
	assert.Equal(t, "Upstream status code", code["description"])
	assert.Equal(t, "000", code["example"])

	// Nesting survives here for the same reason it does on the input side.
	hotelName := dig(t, out, "properties", "bussinessResponse", "properties", "hotelName")
	assert.Equal(t, "string", hotelName["type"])
	assert.Equal(t, "Hotel name", hotelName["description"])
}

// MCP requires the top level of an outputSchema to be an object. A response that
// is an array is left undeclared rather than misdeclared: a client that
// validates against the wrong shape would reject every successful call.
func TestFidelity_NonObjectResponseIsNotDeclared(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	require.Contains(t, tools, "payOrder")

	assert.Nil(t, tools["payOrder"].Tool.OutputSchema)
}

// A response with no schema at all leaves outputSchema unset.
func TestFidelity_MissingResponseSchemaLeavesOutputUnset(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	require.Contains(t, tools, "createOrder")

	assert.Nil(t, tools["createOrder"].Tool.OutputSchema)
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

// A parameter may be declared at path level and again at the operation level;
// the operation's declaration wins. Appending both leaves the argument listed
// twice, which puts a duplicate in the tool's required set and, for a query
// parameter, sends the value twice on the wire.
const duplicateParamSpec = `{
  "openapi": "3.0.1",
  "info": { "title": "Dup", "version": "1.0" },
  "paths": { "/api/order/{orderId}": {
    "parameters": [
      { "name": "orderId", "in": "path", "required": true, "schema": { "type": "string" },
        "description": "from the path item" },
      { "name": "locale", "in": "query", "required": true, "schema": { "type": "string" } }
    ],
    "get": {
      "operationId": "getOrder",
      "parameters": [
        { "name": "orderId", "in": "path", "required": true, "schema": { "type": "string" },
          "description": "from the operation" },
        { "name": "locale", "in": "query", "required": true, "schema": { "type": "string" } }
      ],
      "responses": { "200": { "description": "OK" } } } } } }`

func TestFidelity_DuplicateParameterDeclarationsAreMerged(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, duplicateParamSpec))
	rt := tools["getOrder"]
	require.NotNil(t, rt)

	assert.ElementsMatch(t, []string{"orderId", "locale"}, toolRequired(t, rt.Tool),
		"a parameter declared twice must appear once in required")

	// The operation-level declaration is the more specific one.
	assert.Equal(t, "from the operation", prop(t, rt.Tool, "orderId")["description"])

	seen := map[string]int{}
	for _, cfg := range rt.RouteConfig.MethodConfig.Params {
		seen[string(cfg.In)+"/"+cfg.Name]++
	}
	for key, count := range seen {
		assert.Equal(t, 1, count, "parameter %s recorded %d times", key, count)
	}
	assert.Equal(t, []string{"locale"}, rt.RouteConfig.MethodConfig.QueryParams)
}

// The request body media type decides how the body is encoded. Declaring
// application/json while sending a form-encoded endpoint's arguments as JSON is
// a silent mismatch the upstream rejects.
const formBodySpec = `{
  "openapi": "3.0.1",
  "info": { "title": "Form", "version": "1.0" },
  "paths": { "/api/login": { "post": {
    "operationId": "login",
    "requestBody": { "required": true, "content": { "application/x-www-form-urlencoded": {
      "schema": { "type": "object", "required": ["user"], "properties": {
        "user": { "type": "string" }, "pass": { "type": "string" } } } } } },
    "responses": { "200": { "description": "OK" } } } } } }`

func TestFidelity_FormEncodedBodyIsNotAdvertisedAsJSON(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, formBodySpec))
	rt := tools["login"]
	require.NotNil(t, rt)

	assert.Equal(t, "application/x-www-form-urlencoded", rt.RouteConfig.MethodConfig.BodyContentType)
	assert.NotEqual(t, "application/json", rt.RouteConfig.Headers["Content-Type"],
		"the route must not claim JSON for a form-encoded body")
}

func TestFidelity_JSONBodyKeepsItsMediaType(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	rt := tools["queryHotelInfo"]
	require.NotNil(t, rt)

	assert.Equal(t, "application/json", rt.RouteConfig.MethodConfig.BodyContentType)
}

// A request with no body must not declare a content type. It is the same rule as
// for the body encoding: never state a media type that describes no bytes.
func TestFidelity_BodylessRequestDeclaresNoContentType(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	rt := tools["getOrderDetail"]
	require.NotNil(t, rt)

	assert.NotContains(t, rt.RouteConfig.Headers, "Content-Type",
		"a GET carries no body, so it declares no content type")
}

// MCP has a Title field for the human-readable name; the summary belongs there
// rather than being concatenated into the description. Display precedence in the
// spec is title, then annotations.title, then name.
func TestFidelity_SummaryBecomesTheTitle(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))
	rt := tools["queryHotelInfo"]
	require.NotNil(t, rt)

	assert.Equal(t, "Query hotel detail", rt.Tool.Title)
}

// The description is what the document says about the operation. The method and
// path are addressing details that the caller cannot act on, and they crowded
// out the actual text.
func TestFidelity_DescriptionIsTheDocumentedText(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, merchantSpecJSON))

	assert.Equal(t, "Query hotel detail", tools["queryHotelInfo"].Tool.Description)
	assert.NotContains(t, tools["queryHotelInfo"].Tool.Description, "/api/queryHotelInfo")
}

// A tool with neither summary nor description still needs something to go on, so
// the method and path remain the fallback.
func TestFidelity_UndocumentedOperationFallsBackToMethodAndPath(t *testing.T) {
	const bare = `{
      "openapi": "3.0.1", "info": { "title": "Bare", "version": "1.0" },
      "paths": { "/api/thing": { "get": { "operationId": "getThing",
        "responses": { "200": { "description": "OK" } } } } } }`

	tools := toolsByName(t, parseSpec(t, bare))
	require.Contains(t, tools, "getThing")

	assert.Contains(t, tools["getThing"].Tool.Description, "/api/thing")
	assert.Contains(t, tools["getThing"].Tool.Description, "GET")
}

// Arguments live in one flat namespace, so two parameters in different locations
// that share a name would silently overwrite each other, and a parameter named
// "body" would collide with the request body. The collision is renamed rather
// than dropped, and the original name is kept for the wire.
const collidingParamSpec = `{
  "openapi": "3.0.1",
  "info": { "title": "Collide", "version": "1.0" },
  "paths": { "/api/search": { "post": {
    "operationId": "search",
    "parameters": [
      { "name": "token", "in": "query", "schema": { "type": "string" }, "description": "query token" },
      { "name": "token", "in": "header", "schema": { "type": "string" }, "description": "header token" },
      { "name": "body", "in": "query", "schema": { "type": "string" }, "description": "a parameter called body" }
    ],
    "requestBody": { "required": true, "content": { "application/json": {
      "schema": { "type": "object", "properties": { "q": { "type": "string" } } } } } },
    "responses": { "200": { "description": "OK" } } } } } }`

func TestFidelity_CollidingParameterNamesAreDisambiguated(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, collidingParamSpec))
	rt := tools["search"]
	require.NotNil(t, rt)

	props := dig(t, inputSchema(t, rt.Tool), "properties")

	// Every declared parameter is still reachable, under some name.
	assert.Len(t, props, 4, "3 parameters plus the body, none lost to a collision: %v", keysOf(props))

	// The request body keeps the "body" name; the parameter that wanted it moves.
	assert.Equal(t, "object", dig(t, props, "body")["type"], "the request body keeps its name")
	assert.Contains(t, props, "query_body")

	// The two "token" parameters are told apart by location.
	assert.Contains(t, props, "token")
	assert.Contains(t, props, "header_token")

	// The wire names are unchanged.
	byName := map[string]requester.ParamConfig{}
	for _, cfg := range rt.RouteConfig.MethodConfig.Params {
		byName[cfg.ArgName] = cfg
	}
	assert.Equal(t, "token", byName["header_token"].Name)
	assert.Equal(t, requester.ParamInHeader, byName["header_token"].In)
	assert.Equal(t, "body", byName["query_body"].Name)
	assert.Equal(t, requester.ParamInQuery, byName["query_body"].In)
}

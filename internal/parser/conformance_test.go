package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// OpenAPI 3.1 adopted JSON Schema 2020-12, where exclusiveMinimum is the bound
// itself rather than a flag on minimum. kin-openapi models the 3.0 shape, so a
// numeric value fails to unmarshal and takes the whole document with it — three
// occurrences in a 230 KiB spec were enough to make it unloadable.
const exclusiveBoundSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Bounds", "version": "1.0"},
  "paths": {"/api/items": {"post": {
    "operationId": "createItem",
    "requestBody": {"required": true, "content": {"application/json": {"schema": {
      "type": "object",
      "properties": {
        "quantity": {"type": "integer", "exclusiveMinimum": 0},
        "ratio":    {"type": "number", "exclusiveMaximum": 1}}}}}},
    "responses": {"200": {"description": "OK"}}}}}}`

func TestOpenAPI31_NumericExclusiveBoundsLoad(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, exclusiveBoundSpec))
	rt := tools["createItem"]
	require.NotNil(t, rt, "a 3.1 document with numeric exclusive bounds must load")

	// The published schema is read as 2020-12, where the keyword carries the
	// bound, so the value survives the round trip through kin's 3.0 model.
	body := prop(t, rt.Tool, "body")
	quantity := dig(t, body, "properties", "quantity")
	assert.EqualValues(t, 0, quantity["exclusiveMinimum"])
	assert.NotContains(t, quantity, "minimum", "an exclusive bound is not also an inclusive one")

	ratio := dig(t, body, "properties", "ratio")
	assert.EqualValues(t, 1, ratio["exclusiveMaximum"])
	assert.NotContains(t, ratio, "maximum")
}

// 3.1 has no `nullable`; an optional field is written as a union with null. This
// is how every FastAPI-generated document expresses it — 156 occurrences across
// the two specs measured — so it decides what most fields look like.
const nullableUnionSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Nullable", "version": "1.0"},
  "paths": {"/api/items": {"post": {
    "operationId": "createItem",
    "requestBody": {"required": true, "content": {"application/json": {"schema": {
      "type": "object",
      "required": ["name"],
      "properties": {
        "name":  {"type": "string"},
        "note":  {"anyOf": [{"type": "string"}, {"type": "null"}], "description": "Optional note"},
        "count": {"anyOf": [{"type": "integer"}, {"type": "null"}], "default": 0}}}}}},
    "responses": {"200": {"description": "OK"}}}}}}`

// A union with null is the field's own type, not an object. Falling through to
// the object default would tell the caller to send a JSON object where a string
// belongs.
func TestOpenAPI31_NullableUnionKeepsTheFieldType(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, nullableUnionSpec))
	rt := tools["createItem"]
	require.NotNil(t, rt)

	body := prop(t, rt.Tool, "body")

	note := dig(t, body, "properties", "note")
	assert.Equal(t, "string", note["type"], "a string-or-null field is a string field")
	assert.Equal(t, "Optional note", note["description"])

	count := dig(t, body, "properties", "count")
	assert.Equal(t, "integer", count["type"])
	assert.EqualValues(t, 0, count["default"])
}

// The union carried no constraint beyond "may be absent", so publishing it adds
// bytes and nothing else. With 156 of them in one deployment that is the
// difference between a readable tool list and a wall of noise.
func TestOpenAPI31_NullableUnionIsNotRepublished(t *testing.T) {
	tools := toolsByName(t, parseSpec(t, nullableUnionSpec))
	body := prop(t, tools["createItem"].Tool, "body")

	note := dig(t, body, "properties", "note")
	assert.NotContains(t, note, "anyOf", "a nullable union states nothing worth republishing")
	assert.NotContains(t, note, "oneOf")
	assert.NotContains(t, note["description"], "at least one of",
		"and nothing worth describing either")
}

// A real union of two usable shapes is still published; only the null idiom is
// collapsed.
func TestOpenAPI31_RealUnionIsStillPublished(t *testing.T) {
	const spec = `{
      "openapi": "3.1.0", "info": {"title": "U", "version": "1.0"},
      "paths": {"/api/pay": {"post": {"operationId": "pay",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {
          "type": "object", "properties": {"method": {"anyOf": [
            {"type": "object", "required": ["cardNo"], "properties": {"cardNo": {"type": "string"}}},
            {"type": "object", "required": ["walletId"], "properties": {"walletId": {"type": "string"}}},
            {"type": "null"}]}}}}}},
        "responses": {"200": {"description": "OK"}}}}}}`

	tools := toolsByName(t, parseSpec(t, spec))
	method := dig(t, prop(t, tools["pay"].Tool, "body"), "properties", "method")

	props := dig(t, method, "properties")
	assert.Contains(t, props, "cardNo")
	assert.Contains(t, props, "walletId")
	assert.Contains(t, method, "anyOf", "two usable shapes are a real union")
	assert.Len(t, branchList(t, method, "anyOf"), 2, "the null branch is not one of them")
}

// A document that does not conform is refused, with the reason it failed.
//
// The plural `types` key is the case that prompted this: it is a field of
// swagger-core's Java schema model rather than an OpenAPI keyword, and it appears
// when that model is serialised directly. Rewriting it was accommodating a broken
// document; only conformant documents are in scope, so it is reported instead.
// Silently loading it would publish a quarter of that document as untyped.
const nonConformantSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Plural", "version": "1.0"},
  "paths": {"/api/items": {"post": {
    "operationId": "createItem",
    "requestBody": {"required": true, "content": {"application/json": {"schema": {
      "type": "object",
      "properties": {"name": {"types": ["string"], "exampleSetFlag": false}}}}}},
    "responses": {"200": {"description": "OK"}}}}}}`

func TestConformance_NonConformantDocumentIsRefused(t *testing.T) {
	p := NewSwaggerParser(NewAdjuster())

	err := p.ParseReader(strings.NewReader(nonConformantSpec))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "types", "the reason names the offending field")
}

// A pattern Go cannot compile is not a conformance failure. OpenAPI mandates
// ECMA-262 regular expressions, which permit lookahead; Go's RE2 does not
// implement it. Refusing such a document would reject a correct specification for
// a limitation of this implementation — and both real specifications measured use
// exactly this construct.
func TestConformance_UnsupportedRegexIsNotAConformanceFailure(t *testing.T) {
	const spec = `{
      "openapi": "3.1.0", "info": {"title": "Re", "version": "1.0"},
      "paths": {"/api/items": {"post": {"operationId": "createItem",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"name": {"type": "string",
            "pattern": "(?!^[*.,'#_/-]+$)(?!.*\\./.*)^.*$"}}}}}},
        "responses": {"200": {"description": "OK"}}}}}}`

	tools := toolsByName(t, parseSpec(t, spec))
	require.Contains(t, tools, "createItem")

	name := dig(t, prop(t, tools["createItem"].Tool, "body"), "properties", "name")
	assert.Equal(t, "string", name["type"])
}

// A schema that states no type permits any type. Defaulting to object narrows it
// to the one shape it probably is not, and a consumer that trusts the schema then
// asks for a JSON object where a string belongs.
func TestOpenAPI31_UnstatedTypeIsLeftUnstated(t *testing.T) {
	const spec = `{
      "openapi": "3.1.0", "info": {"title": "U", "version": "1.0"},
      "paths": {"/api/items": {"post": {"operationId": "createItem",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {
            "anything":  {"description": "no type given"},
            "structured": {"properties": {"inner": {"type": "string"}}},
            "listish":   {"items": {"type": "string"}}}}}}},
        "responses": {"200": {"description": "OK"}}}}}}`

	body := prop(t, toolsByName(t, parseSpec(t, spec))["createItem"].Tool, "body")

	assert.NotContains(t, dig(t, body, "properties", "anything"), "type",
		"a schema with nothing to go on states no type")
	assert.Equal(t, "object", dig(t, body, "properties", "structured")["type"],
		"properties are evidence of an object")
	assert.Equal(t, "array", dig(t, body, "properties", "listish")["type"],
		"items are evidence of an array")
}

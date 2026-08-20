package parser

import (
	"sort"

	"github.com/getkin/kin-openapi/openapi3"
)

// inputSchemaBuilder assembles a tool's inputSchema.
//
// The MCP Go SDK takes `InputSchema any` and marshals whatever it is given, so
// the schema is built here as a plain JSON Schema map. There is no options DSL
// in between, which removes a class of bug the previous binding had: it
// signalled "this property is required" by setting the property's own
// `required` key to the boolean true and hoisting it afterwards, so an object
// property that carried a `required` list of its own overwrote the marker and
// the property silently dropped out of the tool's required set.
type inputSchemaBuilder struct {
	properties map[string]any
	required   []string
}

func newInputSchemaBuilder() *inputSchemaBuilder {
	return &inputSchemaBuilder{properties: map[string]any{}}
}

// add installs one named property, recording the tool-level required flag
// separately from the property's own schema.
func (b *inputSchemaBuilder) add(name string, schema map[string]any, required bool) {
	if name == "" {
		return
	}
	b.properties[name] = schema
	if required {
		b.required = append(b.required, name)
	}
}

func (b *inputSchemaBuilder) has(name string) bool {
	_, ok := b.properties[name]
	return ok
}

// build returns the assembled schema. `properties` is always present, even when
// empty: a tool with no arguments still declares an object input, and omitting
// the key makes some clients treat the schema as unknown rather than empty.
func (b *inputSchemaBuilder) build() map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": b.properties,
	}
	if len(b.required) > 0 {
		required := append([]string(nil), b.required...)
		sort.Strings(required)
		out["required"] = required
	}
	return out
}

// bodySchema converts a request body schema into the `body` property's schema.
func bodySchema(schema *openapi3.SchemaRef) map[string]any {
	if schema == nil || schema.Value == nil {
		return map[string]any{"type": "object", "description": "Request body"}
	}
	return jsonSchemaFor(schema)
}

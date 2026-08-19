package parser

import (
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/mark3labs/mcp-go/mcp"
)

// schemaToMCPOptions exposes an OpenAPI schema as one named MCP tool property.
func schemaToMCPOptions(schema *openapi3.SchemaRef, name string, required bool) mcp.ToolOption {
	if schema == nil || schema.Value == nil {
		return withSchemaProperty(name, map[string]any{
			"type":        "object",
			"description": "Request body",
		}, required)
	}
	return withSchemaProperty(name, jsonSchemaFor(schema), required)
}

// withSchemaProperty installs an already-built JSON Schema as a named property
// and records the tool-level required flag separately.
//
// It does not go through mcp.WithObject/mcp.Required: those communicate
// "this property is required" by setting the property's own `required` key to
// the boolean true and hoisting it afterwards, which collides with an object
// property that has a `required` list of its own. The list overwrites the
// boolean, the hoist then finds no boolean, and the property is quietly absent
// from the tool's required set — so a mandatory request body looks optional.
func withSchemaProperty(name string, schema map[string]any, required bool) mcp.ToolOption {
	return func(t *mcp.Tool) {
		if t.InputSchema.Properties == nil {
			t.InputSchema.Properties = map[string]any{}
		}
		t.InputSchema.Properties[name] = schema
		if required {
			t.InputSchema.Required = append(t.InputSchema.Required, name)
		}
	}
}

package parser

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaToMCPOptions(t *testing.T) {
	tests := []struct {
		name     string
		schema   *openapi3.SchemaRef
		required bool
		check    func(t *testing.T, got mcp.ToolOption)
	}{
		{
			name:     "nil schema",
			schema:   nil,
			required: false,
			check: func(t *testing.T, got mcp.ToolOption) {
				tool := mcp.NewTool("test", got)
				assert.Equal(t, "test", tool.Name)
				assert.Empty(t, tool.Description)
			},
		},
		{
			name: "array schema",
			schema: &openapi3.SchemaRef{
				Value: &openapi3.Schema{
					Type: &openapi3.Types{"array"},
					Items: &openapi3.SchemaRef{
						Value: &openapi3.Schema{
							Type: &openapi3.Types{"string"},
						},
					},
				},
			},
			required: true,
			check: func(t *testing.T, got mcp.ToolOption) {
				tool := mcp.NewTool("test", got)
				assert.Equal(t, "test", tool.Name)
				prop, ok := tool.InputSchema.Properties["test"].(map[string]interface{})
				assert.True(t, ok)
				assert.Equal(t, "array", prop["type"])
				if len(tool.InputSchema.Required) > 0 {
					assert.Contains(t, tool.InputSchema.Required, "test")
				}
			},
		},
		{
			name: "object schema with properties",
			schema: &openapi3.SchemaRef{
				Value: &openapi3.Schema{
					Type: &openapi3.Types{"object"},
					Properties: map[string]*openapi3.SchemaRef{
						"name": {
							Value: &openapi3.Schema{
								Type: &openapi3.Types{"string"},
							},
						},
					},
					Required: []string{"name"},
				},
			},
			required: true,
			check: func(t *testing.T, got mcp.ToolOption) {
				tool := mcp.NewTool("test", got)
				assert.Equal(t, "test", tool.Name)
				assert.Equal(t, "object", tool.InputSchema.Type)

				// Debug: Print the actual structure
				t.Logf("InputSchema: %+v", tool.InputSchema)
				t.Logf("Properties: %+v", tool.InputSchema.Properties)

				prop, ok := tool.InputSchema.Properties["test"].(map[string]interface{})
				assert.True(t, ok)
				t.Logf("Test property: %+v", prop)

				// Get the nested properties map
				objPropsVal := prop["properties"]
				t.Logf("objPropsVal: %T, %+v", objPropsVal, objPropsVal)
				objProps, ok := objPropsVal.(map[string]interface{})
				assert.True(t, ok)
				t.Logf("Properties map: %+v", objProps)
				// Check if name exists in properties
				assert.Contains(t, objProps, "name")
				// Check required fields
				reqVal := prop["required"]
				t.Logf("reqVal: %T, %+v", reqVal, reqVal)
				req, ok := reqVal.([]string)
				if !ok {
					reqIface, ok2 := reqVal.([]interface{})
					assert.True(t, ok2)
					for _, r := range reqIface {
						if s, ok := r.(string); ok && s == "name" {
							return
						}
					}
					assert.Fail(t, "'name' not found in required fields")
				} else {
					found := false
					for _, r := range req {
						if r == "name" {
							found = true
						}
					}
					assert.True(t, found)
				}
			},
		},
		{
			name: "string schema with constraints",
			schema: &openapi3.SchemaRef{
				Value: &openapi3.Schema{
					Type:        &openapi3.Types{"string"},
					MaxLength:   openapi3.Uint64Ptr(100),
					MinLength:   1,
					Pattern:     "^[a-zA-Z]+$",
					Enum:        []interface{}{"option1", "option2"},
					Description: "Test string",
				},
			},
			required: false,
			check: func(t *testing.T, got mcp.ToolOption) {
				tool := mcp.NewTool("test", got)
				prop, ok := tool.InputSchema.Properties["test"].(map[string]interface{})
				assert.True(t, ok)
				assert.Equal(t, "Test string", prop["description"])
				assert.EqualValues(t, 100, prop["maxLength"])
				assert.EqualValues(t, 1, prop["minLength"])
				assert.Equal(t, "^[a-zA-Z]+$", prop["pattern"])
				assert.ElementsMatch(t, []interface{}{"option1", "option2"}, prop["enum"])
			},
		},
		{
			name: "number schema with constraints",
			schema: &openapi3.SchemaRef{
				Value: &openapi3.Schema{
					Type:        &openapi3.Types{"number"},
					Max:         openapi3.Float64Ptr(100),
					Min:         openapi3.Float64Ptr(0),
					MultipleOf:  openapi3.Float64Ptr(2),
					Description: "Test number",
				},
			},
			required: true,
			check: func(t *testing.T, got mcp.ToolOption) {
				tool := mcp.NewTool("test", got)
				prop, ok := tool.InputSchema.Properties["test"].(map[string]interface{})
				assert.True(t, ok)
				assert.Equal(t, "Test number", prop["description"])
				assert.Equal(t, 100.0, prop["maximum"])
				assert.Equal(t, 0.0, prop["minimum"])
				assert.Equal(t, 2.0, prop["multipleOf"])
			},
		},
		{
			name: "boolean schema",
			schema: &openapi3.SchemaRef{
				Value: &openapi3.Schema{
					Type:        &openapi3.Types{"boolean"},
					Description: "Test boolean",
				},
			},
			required: false,
			check: func(t *testing.T, got mcp.ToolOption) {
				tool := mcp.NewTool("test", got)
				prop, ok := tool.InputSchema.Properties["test"].(map[string]interface{})
				assert.True(t, ok)
				assert.Equal(t, "Test boolean", prop["description"])
				assert.Equal(t, "boolean", prop["type"])
			},
		},
		{
			name: "unknown type schema",
			schema: &openapi3.SchemaRef{
				Value: &openapi3.Schema{
					Type:        &openapi3.Types{"unknown"},
					Description: "Test unknown",
				},
			},
			required: false,
			check: func(t *testing.T, got mcp.ToolOption) {
				tool := mcp.NewTool("test", got)
				assert.Empty(t, tool.Description)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := schemaToMCPOptions(tt.schema, "test", tt.required)
			tt.check(t, got)
		})
	}
}

// The four schema shapes below used to be handled by separate create*Option
// helpers. They now go through jsonSchemaFor, so the facets are asserted on the
// converted schema itself.
func TestJSONSchemaFor_ArrayKeepsItems(t *testing.T) {
	got := jsonSchemaFor(&openapi3.SchemaRef{Value: &openapi3.Schema{
		Description: "Test array",
		Items:       &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}},
	}})

	assert.Equal(t, "array", got["type"], "items imply an array even when type is absent")
	assert.Equal(t, "Test array", got["description"])
	items, ok := got["items"].(map[string]any)
	require.True(t, ok, "items must be a converted schema, not a kin-openapi struct")
	assert.Equal(t, "string", items["type"])
}

func TestJSONSchemaFor_ObjectKeepsPropertiesAndConstraints(t *testing.T) {
	got := jsonSchemaFor(&openapi3.SchemaRef{Value: &openapi3.Schema{
		Description: "Test object",
		Properties: map[string]*openapi3.SchemaRef{
			"name": {Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}},
		},
		Required:             []string{"name"},
		MaxProps:             openapi3.Uint64Ptr(10),
		MinProps:             1,
		AdditionalProperties: openapi3.AdditionalProperties{Has: openapi3.BoolPtr(true)},
	}})

	assert.Equal(t, "object", got["type"])
	assert.Equal(t, "Test object", got["description"])
	props, ok := got["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "name")
	assert.Equal(t, []string{"name"}, got["required"])
	assert.EqualValues(t, 10, got["maxProperties"])
	assert.EqualValues(t, 1, got["minProperties"])
	assert.Equal(t, true, got["additionalProperties"])
}

func TestJSONSchemaFor_StringKeepsConstraints(t *testing.T) {
	got := jsonSchemaFor(&openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{"string"},
		MaxLength:   openapi3.Uint64Ptr(100),
		MinLength:   1,
		Pattern:     "^[a-zA-Z]+$",
		Enum:        []interface{}{"option1", "option2"},
		Description: "Test string",
	}})

	assert.Equal(t, "string", got["type"])
	assert.Equal(t, "Test string", got["description"])
	assert.EqualValues(t, 100, got["maxLength"])
	assert.EqualValues(t, 1, got["minLength"])
	assert.Equal(t, "^[a-zA-Z]+$", got["pattern"])
	assert.ElementsMatch(t, []interface{}{"option1", "option2"}, got["enum"])
}

func TestJSONSchemaFor_NumberKeepsConstraints(t *testing.T) {
	got := jsonSchemaFor(&openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{"number"},
		Max:         openapi3.Float64Ptr(100),
		Min:         openapi3.Float64Ptr(0),
		MultipleOf:  openapi3.Float64Ptr(2),
		Description: "Test number",
	}})

	assert.Equal(t, "number", got["type"])
	assert.Equal(t, "Test number", got["description"])
	assert.Equal(t, 100.0, got["maximum"])
	assert.Equal(t, 0.0, got["minimum"])
	assert.Equal(t, 2.0, got["multipleOf"])
}

// A schema that references itself must terminate instead of recursing until the
// stack runs out; kin resolves $ref into pointers, so a self-referencing node
// is the same pointer twice on one path.
func TestJSONSchemaFor_CyclicRefTerminates(t *testing.T) {
	node := &openapi3.Schema{
		Type:       &openapi3.Types{"object"},
		Properties: map[string]*openapi3.SchemaRef{"value": {Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}}},
	}
	node.Properties["child"] = &openapi3.SchemaRef{Value: node}

	got := jsonSchemaFor(&openapi3.SchemaRef{Value: node})

	child, ok := got["properties"].(map[string]any)["child"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, recursionNote, child["description"])
}

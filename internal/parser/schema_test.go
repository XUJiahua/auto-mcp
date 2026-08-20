package parser

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONSchemaFor_Types(t *testing.T) {
	tests := []struct {
		name   string
		schema *openapi3.SchemaRef
		check  func(t *testing.T, got map[string]any)
	}{
		{
			name:   "nil schema",
			schema: nil,
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, "object", got["type"], "a missing schema still has to be a usable object")
			},
		},
		{
			name: "array schema",
			schema: &openapi3.SchemaRef{Value: &openapi3.Schema{
				Type:  &openapi3.Types{"array"},
				Items: &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}},
			}},
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, "array", got["type"])
				assert.Equal(t, "string", dig(t, got, "items")["type"])
			},
		},
		{
			name: "object schema with properties",
			schema: &openapi3.SchemaRef{Value: &openapi3.Schema{
				Type: &openapi3.Types{"object"},
				Properties: map[string]*openapi3.SchemaRef{
					"name": {Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}},
				},
				Required: []string{"name"},
			}},
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, "object", got["type"])
				assert.Contains(t, dig(t, got, "properties"), "name")
				assert.Equal(t, []string{"name"}, got["required"])
			},
		},
		{
			name: "boolean schema",
			schema: &openapi3.SchemaRef{Value: &openapi3.Schema{
				Type:        &openapi3.Types{"boolean"},
				Description: "Test boolean",
			}},
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, "boolean", got["type"])
				assert.Equal(t, "Test boolean", got["description"])
			},
		},
		{
			name: "unknown type is passed through rather than dropped",
			schema: &openapi3.SchemaRef{Value: &openapi3.Schema{
				Type:        &openapi3.Types{"unknown"},
				Description: "Test unknown",
			}},
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, "unknown", got["type"])
				assert.Equal(t, "Test unknown", got["description"])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, jsonSchemaFor(tt.schema))
		})
	}
}

// An object property carrying its own `required` list must not disturb the
// tool-level required set. The previous binding expressed both with the same
// key, so a mandatory request body silently became optional.
func TestInputSchemaBuilder_RequiredIsTrackedSeparately(t *testing.T) {
	b := newInputSchemaBuilder()
	b.add("body", map[string]any{
		"type":     "object",
		"required": []string{"header", "businessRequest"},
	}, true)
	b.add("locale", map[string]any{"type": "string"}, false)

	got := b.build()

	assert.Equal(t, []string{"body"}, got["required"], "only body is a required argument")
	body := dig(t, got, "properties", "body")
	assert.Equal(t, []string{"header", "businessRequest"}, body["required"],
		"the body's own required list must survive untouched")
	assert.True(t, b.has("locale"))
	assert.False(t, b.has("missing"))
}

// A tool with no arguments still declares an empty object input.
func TestInputSchemaBuilder_EmptyStillDeclaresObject(t *testing.T) {
	got := newInputSchemaBuilder().build()

	assert.Equal(t, "object", got["type"])
	assert.NotNil(t, got["properties"])
	assert.NotContains(t, got, "required")
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

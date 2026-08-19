package parser

import (
	"fmt"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// maxSchemaDepth bounds recursion for schemas that are deep rather than
// circular. Circular ones are caught by pointer identity, but a spec can also
// be legitimately deep, and an MCP client has to read whatever we emit.
const maxSchemaDepth = 20

const (
	recursionNote = "recursive structure; nested levels omitted"
	depthNote     = "structure deeper than the conversion limit; nested levels omitted"
)

// jsonSchemaFor converts an OpenAPI schema into a self-contained JSON Schema
// map suitable for an MCP tool's inputSchema.
//
// Self-contained matters: the result carries no `$ref` and no `$defs`, because
// consumers of tools/list are not required to resolve references and several
// real ones silently degrade an unresolved `$ref` into an opaque string. kin's
// loader has already resolved refs into pointers by the time we get here; this
// function inlines them.
//
// Three OpenAPI constructs need a decision rather than a translation:
//
//   - allOf is merged. It is the normal way a spec factors out a shared request
//     part (a credentials/signature `header` object, say). Dropping a member
//     loses a whole branch of the request, and a caller that then sends that
//     branch gets it silently ignored.
//   - oneOf/anyOf are merged into one object whose properties are the union of
//     the branches, with nothing marked required and the branch requirements
//     stated in the description. Emitting the composition verbatim is more
//     faithful but less useful: consumers that do not implement it collapse the
//     whole property to a scalar, and then none of the branch fields are
//     reachable at all.
//   - a schema with neither type nor properties becomes an object rather than
//     being omitted, so the property still exists and can carry a description.
func jsonSchemaFor(ref *openapi3.SchemaRef) map[string]any {
	return convertSchema(ref, map[*openapi3.Schema]bool{}, 0)
}

func convertSchema(ref *openapi3.SchemaRef, onPath map[*openapi3.Schema]bool, depth int) map[string]any {
	if ref == nil || ref.Value == nil {
		return map[string]any{"type": "object"}
	}
	schema := ref.Value

	if onPath[schema] {
		return map[string]any{"type": "object", "description": recursionNote}
	}
	if depth >= maxSchemaDepth {
		return map[string]any{"type": "object", "description": depthNote}
	}
	onPath[schema] = true
	defer delete(onPath, schema)

	out := scalarFacets(schema)

	if len(schema.AllOf) > 0 {
		for _, member := range schema.AllOf {
			mergeSchema(out, convertSchema(member, onPath, depth+1))
		}
	}

	if branches := append(append(openapi3.SchemaRefs{}, schema.OneOf...), schema.AnyOf...); len(branches) > 0 {
		converted := make([]map[string]any, 0, len(branches))
		for _, branch := range branches {
			converted = append(converted, convertSchema(branch, onPath, depth+1))
		}
		unionBranches(out, converted)
	}

	if len(schema.Properties) > 0 {
		props := childProperties(out)
		for name, child := range schema.Properties {
			props[name] = convertSchema(child, onPath, depth+1)
		}
		out["properties"] = props
	}
	if len(schema.Required) > 0 {
		out["required"] = mergeRequired(out["required"], schema.Required)
	}
	if schema.Items != nil {
		out["items"] = convertSchema(schema.Items, onPath, depth+1)
	}
	if schema.AdditionalProperties.Has != nil && *schema.AdditionalProperties.Has {
		if schema.AdditionalProperties.Schema != nil {
			out["additionalProperties"] = convertSchema(schema.AdditionalProperties.Schema, onPath, depth+1)
		} else {
			out["additionalProperties"] = true
		}
	}

	if _, ok := out["type"]; !ok {
		switch {
		case out["items"] != nil:
			out["type"] = "array"
		default:
			out["type"] = "object"
		}
	}
	return out
}

// scalarFacets copies the facets that describe a value rather than a structure.
func scalarFacets(schema *openapi3.Schema) map[string]any {
	out := map[string]any{}
	if schema.Type != nil && len(schema.Type.Slice()) > 0 {
		out["type"] = schema.Type.Slice()[0]
	}
	if schema.Description != "" {
		out["description"] = schema.Description
	}
	if schema.Format != "" {
		out["format"] = schema.Format
	}
	if len(schema.Enum) > 0 {
		out["enum"] = schema.Enum
	}
	if schema.Default != nil {
		out["default"] = schema.Default
	}
	if schema.Example != nil {
		out["example"] = schema.Example
	}

	if schema.MaxLength != nil {
		out["maxLength"] = *schema.MaxLength
	}
	if schema.MinLength != 0 {
		out["minLength"] = schema.MinLength
	}
	if schema.Pattern != "" {
		out["pattern"] = schema.Pattern
	}
	if schema.Max != nil {
		out["maximum"] = *schema.Max
	}
	if schema.Min != nil {
		out["minimum"] = *schema.Min
	}
	if schema.MultipleOf != nil {
		out["multipleOf"] = *schema.MultipleOf
	}
	if schema.MaxItems != nil {
		out["maxItems"] = *schema.MaxItems
	}
	if schema.MinItems != 0 {
		out["minItems"] = schema.MinItems
	}
	if schema.MaxProps != nil {
		out["maxProperties"] = *schema.MaxProps
	}
	if schema.MinProps != 0 {
		out["minProperties"] = schema.MinProps
	}
	return out
}

// mergeSchema folds an allOf member into the accumulated schema. Facets already
// present win: the outer schema is the more specific statement, and allOf
// members that disagree on a facet are a defect in the spec rather than
// something we can resolve here.
func mergeSchema(into, member map[string]any) {
	for key, value := range member {
		switch key {
		case "properties":
			props := childProperties(into)
			if memberProps, ok := value.(map[string]any); ok {
				for name, child := range memberProps {
					if _, exists := props[name]; !exists {
						props[name] = child
					}
				}
			}
			into["properties"] = props
		case "required":
			into["required"] = mergeRequired(into["required"], toStringSlice(value))
		case "description":
			if existing, _ := into["description"].(string); existing == "" {
				into["description"] = value
			}
		default:
			if _, exists := into[key]; !exists {
				into[key] = value
			}
		}
	}
}

// unionBranches collapses oneOf/anyOf branches into one object. Requirements
// per branch move into the description because they are conditional, and a
// conditional requirement written as an unconditional one makes every call
// look invalid until every branch is satisfied at once.
func unionBranches(into map[string]any, branches []map[string]any) {
	props := childProperties(into)
	summaries := make([]string, 0, len(branches))
	for i, branch := range branches {
		if branchProps, ok := branch["properties"].(map[string]any); ok {
			for name, child := range branchProps {
				if _, exists := props[name]; !exists {
					props[name] = child
				}
			}
		}
		summaries = append(summaries, branchSummary(i, branch))
	}
	if len(props) == 0 {
		return
	}
	into["properties"] = props
	into["type"] = "object"
	delete(into, "required")

	note := "exactly one of the following shapes: " + strings.Join(summaries, "; ")
	if existing, _ := into["description"].(string); existing != "" {
		note = existing + "\n" + note
	}
	into["description"] = note
}

func branchSummary(index int, branch map[string]any) string {
	required := toStringSlice(branch["required"])
	if len(required) == 0 {
		if props, ok := branch["properties"].(map[string]any); ok && len(props) > 0 {
			return fmt.Sprintf("(%d) any of %s", index+1, strings.Join(sortedKeys(props), ", "))
		}
		return fmt.Sprintf("(%d) no required fields", index+1)
	}
	return fmt.Sprintf("(%d) requires %s", index+1, strings.Join(required, ", "))
}

func childProperties(schema map[string]any) map[string]any {
	if existing, ok := schema["properties"].(map[string]any); ok {
		return existing
	}
	return map[string]any{}
}

// mergeRequired unions required lists and sorts the result so that the same
// spec always converts to the same bytes; Go map iteration order otherwise
// leaks into the output.
func mergeRequired(existing any, added []string) []string {
	seen := map[string]bool{}
	for _, name := range append(toStringSlice(existing), added...) {
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func toStringSlice(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

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
		unionBranches(out, converted, schema.Discriminator)
	}

	// `not` cannot be expressed once the schema is flattened, but dropping it
	// without a word leaves the caller unaware that the constraint exists at
	// all; it will find out from the upstream rejecting the call.
	if schema.Not != nil {
		appendDescription(out, notSummary(convertSchema(schema.Not, onPath, depth+1)))
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

// unionBranches collapses oneOf/anyOf branches into one object.
//
// Per-branch requirements move into the description because they are
// conditional, and a conditional requirement written as an unconditional one
// makes every call look invalid until every branch is satisfied at once.
//
// A property that several branches declare is merged rather than taken from the
// first branch that has it. First-wins is not merely lossy here: with the usual
// discriminator pattern every branch declares the selecting property with its
// own single-value enum, so keeping the first branch's enum publishes a schema
// claiming the discriminator accepts exactly one value, which makes every other
// branch unreachable.
func unionBranches(into map[string]any, branches []map[string]any, discriminator *openapi3.Discriminator) {
	props := childProperties(into)
	summaries := make([]string, 0, len(branches))
	for i, branch := range branches {
		if branchProps, ok := branch["properties"].(map[string]any); ok {
			for name, child := range branchProps {
				childSchema, _ := child.(map[string]any)
				if existing, ok := props[name].(map[string]any); ok && childSchema != nil {
					mergeBranchProperty(existing, childSchema)
					continue
				}
				props[name] = child
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
	if discriminator != nil && discriminator.PropertyName != "" {
		note += fmt.Sprintf("; selected by %s", discriminator.PropertyName)
	}
	appendDescription(into, note)
}

// mergeBranchProperty folds one branch's view of a property into the union.
//
// The union has to describe everything any branch accepts, so each facet widens
// rather than narrows:
//
//   - enum: the union of the values, and dropped entirely if any branch leaves
//     the property unconstrained, because that branch accepts anything.
//   - type: both types are published when branches disagree. JSON Schema allows
//     a list, and picking one arbitrarily would reject values a branch accepts.
//   - nested required: the intersection. Only fields every branch demands are
//     unconditionally required; a union would demand fields one branch never
//     uses, which is the same mistake as hoisting branch requirements.
//   - nested properties: merged recursively on the same rules.
//
// Remaining facets keep the first branch's value. Numeric and length bounds that
// disagree across branches cannot be widened without inventing a constraint the
// document does not state, and first-wins is at least deterministic.
func mergeBranchProperty(into, incoming map[string]any) {
	mergeBranchEnum(into, incoming)
	mergeBranchType(into, incoming)

	if intoRequired, incomingRequired := toStringSlice(into["required"]), toStringSlice(incoming["required"]); len(intoRequired) > 0 {
		if shared := intersect(intoRequired, incomingRequired); len(shared) > 0 {
			into["required"] = shared
		} else {
			delete(into, "required")
		}
	}

	intoProps, hasInto := into["properties"].(map[string]any)
	incomingProps, hasIncoming := incoming["properties"].(map[string]any)
	if hasInto && hasIncoming {
		for name, child := range incomingProps {
			childSchema, _ := child.(map[string]any)
			if existing, ok := intoProps[name].(map[string]any); ok && childSchema != nil {
				mergeBranchProperty(existing, childSchema)
				continue
			}
			intoProps[name] = child
		}
	} else if hasIncoming && !hasInto {
		into["properties"] = incomingProps
	}

	if existing, _ := into["description"].(string); existing == "" {
		if description, ok := incoming["description"].(string); ok {
			into["description"] = description
		}
	}
}

func mergeBranchEnum(into, incoming map[string]any) {
	intoValues, intoHas := into["enum"].([]any)
	incomingValues, incomingHas := incoming["enum"].([]any)
	if !intoHas && !incomingHas {
		return
	}
	if !intoHas || !incomingHas {
		// One branch accepts anything, so the union does too.
		delete(into, "enum")
		return
	}
	into["enum"] = unionValues(intoValues, incomingValues)
}

func mergeBranchType(into, incoming map[string]any) {
	intoTypes, incomingTypes := typeNames(into["type"]), typeNames(incoming["type"])
	if len(incomingTypes) == 0 {
		// An unconstrained type widens the union; say nothing rather than
		// claiming the first branch's type.
		delete(into, "type")
		return
	}
	if len(intoTypes) == 0 {
		return
	}
	merged := unionStrings(intoTypes, incomingTypes)
	if len(merged) == 1 {
		into["type"] = merged[0]
		return
	}
	into["type"] = merged
}

// notSummary describes an excluded shape in one clause.
func notSummary(excluded map[string]any) string {
	facets := make([]string, 0, 3)
	if values, ok := excluded["enum"].([]any); ok && len(values) > 0 {
		rendered := make([]string, 0, len(values))
		for _, value := range values {
			rendered = append(rendered, fmt.Sprintf("%v", value))
		}
		sort.Strings(rendered)
		facets = append(facets, "one of "+strings.Join(rendered, ", "))
	}
	if required := toStringSlice(excluded["required"]); len(required) > 0 {
		facets = append(facets, "an object with "+strings.Join(required, ", "))
	}
	if len(facets) == 0 {
		if name, ok := excluded["type"].(string); ok && name != "" {
			facets = append(facets, "of type "+name)
		}
	}
	if len(facets) == 0 {
		return "the document also excludes a shape here (not), which is not represented in this schema"
	}
	return "must not be " + strings.Join(facets, " or ")
}

// appendDescription adds a clause without discarding what is already there.
func appendDescription(schema map[string]any, note string) {
	if note == "" {
		return
	}
	if existing, _ := schema["description"].(string); existing != "" {
		schema["description"] = existing + "\n" + note
		return
	}
	schema["description"] = note
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

// typeNames normalises the `type` facet, which may be absent, a string, or a
// list of strings.
func typeNames(value any) []string {
	switch v := value.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if name, ok := item.(string); ok && name != "" {
				out = append(out, name)
			}
		}
		return out
	default:
		return nil
	}
}

// unionStrings merges and sorts, so the same document always converts to the
// same bytes.
func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	for _, item := range append(append([]string(nil), a...), b...) {
		seen[item] = true
	}
	out := make([]string, 0, len(seen))
	for item := range seen {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

// unionValues merges enum values, keeping first-seen order so the result is
// stable and the branches stay readable in the order the document listed them.
func unionValues(a, b []any) []any {
	out := make([]any, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, value := range append(append([]any(nil), a...), b...) {
		key := fmt.Sprintf("%T/%v", value, value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

// intersect keeps the entries present in both lists, sorted.
func intersect(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, item := range b {
		inB[item] = true
	}
	out := make([]string, 0, len(a))
	for _, item := range a {
		if inB[item] {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

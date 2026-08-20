package parser

import (
	"github.com/brizzai/auto-mcp/internal/logger"
	"go.uber.org/zap"
)

// OpenAPI 3.1 adopted JSON Schema 2020-12, which changed the meaning of a few
// keywords. kin-openapi models the 3.0 shape, so a 3.1 document that uses the new
// form fails to unmarshal — and it fails entirely, not partially: three numeric
// exclusiveMinimum values in a 230 KiB specification were enough to make the whole
// document unloadable.
//
// The constructs handled here are the ones real 3.1 documents were measured to
// contain. They are rewritten into the equivalent 3.0 form, which is a lossless
// change of spelling rather than an interpretation.

// normalizeOpenAPI31 rewrites 3.1-only spellings in a decoded document.
//
// It walks the whole document rather than only schema positions: schemas appear
// under parameters, request bodies, responses, headers and components, and
// missing one of those places would leave exactly the kind of partial failure this
// is meant to remove.
func normalizeOpenAPI31(node any) any {
	rewritten := 0
	normalized := normalizeNode(node, &rewritten)
	if rewritten > 0 {
		logger.Warn("Specification declares types with the non-standard \"types\" key; "+
			"treating it as \"type\". This is a swagger-core model artifact, not OpenAPI — "+
			"the document should be regenerated with a conformant writer",
			zap.Int("properties", rewritten))
	}
	return normalized
}

func normalizeNode(node any, rewritten *int) any {
	switch value := node.(type) {
	case map[string]any:
		normalizeExclusiveBound(value, "exclusiveMinimum", "minimum")
		normalizeExclusiveBound(value, "exclusiveMaximum", "maximum")
		if normalizePluralTypes(value) {
			*rewritten++
		}
		for key, child := range value {
			value[key] = normalizeNode(child, rewritten)
		}
		return value
	case []any:
		for i, child := range value {
			value[i] = normalizeNode(child, rewritten)
		}
		return value
	default:
		return node
	}
}

// normalizeExclusiveBound converts a 2020-12 exclusive bound into the 3.0 pair.
//
// In 2020-12 the keyword carries the bound itself ("exclusiveMinimum": 0 means
// x > 0); in 3.0 it is a flag qualifying minimum. The rewrite is exact.
//
// An existing inclusive bound is not overwritten. A document carrying both is
// contradictory, and the exclusive one is the narrower claim only sometimes;
// leaving the document's own value in place keeps the outcome predictable
// instead of resolving a contradiction silently.
func normalizeExclusiveBound(schema map[string]any, exclusiveKey, inclusiveKey string) {
	bound, present := schema[exclusiveKey]
	if !present {
		return
	}
	number, isNumber := bound.(float64)
	if !isNumber {
		// Already the 3.0 boolean form.
		return
	}
	if _, taken := schema[inclusiveKey]; !taken {
		schema[inclusiveKey] = number
	}
	schema[exclusiveKey] = true
}

// normalizePluralTypes rewrites a `types` array into `type`.
//
// `types` is not an OpenAPI or JSON Schema keyword. It is a field of
// swagger-core's Java schema model, and it appears in documents where that model
// was serialised directly instead of being written out as a specification —
// alongside its sibling `exampleSetFlag`. One real 425 KiB document declared 220
// of its 854 properties this way, so a conformant parser reads a quarter of it as
// untyped, and every one of those fields reaches a caller with no type.
//
// The rewrite is unambiguous: the values mean what `type` means. It is applied
// rather than refused because the alternative is a quarter of a document silently
// losing its types, and the count is reported so the document can be fixed at its
// source instead of being accommodated forever.
func normalizePluralTypes(schema map[string]any) bool {
	raw, present := schema["types"]
	if !present {
		return false
	}
	if _, taken := schema["type"]; taken {
		// The document says both. Its own `type` is the conformant one.
		delete(schema, "types")
		return false
	}

	switch values := raw.(type) {
	case []any:
		names := make([]any, 0, len(values))
		for _, value := range values {
			if name, ok := value.(string); ok && name != "" {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return false
		}
		if len(names) == 1 {
			schema["type"] = names[0]
		} else {
			schema["type"] = names
		}
	case string:
		schema["type"] = values
	default:
		return false
	}
	delete(schema, "types")
	return true
}

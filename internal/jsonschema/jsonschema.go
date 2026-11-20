// Package jsonschema reads the JSON schemas Ghost Wispr asks models to
// answer in.
package jsonschema

// Strict says whether OpenAI's strict structured output takes schema: every
// object, at any depth, lists each of its properties in required and sets
// additionalProperties false. A map, an object whose additionalProperties is
// a schema, is beyond strict mode: OpenAI, and a gateway in front of it,
// refuse it with HTTP 400. A client asks for strict output only when Strict
// holds.
func Strict(schema map[string]any) bool {
	if schema == nil {
		return true
	}
	properties, _ := schema["properties"].(map[string]any)
	if schema["type"] == "object" || properties != nil {
		if extra, ok := schema["additionalProperties"].(bool); !ok || extra {
			return false
		}
		required := names(schema["required"])
		for name, property := range properties {
			if !required[name] {
				return false
			}
			if sub, ok := property.(map[string]any); ok && !Strict(sub) {
				return false
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok && !Strict(items) {
		return false
	}
	for _, key := range []string{"anyOf", "allOf", "oneOf"} {
		branches, _ := schema[key].([]any)
		for _, branch := range branches {
			if sub, ok := branch.(map[string]any); ok && !Strict(sub) {
				return false
			}
		}
	}
	for _, key := range []string{"$defs", "definitions"} {
		defs, _ := schema[key].(map[string]any)
		for _, def := range defs {
			if sub, ok := def.(map[string]any); ok && !Strict(sub) {
				return false
			}
		}
	}
	return true
}

// names is a required list, as Go builds one ([]string) or JSON decodes one
// ([]any), as a set.
func names(list any) map[string]bool {
	set := map[string]bool{}
	switch list := list.(type) {
	case []string:
		for _, name := range list {
			set[name] = true
		}
	case []any:
		for _, name := range list {
			if s, ok := name.(string); ok {
				set[s] = true
			}
		}
	}
	return set
}

package jsonschema

import (
	"encoding/json"
	"testing"
)

// TestStrictAdmitsOnlySchemasStrictModeTakes fails if a schema OpenAI's
// strict structured output refuses reads as strict (an object whose
// additionalProperties is a schema, as a map is; one that leaves a property
// out of required; one with no additionalProperties false, at any depth), or
// if a schema it takes reads as not strict.
func TestStrictAdmitsOnlySchemasStrictModeTakes(t *testing.T) {
	strictPerson := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"name": map[string]any{"type": "string"}},
		"required":             []string{"name"},
		"additionalProperties": false,
	}
	for name, c := range map[string]struct {
		schema string
		strict bool
	}{
		"flat, every property required":           {`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["b","a"],"additionalProperties":false}`, true},
		"a property left out of required":         {`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"],"additionalProperties":false}`, false},
		"no additionalProperties":                 {`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`, false},
		"a map: additionalProperties is a schema": {`{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"type":"string"}}},"required":["m"],"additionalProperties":false}`, false},
		"a nested object not strict":              {`{"type":"object","properties":{"o":{"type":"object","properties":{"x":{"type":"string"}}}},"required":["o"],"additionalProperties":false}`, false},
		"an array of strict objects":              {`{"type":"object","properties":{"l":{"type":"array","items":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}}},"required":["l"],"additionalProperties":false}`, true},
		"an array of loose objects":               {`{"type":"object","properties":{"l":{"type":"array","items":{"type":"object","properties":{"x":{"type":"string"}}}}},"required":["l"],"additionalProperties":false}`, false},
		"anyOf with a loose branch":               {`{"type":"object","properties":{"u":{"anyOf":[{"type":"string"},{"type":"object","properties":{"x":{"type":"string"}}}]}},"required":["u"],"additionalProperties":false}`, false},
		"a loose definition":                      {`{"type":"object","properties":{"r":{"$ref":"#/$defs/d"}},"required":["r"],"additionalProperties":false,"$defs":{"d":{"type":"object","properties":{"x":{"type":"string"}}}}}`, false},
	} {
		var schema map[string]any
		if err := json.Unmarshal([]byte(c.schema), &schema); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := Strict(schema); got != c.strict {
			t.Errorf("%s: Strict = %v, want %v", name, got, c.strict)
		}
	}
	// A schema built in Go, whose required is a []string, reads the same.
	if !Strict(strictPerson) {
		t.Error("a strict schema built in Go, with required a []string, reads as not strict")
	}
}

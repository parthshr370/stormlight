// Package schema builds the small JSON Schemas that describe tool parameters.
// It replaces hand-written raw JSON strings with a typed builder, so a schema
// lives next to its tool and its decode struct and there is no JSON string to
// escape by hand. The output is a plain [json.RawMessage], byte-compatible with
// what the model APIs expect.
package schema

import "encoding/json"

// JSON is a JSON Schema fragment. Nest it to describe objects, arrays, enums,
// and defaults, for example schema.JSON{"type": "string", "enum": []string{"a", "b"}}.
type JSON map[string]any

// Object marshals an object schema from its properties and an optional required
// list. The "required" key is added only when at least one name is passed,
// matching the mix of tools that omit it entirely. Empty properties still
// marshal as {} (never null), so a bare object schema round-trips.
func Object(props JSON, required ...string) json.RawMessage {
	if props == nil {
		props = JSON{}
	}
	doc := JSON{"type": "object", "properties": props}
	if len(required) > 0 {
		doc["required"] = required
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

// ObjectRequired is like [Object] but always emits the "required" key, even
// when req is empty. Use it to preserve a tool that historically advertised
// "required":[] on the wire (an explicit "nothing is required"), so migrating
// it to the builder is a structural no-op (same decoded schema; map marshaling
// may reorder keys) rather than dropping the key.
func ObjectRequired(props JSON, req []string) json.RawMessage {
	if props == nil {
		props = JSON{}
	}
	if req == nil {
		req = []string{}
	}
	doc := JSON{"type": "object", "properties": props, "required": req}
	b, err := json.Marshal(doc)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

// String returns a string-typed leaf schema.
func String() JSON { return JSON{"type": "string"} }

// Number returns a number-typed leaf schema.
func Number() JSON { return JSON{"type": "number"} }

// Bool returns a boolean-typed leaf schema.
func Bool() JSON { return JSON{"type": "boolean"} }

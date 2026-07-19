package validate

import (
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func toolWithSchema(schema string) types.Tool {
	return types.Tool{Name: "example", Description: "example tool", Parameters: raw(schema)}
}

func toolCall(name, args string) types.ContentBlock {
	return types.ContentBlock{Type: types.BlockToolCall, ID: "tc", Name: name, Arguments: raw(args)}
}

func decodeResult(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return got
}

func TestValidateToolArgumentsValidArgsPassThrough(t *testing.T) {
	tool := toolWithSchema(`{"type":"object","properties":{"count":{"type":"number"}},"required":["count"]}`)
	gotRaw, err := ValidateToolArguments(tool, toolCall("example", `{"count":3}`))
	if err != nil {
		t.Fatalf("ValidateToolArguments error: %v", err)
	}
	got := decodeResult(t, gotRaw)
	if got["count"] != float64(3) {
		t.Fatalf("count = %#v, want 3", got["count"])
	}
}

func TestValidateToolArgumentsCoercesPrimitives(t *testing.T) {
	tool := toolWithSchema(`{"type":"object","properties":{"count":{"type":"number"},"enabled":{"type":"boolean"}},"required":["count","enabled"]}`)
	gotRaw, err := ValidateToolArguments(tool, toolCall("example", `{"count":"5","enabled":"true"}`))
	if err != nil {
		t.Fatalf("ValidateToolArguments error: %v", err)
	}
	got := decodeResult(t, gotRaw)
	if got["count"] != float64(5) || got["enabled"] != true {
		t.Fatalf("coerced args = %#v", got)
	}
}

func TestValidateToolArgumentsCoercesNestedObjectProperties(t *testing.T) {
	tool := toolWithSchema(`{"type":"object","properties":{"nested":{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}},"required":["nested"]}`)
	gotRaw, err := ValidateToolArguments(tool, toolCall("example", `{"nested":{"count":"7"}}`))
	if err != nil {
		t.Fatalf("ValidateToolArguments error: %v", err)
	}
	got := decodeResult(t, gotRaw)
	nested := got["nested"].(map[string]any)
	if nested["count"] != float64(7) {
		t.Fatalf("nested count = %#v, want 7", nested["count"])
	}
}

func TestValidateToolArgumentsCoercesArrayItems(t *testing.T) {
	tool := toolWithSchema(`{"type":"object","properties":{"values":{"type":"array","items":{"type":"number"}}},"required":["values"]}`)
	gotRaw, err := ValidateToolArguments(tool, toolCall("example", `{"values":["1","2"]}`))
	if err != nil {
		t.Fatalf("ValidateToolArguments error: %v", err)
	}
	got := decodeResult(t, gotRaw)
	values := got["values"].([]any)
	if values[0] != float64(1) || values[1] != float64(2) {
		t.Fatalf("values = %#v, want [1 2]", values)
	}
}

func TestValidateToolArgumentsUnionSchemasPickValidatingBranch(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		args   string
		key    string
		want   any
	}{
		{
			name:   "anyOf",
			schema: `{"anyOf":[{"type":"object","properties":{"count":{"type":"number"}},"required":["count"]},{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}]}`,
			args:   `{"count":"5"}`,
			key:    "count",
			want:   float64(5),
		},
		{
			name:   "oneOf",
			schema: `{"oneOf":[{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]},{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}]}`,
			args:   `{"enabled":"true"}`,
			key:    "enabled",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRaw, err := ValidateToolArguments(toolWithSchema(tt.schema), toolCall("example", tt.args))
			if err != nil {
				t.Fatalf("ValidateToolArguments error: %v", err)
			}
			got := decodeResult(t, gotRaw)
			if got[tt.key] != tt.want {
				t.Fatalf("%s = %#v, want %#v", tt.key, got[tt.key], tt.want)
			}
		})
	}
}

func TestValidateToolArgumentsMissingRequiredFormatsError(t *testing.T) {
	tool := toolWithSchema(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	_, err := ValidateToolArguments(tool, toolCall("example", `{}`))
	if err == nil {
		t.Fatal("expected validation error")
	}
	message := err.Error()
	for _, want := range []string{`Validation failed for tool "example":`, "  - root: ", "Received arguments:", "{}"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not contain %q", message, want)
		}
	}
}

func TestValidateToolCallUnknownTool(t *testing.T) {
	_, err := ValidateToolCall([]types.Tool{toolWithSchema(`{"type":"object"}`)}, toolCall("missing", `{}`))
	if err == nil || err.Error() != `Tool "missing" not found` {
		t.Fatalf("error = %v, want unknown tool", err)
	}
}

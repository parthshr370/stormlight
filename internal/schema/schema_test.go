package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

func parse(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return v
}

func TestObjectRequiredOmittedWhenEmpty(t *testing.T) {
	got := parse(t, Object(JSON{"path": String()}))
	want := parse(t, json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestObjectRequiredPresent(t *testing.T) {
	got := parse(t, Object(JSON{"path": String(), "n": Number()}, "path"))
	want := parse(t, json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"n":{"type":"number"}},"required":["path"]}`))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestObjectEmptyPropsMarshalsObject(t *testing.T) {
	got := parse(t, Object(nil))
	want := parse(t, json.RawMessage(`{"type":"object","properties":{}}`))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestLeafHelpers(t *testing.T) {
	if String()["type"] != "string" || Number()["type"] != "number" || Bool()["type"] != "boolean" {
		t.Fatal("leaf helper type mismatch")
	}
}

func TestNestedObjectAndEnum(t *testing.T) {
	got := parse(t, Object(JSON{
		"items": JSON{"type": "array", "items": Object(nestedProps())},
		"op":    JSON{"type": "string", "enum": []string{"a", "b"}},
	}, "op"))
	want := parse(t, json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"k":{"type":"string"}}}},"op":{"type":"string","enum":["a","b"]}},"required":["op"]}`))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nested got %v want %v", got, want)
	}
}

func nestedProps() JSON { return JSON{"k": String()} }

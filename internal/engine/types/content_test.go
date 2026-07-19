package types

import (
	"encoding/json"
	"testing"
)

// roundTrip marshals v and unmarshals into a fresh ContentBlock, asserting the
// JSON survives a round-trip byte-for-byte.
func roundTrip(t *testing.T, b ContentBlock) ContentBlock {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ContentBlock
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return got
}

func assertReferenceFields(t *testing.T, got, want ContentBlock) {
	t.Helper()
	if got.Type != want.Type ||
		got.RefStore != want.RefStore ||
		got.RefKey != want.RefKey ||
		got.RefMediaType != want.RefMediaType ||
		got.RefFilename != want.RefFilename ||
		got.RefSizeBytes != want.RefSizeBytes ||
		got.RefPageCount != want.RefPageCount {
		t.Errorf("reference fields = %#v, want %#v", got, want)
	}
}

func TestNewDocumentRef(t *testing.T) {
	want := ContentBlock{
		Type:         BlockDocumentRef,
		RefStore:     "session-local",
		RefKey:       "sha256/ab/cd.pdf",
		RefMediaType: "application/pdf",
		RefFilename:  "report.pdf",
		RefSizeBytes: 1234,
		RefPageCount: 7,
	}
	assertReferenceFields(t, NewDocumentRef(
		want.RefStore,
		want.RefKey,
		want.RefMediaType,
		want.RefFilename,
		want.RefSizeBytes,
		want.RefPageCount,
	), want)
}

func TestNewImageRef(t *testing.T) {
	want := ContentBlock{
		Type:         BlockImageRef,
		RefStore:     "session-local",
		RefKey:       "sha256/ef/gh.png",
		RefMediaType: "image/png",
		RefFilename:  "chart.png",
		RefSizeBytes: 5678,
	}
	assertReferenceFields(t, NewImageRef(
		want.RefStore,
		want.RefKey,
		want.RefMediaType,
		want.RefFilename,
		want.RefSizeBytes,
	), want)
}

func TestReferenceBlockJSONIsByteFree(t *testing.T) {
	cases := []struct {
		name  string
		block ContentBlock
	}{
		{
			name:  "document",
			block: NewDocumentRef("session-local", "sha256/ab/cd.pdf", "application/pdf", "report.pdf", 1234, 7),
		},
		{
			name:  "image",
			block: NewImageRef("session-local", "sha256/ef/gh.png", "image/png", "chart.png", 5678),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.block)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("unmarshal fields: %v", err)
			}
			for _, unwanted := range []string{"data", "text", "base64"} {
				if _, ok := fields[unwanted]; ok {
					t.Errorf("reference block leaked field %q: %s", unwanted, raw)
				}
			}

			var got ContentBlock
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal block: %v", err)
			}
			assertReferenceFields(t, got, tc.block)
		})
	}
}

func TestContentBlockRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		block ContentBlock
	}{
		{"text", NewText("hello")},
		{"thinking", NewThinking("reasoning...", "sig-abc")},
		{"image", NewImage("aGk=", "image/png")},
		{"toolcall", NewToolCall("call_1", "read", json.RawMessage(`{"path":"a.txt"}`))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.block)
			if got.Type != tc.block.Type {
				t.Errorf("Type = %q, want %q", got.Type, tc.block.Type)
			}
			a, _ := json.Marshal(tc.block)
			b, _ := json.Marshal(got)
			if string(a) != string(b) {
				t.Errorf("round-trip mismatch:\n in:  %s\n out: %s", a, b)
			}
		})
	}
}

func TestContentBlockOmitsUnsetFields(t *testing.T) {
	raw, _ := json.Marshal(NewText("hi"))
	// A text block must not serialize thinking/image/toolCall fields.
	for _, unwanted := range []string{"thinking", "data", "mimeType", "id", "arguments"} {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if _, ok := m[unwanted]; ok {
			t.Errorf("text block leaked field %q: %s", unwanted, raw)
		}
	}
}

func TestToolCallArgumentsPreserved(t *testing.T) {
	args := json.RawMessage(`{"path":"x","offset":5}`)
	got := roundTrip(t, NewToolCall("id1", "read", args))
	if string(got.Arguments) != string(args) {
		t.Errorf("Arguments = %s, want %s", got.Arguments, args)
	}
}

func TestToolCallArgumentsRequiredWhenEmpty(t *testing.T) {
	raw, err := json.Marshal(NewToolCall("id1", "read", nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["arguments"]) != `{}` {
		t.Fatalf("arguments = %s, want {} in %s", got["arguments"], raw)
	}
}

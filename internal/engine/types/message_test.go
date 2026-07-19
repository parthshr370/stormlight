package types

import (
	"encoding/json"
	"testing"
)

func TestUserContentUnionRoundTrip(t *testing.T) {
	t.Run("string form", func(t *testing.T) {
		raw, _ := json.Marshal(StringContent("hi there"))
		if string(raw) != `"hi there"` {
			t.Fatalf("string content marshaled as %s", raw)
		}
		var got UserContent
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.IsBlocks() || got.Text != "hi there" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("block form", func(t *testing.T) {
		in := BlockContent(NewText("a"), NewImage("Zg==", "image/png"))
		raw, _ := json.Marshal(in)
		var got UserContent
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !got.IsBlocks() || len(got.Blocks) != 2 {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestUnmarshalMessageByRole(t *testing.T) {
	msgs := []Message{
		UserMessage{Content: StringContent("build me an app"), Timestamp: 1},
		AssistantMessage{Content: []ContentBlock{NewText("ok"), NewToolCall("c1", "write", json.RawMessage(`{}`))}, StopReason: StopToolUse},
		ToolResultMessage{ToolCallID: "c1", ToolName: "write", Content: []ContentBlock{NewText("done")}},
	}
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal %s: %v", m.Role(), err)
		}
		got, err := UnmarshalMessage(raw)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if got.Role() != m.Role() {
			t.Errorf("role = %q, want %q (%s)", got.Role(), m.Role(), raw)
		}
	}
}

func TestUnmarshalMessageUnknownRole(t *testing.T) {
	if _, err := UnmarshalMessage([]byte(`{"role":"system","content":"x"}`)); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestAssistantMessageCarriesRole(t *testing.T) {
	raw, _ := json.Marshal(AssistantMessage{Content: []ContentBlock{NewText("hi")}})
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["role"] != "assistant" {
		t.Fatalf("role field = %v, want assistant (%s)", m["role"], raw)
	}
}

func TestRequiredMessageFieldsAreNotOmitted(t *testing.T) {
	assistantRaw, err := json.Marshal(AssistantMessage{Content: []ContentBlock{NewText("")}})
	if err != nil {
		t.Fatalf("assistant marshal: %v", err)
	}
	var assistant map[string]any
	if err := json.Unmarshal(assistantRaw, &assistant); err != nil {
		t.Fatalf("assistant unmarshal: %v", err)
	}
	for _, key := range []string{"api", "provider", "model", "stopReason", "timestamp"} {
		if _, ok := assistant[key]; !ok {
			t.Fatalf("assistant missing required field %q: %s", key, assistantRaw)
		}
	}

	toolRaw, err := json.Marshal(ToolResultMessage{ToolCallID: "tc1", ToolName: "read", Content: []ContentBlock{NewText("ok")}})
	if err != nil {
		t.Fatalf("tool result marshal: %v", err)
	}
	var toolResult map[string]any
	if err := json.Unmarshal(toolRaw, &toolResult); err != nil {
		t.Fatalf("tool result unmarshal: %v", err)
	}
	if value, ok := toolResult["isError"]; !ok || value != false {
		t.Fatalf("isError = %v (present %v), want explicit false in %s", value, ok, toolRaw)
	}
	if _, ok := toolResult["timestamp"]; !ok {
		t.Fatalf("tool result missing timestamp: %s", toolRaw)
	}
}

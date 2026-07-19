package types

import (
	"encoding/json"
	"testing"
)

// TestFoldReducer feeds a scripted event sequence (one text block streamed then
// finalized, one tool call streamed as JSON deltas then finalized with the
// assembled call) and asserts the accumulated AssistantMessage.
func TestFoldReducer(t *testing.T) {
	b := NewAssistantBuilder("anthropic-messages", "anthropic", "claude-x")
	events := []StreamEvent{
		{Type: EvStart},
		{Type: EvTextStart, ContentIndex: 0},
		{Type: EvTextDelta, ContentIndex: 0, Delta: "Hel"},
		{Type: EvTextDelta, ContentIndex: 0, Delta: "lo"},
		{Type: EvTextEnd, ContentIndex: 0, Content: "Hello"},
		{Type: EvToolCallStart, ContentIndex: 1, ToolCall: &ContentBlock{ID: "c1", Name: "read"}},
		{Type: EvToolCallDelta, ContentIndex: 1, Delta: `{"path":`},
		{Type: EvToolCallDelta, ContentIndex: 1, Delta: `"a.txt"}`},
		{Type: EvToolCallEnd, ContentIndex: 1, ToolCall: &ContentBlock{
			Type: BlockToolCall, ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.txt"}`),
		}},
		{Type: EvDone, Reason: StopToolUse},
	}
	for _, ev := range events {
		b.Fold(ev)
	}
	msg := b.Message()

	if msg.Model != "claude-x" || msg.Provider != "anthropic" {
		t.Fatalf("provenance lost: %+v", msg)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("want 2 blocks, got %d: %+v", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Type != BlockText || msg.Content[0].Text != "Hello" {
		t.Errorf("block0 = %+v, want text 'Hello'", msg.Content[0])
	}
	tc := msg.Content[1]
	if tc.Type != BlockToolCall || tc.ID != "c1" || tc.Name != "read" {
		t.Errorf("block1 header = %+v", tc)
	}
	if string(tc.Arguments) != `{"path":"a.txt"}` {
		t.Errorf("tool args = %s, want {\"path\":\"a.txt\"}", tc.Arguments)
	}
	if msg.StopReason != StopToolUse {
		t.Errorf("stopReason = %q, want toolUse", msg.StopReason)
	}
}

func TestFoldError(t *testing.T) {
	b := NewAssistantBuilder("", "", "")
	b.Fold(StreamEvent{Type: EvError, Reason: StopError, Err: &AssistantMessage{ErrorMessage: "boom"}})
	msg := b.Message()
	if msg.StopReason != StopError || msg.ErrorMessage != "boom" {
		t.Fatalf("error not recorded: %+v", msg)
	}
}

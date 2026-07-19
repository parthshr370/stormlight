package estimate

import (
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestEstimateTextTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"😀😀😀😀😀", 3}, // JavaScript string.length counts each emoji as two UTF-16 code units.
	}
	for _, tc := range cases {
		if got := EstimateTextTokens(tc.text); got != tc.want {
			t.Fatalf("EstimateTextTokens(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestCalculateContextTokens(t *testing.T) {
	withTotal := types.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, TotalTokens: 99}
	if got := CalculateContextTokens(withTotal); got != 99 {
		t.Fatalf("CalculateContextTokens(total) = %d, want 99", got)
	}

	withoutTotal := types.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}
	if got := CalculateContextTokens(withoutTotal); got != 10 {
		t.Fatalf("CalculateContextTokens(sum) = %d, want 10", got)
	}
}

func TestEstimateMessageTokens(t *testing.T) {
	userText := types.UserMessage{Content: types.StringContent("abcde")}
	if got := EstimateMessageTokens(userText); got != 2 {
		t.Fatalf("EstimateMessageTokens(user text) = %d, want 2", got)
	}

	userBlocks := types.UserMessage{Content: types.BlockContent(types.NewText("abcd"), types.NewImage("data", "image/png"))}
	if got := EstimateMessageTokens(userBlocks); got != 1201 {
		t.Fatalf("EstimateMessageTokens(user blocks) = %d, want 1201", got)
	}

	toolResult := types.ToolResultMessage{Content: []types.ContentBlock{types.NewText("abcde")}}
	if got := EstimateMessageTokens(toolResult); got != 2 {
		t.Fatalf("EstimateMessageTokens(tool result) = %d, want 2", got)
	}

	assistant := types.AssistantMessage{Content: []types.ContentBlock{
		types.NewText("abcd"),
		types.NewThinking("efgh", ""),
		types.NewToolCall("call_1", "tool", json.RawMessage(`{"a":1}`)),
	}}
	// chars = 4 text + 4 thinking + len("tool") + len(`{"a":1}`) = 19; ceil(19/4)=5.
	if got := EstimateMessageTokens(assistant); got != 5 {
		t.Fatalf("EstimateMessageTokens(assistant) = %d, want 5", got)
	}
}

func TestEstimateMessagesUsesLastValidAssistantUsage(t *testing.T) {
	messages := []types.Message{
		types.AssistantMessage{Usage: types.Usage{Input: 1, Output: 1}, StopReason: types.StopAborted},
		types.AssistantMessage{Usage: types.Usage{TotalTokens: 10}, StopReason: types.StopStop},
		types.UserMessage{Content: types.StringContent("abcde")},
		types.AssistantMessage{Usage: types.Usage{TotalTokens: 30}, StopReason: types.StopError},
		types.ToolResultMessage{Content: []types.ContentBlock{types.NewText("abcd")}},
	}

	got := EstimateMessages(messages)
	if got.Tokens != 13 || got.UsageTokens != 10 || got.TrailingTokens != 3 {
		t.Fatalf("EstimateMessages() = %+v, want tokens=13 usage=10 trailing=3", got)
	}
	if got.LastUsageIndex == nil || *got.LastUsageIndex != 1 {
		t.Fatalf("LastUsageIndex = %v, want 1", got.LastUsageIndex)
	}
}

func TestEstimateMessagesNoUsage(t *testing.T) {
	messages := []types.Message{
		types.UserMessage{Content: types.StringContent("abcde")},
		types.ToolResultMessage{Content: []types.ContentBlock{types.NewText("abcd")}},
	}
	got := EstimateMessages(messages)
	if got.Tokens != 3 || got.UsageTokens != 0 || got.TrailingTokens != 3 {
		t.Fatalf("EstimateMessages(no usage) = %+v, want tokens=3 usage=0 trailing=3", got)
	}
	if got.LastUsageIndex != nil {
		t.Fatalf("LastUsageIndex = %v, want nil", got.LastUsageIndex)
	}
}

func TestEstimateContextTokensAddsPrefixOnlyWithoutUsage(t *testing.T) {
	ctx := types.Context{
		SystemPrompt: "abcde",
		Messages:     []types.Message{types.UserMessage{Content: types.StringContent("abcd")}},
		Tools:        []types.Tool{{Name: "read", Description: "Read", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}

	got := EstimateContextTokens(ctx)
	wantPrefix := EstimateTextTokens("abcde") + EstimateTextTokens(safeJSONStringify(ctx.Tools))
	if got.Tokens != 1+wantPrefix || got.TrailingTokens != 1+wantPrefix || got.UsageTokens != 0 || got.LastUsageIndex != nil {
		t.Fatalf("EstimateContextTokens(no usage) = %+v, want message+prefix %d", got, 1+wantPrefix)
	}

	ctx.Messages = []types.Message{types.AssistantMessage{Usage: types.Usage{TotalTokens: 20}, StopReason: types.StopStop}}
	anchored := EstimateContextTokens(ctx)
	if anchored.Tokens != 20 || anchored.TrailingTokens != 0 || anchored.UsageTokens != 20 {
		t.Fatalf("EstimateContextTokens(with usage) = %+v, want no prefix", anchored)
	}
}

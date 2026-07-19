package transform

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func targetModel(input ...string) types.Model {
	return types.Model{ID: "target", API: "api", Provider: "provider", Input: input}
}

func TestTransformMessagesDowngradesUnsupportedImages(t *testing.T) {
	messages := []types.Message{
		types.UserMessage{Content: types.BlockContent(
			types.NewText("before"),
			types.NewImage("a", "image/png"),
			types.NewImage("b", "image/png"),
			types.NewText("after"),
		)},
		types.ToolResultMessage{ToolCallID: "tc", ToolName: "read", Content: []types.ContentBlock{
			types.NewImage("a", "image/png"),
			types.NewImage("b", "image/png"),
		}},
	}

	got := TransformMessages(messages, targetModel(), nil)
	user := got[0].(types.UserMessage)
	if want := []types.ContentBlock{types.NewText("before"), types.NewText(nonVisionUserImagePlaceholder), types.NewText("after")}; !sameBlocks(user.Content.Blocks, want) {
		t.Fatalf("user blocks = %#v, want %#v", user.Content.Blocks, want)
	}
	toolResult := got[1].(types.ToolResultMessage)
	if want := []types.ContentBlock{types.NewText(nonVisionToolImagePlaceholder)}; !sameBlocks(toolResult.Content, want) {
		t.Fatalf("tool result blocks = %#v, want %#v", toolResult.Content, want)
	}

	vision := TransformMessages(messages, targetModel("image"), nil)
	visionUser := vision[0].(types.UserMessage)
	if visionUser.Content.Blocks[1].Type != types.BlockImage || visionUser.Content.Blocks[2].Type != types.BlockImage {
		t.Fatalf("vision model should keep image blocks: %#v", visionUser.Content.Blocks)
	}
}

func TestTransformMessagesThinkingByModel(t *testing.T) {
	crossModel := types.AssistantMessage{
		API:      "old-api",
		Provider: "old-provider",
		Model:    "old-model",
		Content: []types.ContentBlock{
			{Type: types.BlockThinking, Thinking: "encrypted", Redacted: true},
			{Type: types.BlockThinking, Thinking: "useful reasoning"},
			{Type: types.BlockThinking, Thinking: "   "},
			{Type: types.BlockText, Text: "hello", TextSignature: "drop-me"},
			{Type: types.BlockToolCall, ID: "tc", Name: "tool", Arguments: json.RawMessage(`{}`), ThoughtSignature: "drop-me"},
		},
	}

	got := TransformMessages([]types.Message{crossModel, types.ToolResultMessage{ToolCallID: "tc", ToolName: "tool", Content: []types.ContentBlock{types.NewText("ok")}}}, targetModel(), nil)
	assistant := got[0].(types.AssistantMessage)
	if len(assistant.Content) != 3 {
		t.Fatalf("cross-model content length = %d, want 3: %#v", len(assistant.Content), assistant.Content)
	}
	if !sameBlock(assistant.Content[0], types.NewText("useful reasoning")) {
		t.Fatalf("thinking converted to text = %#v", assistant.Content[0])
	}
	if !sameBlock(assistant.Content[1], types.NewText("hello")) {
		t.Fatalf("text block was not rebuilt cleanly: %#v", assistant.Content[1])
	}
	if assistant.Content[2].ThoughtSignature != "" {
		t.Fatalf("cross-model thought signature should be stripped: %#v", assistant.Content[2])
	}

	sameModel := types.AssistantMessage{
		API:      "api",
		Provider: "provider",
		Model:    "target",
		Content: []types.ContentBlock{
			{Type: types.BlockThinking, Thinking: "", ThinkingSignature: "sig"},
			{Type: types.BlockThinking, Thinking: "encrypted", Redacted: true},
		},
	}
	got = TransformMessages([]types.Message{sameModel}, targetModel(), nil)
	assistant = got[0].(types.AssistantMessage)
	if len(assistant.Content) != 2 || assistant.Content[0].ThinkingSignature != "sig" || !assistant.Content[1].Redacted {
		t.Fatalf("same-model thinking blocks not preserved: %#v", assistant.Content)
	}
}

func TestTransformMessagesNormalizesToolCallIDAndResult(t *testing.T) {
	messages := []types.Message{
		types.AssistantMessage{API: "old-api", Provider: "old-provider", Model: "old", Content: []types.ContentBlock{
			{Type: types.BlockToolCall, ID: "bad|id", Name: "tool", Arguments: json.RawMessage(`{}`)},
		}},
		types.ToolResultMessage{ToolCallID: "bad|id", ToolName: "tool", Content: []types.ContentBlock{types.NewText("ok")}},
	}

	got := TransformMessages(messages, targetModel(), func(id string, model types.Model, source types.AssistantMessage) string {
		if id != "bad|id" || model.ID != "target" || source.Model != "old" {
			t.Fatalf("normalize args = %q, %#v, %#v", id, model, source)
		}
		return "normalized"
	})
	assistant := got[0].(types.AssistantMessage)
	if assistant.Content[0].ID != "normalized" {
		t.Fatalf("tool call ID = %q, want normalized", assistant.Content[0].ID)
	}
	toolResult := got[1].(types.ToolResultMessage)
	if toolResult.ToolCallID != "normalized" {
		t.Fatalf("tool result ID = %q, want normalized", toolResult.ToolCallID)
	}
}

func TestTransformMessagesSynthesizesOrphanToolResults(t *testing.T) {
	assistant := types.AssistantMessage{API: "api", Provider: "provider", Model: "target", Content: []types.ContentBlock{
		{Type: types.BlockToolCall, ID: "tc", Name: "tool", Arguments: json.RawMessage(`{}`)},
	}}
	user := types.UserMessage{Content: types.StringContent("next")}

	got := TransformMessages([]types.Message{assistant, user}, targetModel(), nil)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %#v", len(got), got)
	}
	synthetic := got[1].(types.ToolResultMessage)
	if synthetic.ToolCallID != "tc" || synthetic.ToolName != "tool" || !synthetic.IsError || synthetic.Content[0].Text != "No result provided" {
		t.Fatalf("synthetic result = %#v", synthetic)
	}
	if _, ok := got[2].(types.UserMessage); !ok {
		t.Fatalf("user message should follow synthetic result: %#v", got[2])
	}

	got = TransformMessages([]types.Message{assistant}, targetModel(), nil)
	if len(got) != 2 {
		t.Fatalf("conversation-ending orphan len = %d, want 2: %#v", len(got), got)
	}
	if got[1].(types.ToolResultMessage).ToolCallID != "tc" {
		t.Fatalf("final synthetic result = %#v", got[1])
	}
}

func TestTransformMessagesSkipsErroredAndAbortedAssistants(t *testing.T) {
	messages := []types.Message{
		types.AssistantMessage{StopReason: types.StopError, Content: []types.ContentBlock{types.NewText("partial")}},
		types.AssistantMessage{StopReason: types.StopAborted, Content: []types.ContentBlock{types.NewText("partial")}},
	}

	got := TransformMessages(messages, targetModel(), nil)
	if len(got) != 0 {
		t.Fatalf("got %#v, want no replayed assistant messages", got)
	}
}

func TestTransformMessagesHandlesPointerMessages(t *testing.T) {
	assistant := &types.AssistantMessage{API: "old-api", Provider: "old-provider", Model: "old", Content: []types.ContentBlock{
		{Type: types.BlockToolCall, ID: "bad|id", Name: "tool", Arguments: json.RawMessage(`{}`), ThoughtSignature: "drop-me"},
	}}
	toolResult := &types.ToolResultMessage{ToolCallID: "bad|id", ToolName: "tool", Content: []types.ContentBlock{types.NewText("ok")}}

	got := TransformMessages([]types.Message{assistant, toolResult}, targetModel(), func(id string, model types.Model, source types.AssistantMessage) string {
		return "normalized"
	})

	gotAssistant := got[0].(types.AssistantMessage)
	if gotAssistant.Content[0].ID != "normalized" || gotAssistant.Content[0].ThoughtSignature != "" {
		t.Fatalf("assistant pointer not transformed: %#v", gotAssistant.Content[0])
	}
	gotToolResult := got[1].(types.ToolResultMessage)
	if gotToolResult.ToolCallID != "normalized" {
		t.Fatalf("tool result pointer ID = %q, want normalized", gotToolResult.ToolCallID)
	}

	aborted := &types.AssistantMessage{StopReason: types.StopAborted, Content: []types.ContentBlock{types.NewText("partial")}}
	if got := TransformMessages([]types.Message{aborted}, targetModel(), nil); len(got) != 0 {
		t.Fatalf("aborted pointer assistant replayed: %#v", got)
	}
}

func sameBlocks(a, b []types.ContentBlock) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameBlock(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameBlock(a, b types.ContentBlock) bool {
	return reflect.DeepEqual(a, b)
}

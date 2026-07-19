package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.harness.dev/harness/internal/engine/jsonrepair"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/transport/sseparse"
)

// anthropicMessageEvents is the set of Anthropic SSE event types we fold.
// Non-message events (like ping) are silently ignored.
var anthropicMessageEvents = map[string]bool{
	"message_start":       true,
	"message_delta":       true,
	"message_stop":        true,
	"content_block_start": true,
	"content_block_delta": true,
	"content_block_stop":  true,
}

// rawAnthropicEvent is a single SSE data frame. Only the fields relevant to
// the event's Type are populated; the others remain zero.
type rawAnthropicEvent struct {
	Type         string                `json:"type"`
	Message      anthropicMessage      `json:"message"`
	Index        int                   `json:"index"`
	ContentBlock anthropicContentBlock `json:"content_block"`
	Delta        anthropicDelta        `json:"delta"`
	Usage        anthropicUsage        `json:"usage"`
}

// anthropicMessage holds the message_start fields that seed our response identity and usage.
type anthropicMessage struct {
	ID    string         `json:"id"`
	Usage anthropicUsage `json:"usage"`
}

// anthropicContentBlock covers every block shape Anthropic can introduce before its deltas arrive.
type anthropicContentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	Data  string          `json:"data"`
}

// anthropicDelta keeps the mutually exclusive SSE delta payloads together until the block type tells us which one matters.
type anthropicDelta struct {
	Type        string       `json:"type"`
	Text        string       `json:"text"`
	Thinking    string       `json:"thinking"`
	PartialJSON string       `json:"partial_json"`
	Signature   string       `json:"signature"`
	StopReason  string       `json:"stop_reason"`
	StopDetails *stopDetails `json:"stop_details"`
}

// stopDetails carries Anthropic's human explanation when a stop reason needs more context.
type stopDetails struct {
	Explanation string `json:"explanation"`
}

// anthropicUsage preserves optional counters because each event exposes a different subset of usage.
type anthropicUsage struct {
	InputTokens              *int                      `json:"input_tokens"`
	OutputTokens             *int                      `json:"output_tokens"`
	CacheReadInputTokens     *int                      `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int                      `json:"cache_creation_input_tokens"`
	CacheCreation            *cacheCreationUsage       `json:"cache_creation"`
	OutputTokensDetails      *outputTokensDetailsUsage `json:"output_tokens_details"`
}

// cacheCreationUsage keeps the long-lived cache count separate from Anthropic's regular cache counter.
type cacheCreationUsage struct {
	Ephemeral1hInputTokens *int `json:"ephemeral_1h_input_tokens"`
}

// outputTokensDetailsUsage carries reasoning tokens without assuming every model reports them.
type outputTokensDetailsUsage struct {
	ThinkingTokens *int `json:"thinking_tokens"`
}

// foldBlock tracks one content block during stream folding. AnthropicIndex is
// the index in the raw SSE event (same for start/delta/stop of one block);
// ContentIndex is our linear position in [types.AssistantMessage.Content].
// PartialJSON accumulates tool-call JSON deltas across successive
// input_json_delta events.
type foldBlock struct {
	AnthropicIndex int
	ContentIndex   int
	PartialJSON    string
}

// foldState is the mutable accumulator carried across SSE events for one
// streaming response.
type foldState struct {
	Output  *types.AssistantMessage
	IsOAuth bool
	Tools   []types.Tool
	Blocks  []foldBlock
}

// iterateAnthropicEvents reads an SSE stream from r, ignores non-message
// events, parses and feeds each raw event to fn, and enforces the
// message_start→message_stop bracketing — an unterminated stream is an error
// (the provider gave us a start but never a stop).
func iterateAnthropicEvents(ctx context.Context, r io.Reader, onActivity func(), fn func(rawAnthropicEvent) error) error {
	if r == nil {
		return fmt.Errorf("Attempted to iterate over an Anthropic response with no body")
	}
	sawMessageStart := false
	sawMessageEnd := false

	err := sseparse.IterateSSE(ctx, r, func(event sseparse.ServerSentEvent) error {
		if onActivity != nil {
			onActivity()
		}
		if event.Event == "error" {
			return errors.New(event.Data)
		}
		if !anthropicMessageEvents[event.Event] {
			return nil
		}

		var raw rawAnthropicEvent
		if err := jsonrepair.ParseJsonWithRepair(event.Data, &raw); err != nil {
			return fmt.Errorf("Could not parse Anthropic SSE event %s: %s; data=%s; raw=%s", event.Event, err.Error(), event.Data, strings.Join(event.Raw, "\n"))
		}
		if raw.Type == "message_start" {
			sawMessageStart = true
		} else if raw.Type == "message_stop" {
			sawMessageEnd = true
		}
		return fn(raw)
	})
	if err != nil {
		return err
	}
	if sawMessageStart && !sawMessageEnd {
		return fmt.Errorf("Anthropic stream ended before message_stop")
	}
	return nil
}

// foldAnthropicEvent dispatches one raw event to the appropriate folding
// helper and returns zero or more [types.StreamEvent]s to push downstream.
func foldAnthropicEvent(event rawAnthropicEvent, state *foldState, model types.Model) ([]types.StreamEvent, error) {
	output := state.Output
	switch event.Type {
	case "message_start":
		output.ResponseID = event.Message.ID
		applyUsageStart(output, event.Message.Usage, model)
	case "content_block_start":
		return foldContentBlockStart(event, state), nil
	case "content_block_delta":
		return foldContentBlockDelta(event, state), nil
	case "content_block_stop":
		return foldContentBlockStop(event, state), nil
	case "message_delta":
		if event.Delta.StopReason != "" {
			stopReason, message, err := mapStopReason(event.Delta.StopReason, event.Delta.StopDetails)
			if err != nil {
				return nil, err
			}
			output.StopReason = stopReason
			if message != "" {
				output.ErrorMessage = message
			}
		}
		applyUsageDelta(output, event.Usage, model)
	}
	return nil, nil
}

// foldContentBlockStart creates a new content block at the end of
// [types.AssistantMessage.Content] and registers it in the foldState so
// subsequent deltas can find it by Anthropic index.
func foldContentBlockStart(event rawAnthropicEvent, state *foldState) []types.StreamEvent {
	output := state.Output
	contentIndex := len(output.Content)
	switch event.ContentBlock.Type {
	case "text":
		output.Content = append(output.Content, types.ContentBlock{Type: types.BlockText})
		state.Blocks = append(state.Blocks, foldBlock{AnthropicIndex: event.Index, ContentIndex: contentIndex})
		return []types.StreamEvent{{Type: types.EvTextStart, ContentIndex: contentIndex, Partial: output}}
	case "thinking":
		output.Content = append(output.Content, types.ContentBlock{Type: types.BlockThinking})
		state.Blocks = append(state.Blocks, foldBlock{AnthropicIndex: event.Index, ContentIndex: contentIndex})
		return []types.StreamEvent{{Type: types.EvThinkingStart, ContentIndex: contentIndex, Partial: output}}
	case "redacted_thinking":
		output.Content = append(output.Content, types.ContentBlock{Type: types.BlockThinking, Thinking: "[Reasoning redacted]", ThinkingSignature: event.ContentBlock.Data, Redacted: true})
		state.Blocks = append(state.Blocks, foldBlock{AnthropicIndex: event.Index, ContentIndex: contentIndex})
		return []types.StreamEvent{{Type: types.EvThinkingStart, ContentIndex: contentIndex, Partial: output}}
	case "tool_use":
		name := event.ContentBlock.Name
		if state.IsOAuth {
			// OAuth: Anthropic delivers Claude-Code names (Read, Write…).
			// Map back to lowercase tool names our tool registry expects.
			name = fromClaudeCodeName(name, state.Tools)
		}
		args := rawObjectOrEmpty(event.ContentBlock.Input)
		block := types.ContentBlock{Type: types.BlockToolCall, ID: event.ContentBlock.ID, Name: name, Arguments: args}
		output.Content = append(output.Content, block)
		state.Blocks = append(state.Blocks, foldBlock{AnthropicIndex: event.Index, ContentIndex: contentIndex})
		return []types.StreamEvent{{Type: types.EvToolCallStart, ContentIndex: contentIndex, ToolCall: &block, Partial: output}}
	}
	return nil
}

// foldContentBlockDelta applies a delta to the block identified by the raw
// event's index. Text/thinking deltas append to the block's text; input_json
// deltas accumulate partial JSON; signature deltas append to the thinking
// signature.
func foldContentBlockDelta(event rawAnthropicEvent, state *foldState) []types.StreamEvent {
	blockState, block := state.find(event.Index)
	if block == nil {
		return nil
	}
	switch event.Delta.Type {
	case "text_delta":
		if block.Type == types.BlockText {
			block.Text += event.Delta.Text
			return []types.StreamEvent{{Type: types.EvTextDelta, ContentIndex: blockState.ContentIndex, Delta: event.Delta.Text, Partial: state.Output}}
		}
	case "thinking_delta":
		if block.Type == types.BlockThinking {
			block.Thinking += event.Delta.Thinking
			return []types.StreamEvent{{Type: types.EvThinkingDelta, ContentIndex: blockState.ContentIndex, Delta: event.Delta.Thinking, Partial: state.Output}}
		}
	case "input_json_delta":
		if block.Type == types.BlockToolCall {
			blockState.PartialJSON += event.Delta.PartialJSON
			// Mid-stream tool args are a best-effort ParseStreamingJSON
			// (never errors, returns {} for invalid/incomplete JSON). The
			// final args are repaired at content_block_stop.
			block.Arguments = streamingJSONRaw(blockState.PartialJSON)
			return []types.StreamEvent{{Type: types.EvToolCallDelta, ContentIndex: blockState.ContentIndex, Delta: event.Delta.PartialJSON, Partial: state.Output}}
		}
	case "signature_delta":
		if block.Type == types.BlockThinking {
			block.ThinkingSignature += event.Delta.Signature
		}
	}
	return nil
}

// foldContentBlockStop finalizes a block. For tool calls it reparses the
// accumulated partial JSON (with repair this time) to produce a valid
// Arguments value for downstream consumers.
func foldContentBlockStop(event rawAnthropicEvent, state *foldState) []types.StreamEvent {
	blockState, block := state.find(event.Index)
	if block == nil {
		return nil
	}
	switch block.Type {
	case types.BlockText:
		return []types.StreamEvent{{Type: types.EvTextEnd, ContentIndex: blockState.ContentIndex, Content: block.Text, Partial: state.Output}}
	case types.BlockThinking:
		return []types.StreamEvent{{Type: types.EvThinkingEnd, ContentIndex: blockState.ContentIndex, Content: block.Thinking, Partial: state.Output}}
	case types.BlockToolCall:
		block.Arguments = finalJSONRaw(blockState.PartialJSON)
		assembled := *block
		return []types.StreamEvent{{Type: types.EvToolCallEnd, ContentIndex: blockState.ContentIndex, ToolCall: &assembled, Partial: state.Output}}
	}
	return nil
}

// find locates a block by its Anthropic event index, translating to our linear
// content index.
func (s *foldState) find(anthropicIndex int) (*foldBlock, *types.ContentBlock) {
	for i := range s.Blocks {
		if s.Blocks[i].AnthropicIndex == anthropicIndex {
			idx := s.Blocks[i].ContentIndex
			if idx >= 0 && idx < len(s.Output.Content) {
				return &s.Blocks[i], &s.Output.Content[idx]
			}
		}
	}
	return nil, nil
}

// applyUsageStart sets usage from the message_start event (initial count).
func applyUsageStart(output *types.AssistantMessage, usage anthropicUsage, model types.Model) {
	output.Usage.Input = intValue(usage.InputTokens)
	output.Usage.Output = intValue(usage.OutputTokens)
	output.Usage.CacheRead = intValue(usage.CacheReadInputTokens)
	output.Usage.CacheWrite = intValue(usage.CacheCreationInputTokens)
	cacheWrite1h := 0
	if usage.CacheCreation != nil {
		cacheWrite1h = intValue(usage.CacheCreation.Ephemeral1hInputTokens)
	}
	output.Usage.CacheWrite1h = &cacheWrite1h
	output.Usage.TotalTokens = output.Usage.Input + output.Usage.Output + output.Usage.CacheRead + output.Usage.CacheWrite
	types.CalculateCost(model.Cost, &output.Usage)
}

// applyUsageDelta updates usage from the message_delta event (cumulative).
func applyUsageDelta(output *types.AssistantMessage, usage anthropicUsage, model types.Model) {
	if usage.InputTokens != nil {
		output.Usage.Input = *usage.InputTokens
	}
	if usage.OutputTokens != nil {
		output.Usage.Output = *usage.OutputTokens
	}
	if usage.CacheReadInputTokens != nil {
		output.Usage.CacheRead = *usage.CacheReadInputTokens
	}
	if usage.CacheCreationInputTokens != nil {
		output.Usage.CacheWrite = *usage.CacheCreationInputTokens
	}
	if usage.OutputTokensDetails != nil && usage.OutputTokensDetails.ThinkingTokens != nil {
		reasoning := *usage.OutputTokensDetails.ThinkingTokens
		output.Usage.Reasoning = &reasoning
	}
	output.Usage.TotalTokens = output.Usage.Input + output.Usage.Output + output.Usage.CacheRead + output.Usage.CacheWrite
	types.CalculateCost(model.Cost, &output.Usage)
}

// intValue dereferences an *int, returning 0 when nil.
func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// streamingJSONRaw produces a best-effort streaming parse of partial tool-call
// JSON. It never errors — invalid/incomplete JSON returns {} and the real
// result comes at content_block_stop via [finalJSONRaw].
func streamingJSONRaw(partial string) json.RawMessage {
	parsed := jsonrepair.ParseStreamingJSON(partial)
	return marshalObject(parsed)
}

// finalJSONRaw parses the complete accumulated tool-call JSON with repair.
// Empty/whitespace input yields {}.
func finalJSONRaw(partial string) json.RawMessage {
	if strings.TrimSpace(partial) == "" {
		return json.RawMessage(`{}`)
	}
	var parsed map[string]any
	if err := jsonrepair.ParseJsonWithRepair(partial, &parsed); err != nil || parsed == nil {
		return json.RawMessage(`{}`)
	}
	return marshalObject(parsed)
}

// rawObjectOrEmpty normalizes a tool-use input into a non-nil JSON object.
func rawObjectOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`)
	}
	return raw
}

// marshalObject serializes value to JSON bytes, defaulting to {} on failure.
func marshalObject(value map[string]any) json.RawMessage {
	if value == nil {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/engine/transform"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/util"
)

// buildParams assembles the OpenAI chat-completions request body from the model,
// context, and options, always enabling streaming with usage.
func buildParams(model types.Model, c types.Context, opts *Options) map[string]any {
	maxTokens := model.MaxTokens
	if opts != nil && opts.MaxTokens != 0 {
		maxTokens = opts.MaxTokens
	}

	params := map[string]any{
		"model":    model.ID,
		"messages": convertMessages(model, c),
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}

	if maxTokens > 0 {
		params["max_completion_tokens"] = maxTokens
	}

	if opts != nil && opts.Temperature != nil {
		params["temperature"] = *opts.Temperature
	}

	if len(c.Tools) > 0 {
		params["tools"] = convertTools(c.Tools)
	} else if hasToolHistory(c.Messages) {
		params["tools"] = []map[string]any{}
	}

	if opts != nil && opts.ToolChoice != nil {
		params["tool_choice"] = opts.ToolChoice
	}

	if opts != nil && opts.ReasoningEffort != "" {
		params["reasoning_effort"] = opts.ReasoningEffort
	}

	return params
}

// hasToolHistory reports whether the messages contain any tool result or tool call,
// used to keep the tools field present so the model can interpret prior tool turns.
func hasToolHistory(messages []types.Message) bool {
	for _, msg := range messages {
		if _, ok := asToolResult(msg); ok {
			return true
		}
		if assistant, ok := asAssistant(msg); ok {
			for _, block := range assistant.Content {
				if block.Type == types.BlockToolCall {
					return true
				}
			}
		}
	}
	return false
}

// convertMessages transforms the context into OpenAI wire-format messages,
// prepending the system prompt and coalescing consecutive tool results.
func convertMessages(model types.Model, c types.Context) []map[string]any {
	params := []map[string]any{}

	normalizeID := func(id string, _ types.Model, _ types.AssistantMessage) string {
		return id
	}
	transformedMessages := transform.TransformMessages(c.Messages, model, normalizeID)

	if c.SystemPrompt != "" {
		params = append(params, map[string]any{
			"role":    "system",
			"content": util.SanitizeSurrogates(c.SystemPrompt),
		})
	}

	for i := 0; i < len(transformedMessages); i++ {
		msg := transformedMessages[i]

		if user, ok := asUser(msg); ok {
			if !user.Content.IsBlocks() {
				if strings.TrimSpace(user.Content.Text) > "" {
					params = append(params, map[string]any{"role": "user", "content": util.SanitizeSurrogates(user.Content.Text)})
				}
			} else {
				parts := []map[string]any{}
				for _, item := range user.Content.Blocks {
					if item.Type == types.BlockText {
						text := util.SanitizeSurrogates(item.Text)
						if strings.TrimSpace(text) != "" {
							parts = append(parts, map[string]any{"type": "text", "text": text})
						}
					} else if item.Type == types.BlockImage {
						parts = append(parts, map[string]any{
							"type":      "image_url",
							"image_url": map[string]any{"url": fmt.Sprintf("data:%s;base64,%s", item.MimeType, item.Data)},
						})
					}
				}
				if len(parts) > 0 {
					params = append(params, map[string]any{"role": "user", "content": parts})
				}
			}
			continue
		}

		if assistant, ok := asAssistant(msg); ok {
			assistantMsg := map[string]any{"role": "assistant"}
			textParts := []string{}
			for _, block := range assistant.Content {
				if block.Type == types.BlockText && strings.TrimSpace(block.Text) != "" {
					textParts = append(textParts, util.SanitizeSurrogates(block.Text))
				}
			}
			assistantText := strings.Join(textParts, "")

			toolCalls := []map[string]any{}
			for _, block := range assistant.Content {
				if block.Type == types.BlockToolCall {
					toolCalls = append(toolCalls, map[string]any{
						"id":   block.ID,
						"type": "function",
						"function": map[string]any{
							"name":      block.Name,
							"arguments": string(rawJSONOrEmpty(block.Arguments)),
						},
					})
				}
			}

			if len(assistantText) > 0 || len(toolCalls) > 0 {
				if len(assistantText) > 0 {
					assistantMsg["content"] = assistantText
				} else {
					assistantMsg["content"] = ""
				}
				if len(toolCalls) > 0 {
					assistantMsg["tool_calls"] = toolCalls
				}
				params = append(params, assistantMsg)
			}
			continue
		}

		if toolResult, ok := asToolResult(msg); ok {
			toolResults := []map[string]any{toolResultBlock(toolResult)}
			j := i + 1
			for j < len(transformedMessages) {
				nextMsg, ok := asToolResult(transformedMessages[j])
				if !ok {
					break
				}
				toolResults = append(toolResults, toolResultBlock(nextMsg))
				j++
			}
			i = j - 1
			for _, tr := range toolResults {
				params = append(params, tr)
			}
		}
	}
	return params
}

// toolResultBlock renders a tool result message as an OpenAI "tool" role message.
func toolResultBlock(msg types.ToolResultMessage) map[string]any {
	textParts := []string{}
	for _, block := range msg.Content {
		if block.Type == types.BlockText {
			textParts = append(textParts, block.Text)
		}
	}
	text := strings.Join(textParts, "\n")
	return map[string]any{
		"role":         "tool",
		"content":      util.SanitizeSurrogates(text),
		"tool_call_id": msg.ToolCallID,
	}
}

// convertTools renders tools as OpenAI function-tool definitions.
func convertTools(tools []types.Tool) []map[string]any {
	if tools == nil {
		return []map[string]any{}
	}
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  rawJSONToAny(tool.Parameters),
			},
		})
	}
	return result
}

// mapOpenAIStopReason maps an OpenAI finish_reason to a StopReason and optional
// error message, returning an error for unrecognized values.
func mapOpenAIStopReason(reason string) (types.StopReason, string, error) {
	if reason == "" {
		return types.StopStop, "", nil
	}
	switch reason {
	case "stop", "end":
		return types.StopStop, "", nil
	case "length":
		return types.StopLength, "", nil
	case "function_call", "tool_calls":
		return types.StopToolUse, "", nil
	case "content_filter":
		return types.StopError, "Provider finish_reason: content_filter", nil
	case "network_error":
		return types.StopError, "Provider finish_reason: network_error", nil
	default:
		return "", "", fmt.Errorf("unhandled stop reason: %s", reason)
	}
}

// parseChunkUsage builds a Usage from a raw usage object, subtracting cached and
// cache-write tokens from the input count and computing cost.
func parseChunkUsage(raw map[string]any, model types.Model) types.Usage {
	promptTokens := intValue(extractInt(raw, "prompt_tokens"))
	completionTokens := intValue(extractInt(raw, "completion_tokens"))

	cacheRead := 0
	cacheWrite := 0
	if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
		if v := extractInt(details, "cached_tokens"); v != nil {
			cacheRead = *v
		}
		if v := extractInt(details, "cache_write_tokens"); v != nil {
			cacheWrite = *v
		}
	}

	reasoning := 0
	if details, ok := raw["completion_tokens_details"].(map[string]any); ok {
		if v := extractInt(details, "reasoning_tokens"); v != nil {
			reasoning = *v
		}
	}

	input := maxInt(0, promptTokens-cacheRead-cacheWrite)
	usage := types.Usage{
		Input:       input,
		Output:      completionTokens,
		CacheRead:   cacheRead,
		CacheWrite:  cacheWrite,
		Reasoning:   &reasoning,
		TotalTokens: input + completionTokens + cacheRead + cacheWrite,
		Cost:        types.Cost{},
	}
	types.CalculateCost(model.Cost, &usage)
	return usage
}

// extractInt reads key from raw as an int, tolerating float64/int/*int JSON shapes,
// returning nil when absent or not numeric.
func extractInt(raw map[string]any, key string) *int {
	if v, ok := raw[key]; ok {
		switch n := v.(type) {
		case float64:
			i := int(n)
			return &i
		case int:
			return &n
		case *int:
			return n
		}
	}
	return nil
}

// intValue keeps optional usage fields from turning an incomplete provider
// response into an error.
func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// rawJSONToAny unmarshals raw into an any, falling back to an empty object on
// empty or invalid input.
func rawJSONToAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return map[string]any{}
	}
	return value
}

// rawJSONOrEmpty returns raw, or an empty JSON object for empty or null input.
func rawJSONOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`)
	}
	return raw
}

// asUser normalizes the value and pointer forms that can live in [types.Message]
// and reports whether it found a user message.
func asUser(msg types.Message) (types.UserMessage, bool) {
	switch m := msg.(type) {
	case types.UserMessage:
		return m, true
	case *types.UserMessage:
		if m != nil {
			return *m, true
		}
	}
	return types.UserMessage{}, false
}

// asAssistant normalizes the value and pointer forms that can live in [types.Message]
// and reports whether it found an assistant message.
func asAssistant(msg types.Message) (types.AssistantMessage, bool) {
	switch m := msg.(type) {
	case types.AssistantMessage:
		return m, true
	case *types.AssistantMessage:
		if m != nil {
			return *m, true
		}
	}
	return types.AssistantMessage{}, false
}

// asToolResult normalizes the value and pointer forms that can live in [types.Message]
// and reports whether it found a tool result.
func asToolResult(msg types.Message) (types.ToolResultMessage, bool) {
	switch m := msg.(type) {
	case types.ToolResultMessage:
		return m, true
	case *types.ToolResultMessage:
		if m != nil {
			return *m, true
		}
	}
	return types.ToolResultMessage{}, false
}

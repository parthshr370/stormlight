// Package transform rewrites a message transcript for a target model before
// provider conversion, handling image downgrades, thinking cleanup, tool-call
// ID normalization, and synthetic results for orphaned tool calls.
// Package transform normalizes conversation messages before a provider call:
// pass1 downgrades unsupported images, strips cross-model thinking signatures,
// and normalizes tool-call IDs; pass2 synthesizes "No result provided" for
// orphan tool calls and skips errored/aborted assistant messages.
package transform

import (
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

const (
	nonVisionUserImagePlaceholder = "(image omitted: model does not support images)"
	nonVisionToolImagePlaceholder = "(tool image omitted: model does not support images)"
)

// replaceImagesWithPlaceholder collapses adjacent losses so non-vision models get one explanation, not one per image.
func replaceImagesWithPlaceholder(content []types.ContentBlock, placeholder string) []types.ContentBlock {
	result := make([]types.ContentBlock, 0, len(content))
	previousWasPlaceholder := false

	for _, block := range content {
		if block.Type == types.BlockImage {
			if !previousWasPlaceholder {
				result = append(result, types.NewText(placeholder))
			}
			previousWasPlaceholder = true
			continue
		}

		result = append(result, block)
		previousWasPlaceholder = block.Text == placeholder
	}

	return result
}

// downgradeUnsupportedImages copies pointer-backed messages before rewriting them, so one request can't mutate retained history.
func downgradeUnsupportedImages(messages []types.Message, model types.Model) []types.Message {
	if model.SupportsVision() {
		return messages
	}

	result := make([]types.Message, 0, len(messages))
	for _, msg := range messages {
		switch m := msg.(type) {
		case types.UserMessage:
			if m.Content.IsBlocks() {
				m.Content = types.BlockContent(replaceImagesWithPlaceholder(m.Content.Blocks, nonVisionUserImagePlaceholder)...)
			}
			result = append(result, m)
		case *types.UserMessage:
			if m == nil {
				result = append(result, msg)
				continue
			}
			copy := *m
			if copy.Content.IsBlocks() {
				copy.Content = types.BlockContent(replaceImagesWithPlaceholder(copy.Content.Blocks, nonVisionUserImagePlaceholder)...)
			}
			result = append(result, copy)
		case types.ToolResultMessage:
			m.Content = replaceImagesWithPlaceholder(m.Content, nonVisionToolImagePlaceholder)
			result = append(result, m)
		case *types.ToolResultMessage:
			if m == nil {
				result = append(result, msg)
				continue
			}
			copy := *m
			copy.Content = replaceImagesWithPlaceholder(copy.Content, nonVisionToolImagePlaceholder)
			result = append(result, copy)
		default:
			result = append(result, msg)
		}
	}

	return result
}

// TransformMessages rewrites a transcript for the target model before provider
// conversion: image downgrades, cross-model thinking cleanup, tool-call ID
// normalization, and synthetic results for orphaned tool calls.
func TransformMessages(messages []types.Message, model types.Model, normalizeToolCallID func(id string, model types.Model, source types.AssistantMessage) string) []types.Message {
	toolCallIDMap := map[string]string{}
	imageAwareMessages := downgradeUnsupportedImages(messages, model)

	transformed := make([]types.Message, 0, len(imageAwareMessages))
	for _, msg := range imageAwareMessages {
		switch m := msg.(type) {
		case types.UserMessage:
			transformed = append(transformed, m)
		case *types.UserMessage:
			if m == nil {
				transformed = append(transformed, msg)
			} else {
				transformed = append(transformed, *m)
			}
		case types.ToolResultMessage:
			if normalizedID, ok := toolCallIDMap[m.ToolCallID]; ok && normalizedID != m.ToolCallID {
				m.ToolCallID = normalizedID
			}
			transformed = append(transformed, m)
		case *types.ToolResultMessage:
			if m == nil {
				transformed = append(transformed, msg)
				continue
			}
			copy := *m
			if normalizedID, ok := toolCallIDMap[copy.ToolCallID]; ok && normalizedID != copy.ToolCallID {
				copy.ToolCallID = normalizedID
			}
			transformed = append(transformed, copy)
		case types.AssistantMessage:
			transformed = append(transformed, transformAssistantMessage(m, model, normalizeToolCallID, toolCallIDMap))
		case *types.AssistantMessage:
			if m == nil {
				transformed = append(transformed, msg)
			} else {
				transformed = append(transformed, transformAssistantMessage(*m, model, normalizeToolCallID, toolCallIDMap))
			}
		default:
			transformed = append(transformed, msg)
		}
	}

	result := make([]types.Message, 0, len(transformed))
	var pendingToolCalls []types.ContentBlock
	existingToolResultIDs := map[string]bool{}
	insertSyntheticToolResults := func() {
		if len(pendingToolCalls) == 0 {
			return
		}
		for _, tc := range pendingToolCalls {
			if !existingToolResultIDs[tc.ID] {
				result = append(result, types.ToolResultMessage{
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
					Content:    []types.ContentBlock{types.NewText("No result provided")},
					IsError:    true,
					// Go uses Unix milliseconds for event timestamps.
					Timestamp: time.Now().UnixMilli(),
				})
			}
		}
		pendingToolCalls = nil
		existingToolResultIDs = map[string]bool{}
	}

	for _, msg := range transformed {
		switch m := msg.(type) {
		case types.AssistantMessage:
			insertSyntheticToolResults()
			if m.StopReason == types.StopError || m.StopReason == types.StopAborted {
				continue
			}

			toolCalls := make([]types.ContentBlock, 0)
			for _, block := range m.Content {
				if block.Type == types.BlockToolCall {
					toolCalls = append(toolCalls, block)
				}
			}
			if len(toolCalls) > 0 {
				pendingToolCalls = toolCalls
				existingToolResultIDs = map[string]bool{}
			}

			result = append(result, m)
		case *types.AssistantMessage:
			if m == nil {
				result = append(result, msg)
				continue
			}
			insertSyntheticToolResults()
			if m.StopReason == types.StopError || m.StopReason == types.StopAborted {
				continue
			}

			toolCalls := make([]types.ContentBlock, 0)
			for _, block := range m.Content {
				if block.Type == types.BlockToolCall {
					toolCalls = append(toolCalls, block)
				}
			}
			if len(toolCalls) > 0 {
				pendingToolCalls = toolCalls
				existingToolResultIDs = map[string]bool{}
			}

			result = append(result, *m)
		case types.ToolResultMessage:
			existingToolResultIDs[m.ToolCallID] = true
			result = append(result, m)
		case *types.ToolResultMessage:
			if m == nil {
				result = append(result, msg)
			} else {
				existingToolResultIDs[m.ToolCallID] = true
				result = append(result, *m)
			}
		case types.UserMessage:
			insertSyntheticToolResults()
			result = append(result, m)
		case *types.UserMessage:
			insertSyntheticToolResults()
			if m == nil {
				result = append(result, msg)
			} else {
				result = append(result, *m)
			}
		default:
			result = append(result, msg)
		}
	}

	insertSyntheticToolResults()
	return result
}

// transformAssistantMessage preserves provider-bound thinking only for its source model while recording rewritten tool IDs for later results.
func transformAssistantMessage(m types.AssistantMessage, model types.Model, normalizeToolCallID func(id string, model types.Model, source types.AssistantMessage) string, toolCallIDMap map[string]string) types.AssistantMessage {
	isSameModel := m.Provider == model.Provider && m.API == model.API && m.Model == model.ID
	content := make([]types.ContentBlock, 0, len(m.Content))

	for _, block := range m.Content {
		switch block.Type {
		case types.BlockThinking:
			if block.Redacted {
				if isSameModel {
					content = append(content, block)
				}
				continue
			}
			if isSameModel && block.ThinkingSignature != "" {
				content = append(content, block)
				continue
			}
			if strings.TrimSpace(block.Thinking) == "" {
				continue
			}
			if isSameModel {
				content = append(content, block)
				continue
			}
			content = append(content, types.NewText(block.Thinking))
		case types.BlockText:
			if isSameModel {
				content = append(content, block)
			} else {
				content = append(content, types.NewText(block.Text))
			}
		case types.BlockToolCall:
			normalizedToolCall := block
			if !isSameModel && block.ThoughtSignature != "" {
				// deviation: TypeScript deletes the optional property; Go clears the
				// field so JSON omits it via `omitempty`.
				normalizedToolCall.ThoughtSignature = ""
			}
			if !isSameModel && normalizeToolCallID != nil {
				normalizedID := normalizeToolCallID(block.ID, model, m)
				if normalizedID != block.ID {
					toolCallIDMap[block.ID] = normalizedID
					normalizedToolCall.ID = normalizedID
				}
			}
			content = append(content, normalizedToolCall)
		default:
			content = append(content, block)
		}
	}

	m.Content = content
	return m
}

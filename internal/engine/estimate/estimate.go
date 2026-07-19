// Package estimate approximates token usage for a transcript by counting
// characters and reusing the last provider-reported usage when available.
// Package estimate provides token-count estimation for context-window budgeting.
// Text tokens are estimated by character count; messages are walked recursively;
// the context estimate adds a fixed overhead for system prompt and tool schemas.
// Usage-anchored estimates refine the guess from actual observed token counts.
package estimate

import (
	"encoding/json"
	"math"

	"go.harness.dev/harness/internal/engine/types"
)

// CharsPerToken is the assumed average number of characters per token.
const CharsPerToken = 4

// EstimatedImageChars is the character weight assigned to one image block.
const EstimatedImageChars = 4800

// EstimatedDocumentPageChars is the character weight of one page of a natively
// delivered (non-text) document, such as a PDF. Anthropic renders each PDF page
// as image plus extracted text, costing up to roughly 3000 tokens per page, so a
// page is weighted at 3000*CharsPerToken characters. This is a deliberately high
// reserving heuristic for compaction and max_tokens clamping, not an
// authorization: it never decides whether a request may be sent.
const EstimatedDocumentPageChars = 3000 * CharsPerToken

// ContextUsageEstimate describes estimated context use.
// Tokens is the estimated total context size; UsageTokens is the
// provider-reported usage anchoring the estimate (or zero); TrailingTokens is
// the estimate for messages after the anchor; LastUsageIndex is the anchoring
// message index, or nil when none.
type ContextUsageEstimate struct {
	Tokens         int  `json:"tokens"`
	UsageTokens    int  `json:"usageTokens"`
	TrailingTokens int  `json:"trailingTokens"`
	LastUsageIndex *int `json:"lastUsageIndex"`
}

// CalculateContextTokens returns provider-reported total tokens, or the summed
// components when totalTokens is zero.
func CalculateContextTokens(u types.Usage) int {
	if u.TotalTokens != 0 {
		return u.TotalTokens
	}
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// safeJSONStringify keeps an invalid tool definition from blocking the context estimate.
func safeJSONStringify(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "[unserializable]"
	}
	if raw == nil {
		return "undefined"
	}
	return string(raw)
}

func estimateTextAndImageContentChars(content types.UserContent) int {
	if !content.IsBlocks() {
		return jsStringLength(content.Text)
	}
	return estimateTextAndImageBlocksChars(content.Blocks)
}

func estimateTextAndImageBlocksChars(blocks []types.ContentBlock) int {
	chars := 0
	for _, block := range blocks {
		chars += blockEstimatedChars(block)
	}
	return chars
}

// blockEstimatedChars returns the character weight of one content block. Text
// blocks count their own length. The net-new document/image reference blocks
// count their delivered size instead of a single image tile, so a large
// extracted-text document or a many-page PDF is not under-counted — that
// undercount let a referenced attachment blow the context window even when the
// byte guard passed. Every other non-text block keeps EstimatedImageChars.
func blockEstimatedChars(block types.ContentBlock) int {
	switch block.Type {
	case types.BlockText:
		return jsStringLength(block.Text)
	case types.BlockDocumentRef:
		return documentRefEstimatedChars(block)
	default:
		return EstimatedImageChars
	}
}

// documentRefEstimatedChars weights a document reference by its delivered form.
// Text-derived documents (OOXML extraction, plain text) are sent as UTF-8 text,
// so their byte size approximates their character count. Native paged documents
// (PDF) weight each page at EstimatedDocumentPageChars rather than counting the
// whole file as one tile. The estimate biases high on purpose: over-counting
// shrinks max_tokens and triggers compaction earlier, the safe direction for a
// context-window guard.
func documentRefEstimatedChars(block types.ContentBlock) int {
	if isTextRefMediaType(block.RefMediaType) {
		if block.RefSizeBytes <= 0 {
			return 0
		}
		return int(block.RefSizeBytes)
	}
	pages := block.RefPageCount
	if pages < 1 {
		pages = 1
	}
	return pages * EstimatedDocumentPageChars
}

// isTextRefMediaType reports whether a reference block is delivered as plain
// text, so its byte size approximates its character (token) weight.
func isTextRefMediaType(mediaType string) bool {
	switch mediaType {
	case "text/plain", "text/markdown", "text/csv":
		return true
	default:
		return false
	}
}

func tokensFromChars(chars int) int {
	return int(math.Ceil(float64(chars) / float64(CharsPerToken)))
}

// EstimateTextTokens estimates text tokens by dividing character count by four.
func EstimateTextTokens(text string) int {
	return tokensFromChars(jsStringLength(text))
}

// EstimateMessageTokens estimates the token weight of one transcript message.
func EstimateMessageTokens(message types.Message) int {
	switch m := message.(type) {
	case types.UserMessage:
		return tokensFromChars(estimateTextAndImageContentChars(m.Content))
	case *types.UserMessage:
		if m == nil {
			return 0
		}
		return tokensFromChars(estimateTextAndImageContentChars(m.Content))
	case types.ToolResultMessage:
		return tokensFromChars(estimateTextAndImageBlocksChars(m.Content))
	case *types.ToolResultMessage:
		if m == nil {
			return 0
		}
		return tokensFromChars(estimateTextAndImageBlocksChars(m.Content))
	case types.AssistantMessage:
		return estimateAssistantMessageTokens(m)
	case *types.AssistantMessage:
		if m == nil {
			return 0
		}
		return estimateAssistantMessageTokens(*m)
	default:
		return 0
	}
}

// estimateAssistantMessageTokens counts thinking and tool-call JSON, which don't live in visible text blocks.
func estimateAssistantMessageTokens(message types.AssistantMessage) int {
	chars := 0
	for _, block := range message.Content {
		switch block.Type {
		case types.BlockText:
			chars += jsStringLength(block.Text)
		case types.BlockThinking:
			chars += jsStringLength(block.Thinking)
		default:
			chars += jsStringLength(block.Name) + jsStringLength(rawArgumentsString(block.Arguments))
		}
	}
	return tokensFromChars(chars)
}

// jsStringLength returns the UTF-16 code-unit length of text, matching
// JavaScript's String.length, so astral characters count as two.
func jsStringLength(text string) int {
	length := 0
	for _, r := range text {
		if r > 0xffff {
			length += 2
		} else {
			length++
		}
	}
	return length
}

func rawArgumentsString(args json.RawMessage) string {
	if len(args) == 0 {
		// A missing ToolCall.arguments value is estimated as an empty object.
		return "{}"
	}
	return string(args)
}

// getLastAssistantUsageInfo finds the latest usable provider usage anchor; aborted and failed turns never observed a complete context.
func getLastAssistantUsageInfo(messages []types.Message) (types.Usage, int, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		assistant, ok := asAssistant(messages[i])
		if !ok {
			continue
		}
		if assistant.StopReason == types.StopAborted || assistant.StopReason == types.StopError {
			continue
		}
		if CalculateContextTokens(assistant.Usage) > 0 {
			return assistant.Usage, i, true
		}
	}
	return types.Usage{}, 0, false
}

func asAssistant(message types.Message) (types.AssistantMessage, bool) {
	switch m := message.(type) {
	case types.AssistantMessage:
		return m, true
	case *types.AssistantMessage:
		if m == nil {
			return types.AssistantMessage{}, false
		}
		return *m, true
	default:
		return types.AssistantMessage{}, false
	}
}

// EstimateMessages estimates a transcript without system prompt or tools.
func EstimateMessages(messages []types.Message) ContextUsageEstimate {
	if usage, index, ok := getLastAssistantUsageInfo(messages); ok {
		usageTokens := CalculateContextTokens(usage)
		trailingTokens := 0
		for i := index + 1; i < len(messages); i++ {
			trailingTokens += EstimateMessageTokens(messages[i])
		}
		lastUsageIndex := index
		return ContextUsageEstimate{Tokens: usageTokens + trailingTokens, UsageTokens: usageTokens, TrailingTokens: trailingTokens, LastUsageIndex: &lastUsageIndex}
	}

	tokens := 0
	for _, message := range messages {
		tokens += EstimateMessageTokens(message)
	}
	return ContextUsageEstimate{Tokens: tokens, UsageTokens: 0, TrailingTokens: tokens, LastUsageIndex: nil}
}

// EstimateContextTokens estimates a full provider context, adding system prompt
// and tool definitions only when no assistant usage block anchors the estimate.
func EstimateContextTokens(ctx types.Context) ContextUsageEstimate {
	estimate := EstimateMessages(ctx.Messages)
	if estimate.LastUsageIndex != nil {
		return estimate
	}

	prefixTokens := 0
	if ctx.SystemPrompt != "" {
		prefixTokens += EstimateTextTokens(ctx.SystemPrompt)
	}
	if len(ctx.Tools) > 0 {
		prefixTokens += EstimateTextTokens(safeJSONStringify(ctx.Tools))
	}

	estimate.Tokens += prefixTokens
	estimate.TrailingTokens += prefixTokens
	return estimate
}

// Package types holds the core message / content-block / tool / streaming types.
package types

import "encoding/json"

// BlockType is the discriminant for a ContentBlock.
type BlockType string

const (
	BlockText     BlockType = "text"
	BlockThinking BlockType = "thinking"
	BlockImage    BlockType = "image"
	// Net-new native-documents additions.
	BlockDocumentRef BlockType = "documentRef"
	BlockImageRef    BlockType = "imageRef"
	BlockToolCall    BlockType = "toolCall"
)

// ContentBlock is a single heterogeneous content block. Per the map's locked
// idiom (02-map-ai-agent.md A9), content blocks are ONE flat tagged struct —
// closest to the Anthropic wire shape, trivial encoding/json, fewer allocations.
// Only the fields relevant to Type are populated; the rest stay zero and are
// omitted from JSON. Interfaces are reserved for the top-level Message/event
// unions, not for blocks.
type ContentBlock struct {
	Type BlockType `json:"type"`

	// text
	Text          string `json:"text,omitempty"`
	TextSignature string `json:"textSignature,omitempty"`

	// thinking
	Thinking          string `json:"thinking,omitempty"`
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
	Redacted          bool   `json:"redacted,omitempty"`

	// image
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`

	// Net-new native-documents additions.
	// documentRef / imageRef
	RefStore     string `json:"refStore,omitempty"`
	RefKey       string `json:"refKey,omitempty"`
	RefMediaType string `json:"refMediaType,omitempty"`
	RefFilename  string `json:"refFilename,omitempty"`
	RefSizeBytes int64  `json:"refSizeBytes,omitempty"`
	RefPageCount int    `json:"refPageCount,omitempty"`

	// toolCall
	ID               string          `json:"id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case BlockText:
		return json.Marshal(struct {
			Type          BlockType `json:"type"`
			Text          string    `json:"text"`
			TextSignature string    `json:"textSignature,omitempty"`
		}{b.Type, b.Text, b.TextSignature})
	case BlockThinking:
		return json.Marshal(struct {
			Type              BlockType `json:"type"`
			Thinking          string    `json:"thinking"`
			ThinkingSignature string    `json:"thinkingSignature,omitempty"`
			Redacted          bool      `json:"redacted,omitempty"`
		}{b.Type, b.Thinking, b.ThinkingSignature, b.Redacted})
	case BlockImage:
		return json.Marshal(struct {
			Type     BlockType `json:"type"`
			Data     string    `json:"data"`
			MimeType string    `json:"mimeType"`
		}{b.Type, b.Data, b.MimeType})
	case BlockDocumentRef, BlockImageRef:
		return json.Marshal(struct {
			Type         BlockType `json:"type"`
			RefStore     string    `json:"refStore"`
			RefKey       string    `json:"refKey"`
			RefMediaType string    `json:"refMediaType"`
			RefFilename  string    `json:"refFilename"`
			RefSizeBytes int64     `json:"refSizeBytes"`
			RefPageCount int       `json:"refPageCount,omitempty"`
		}{
			Type:         b.Type,
			RefStore:     b.RefStore,
			RefKey:       b.RefKey,
			RefMediaType: b.RefMediaType,
			RefFilename:  b.RefFilename,
			RefSizeBytes: b.RefSizeBytes,
			RefPageCount: b.RefPageCount,
		})
	case BlockToolCall:
		args := b.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		return json.Marshal(struct {
			Type             BlockType       `json:"type"`
			ID               string          `json:"id"`
			Name             string          `json:"name"`
			Arguments        json.RawMessage `json:"arguments"`
			ThoughtSignature string          `json:"thoughtSignature,omitempty"`
		}{b.Type, b.ID, b.Name, args, b.ThoughtSignature})
	default:
		type alias ContentBlock
		return json.Marshal(alias(b))
	}
}

// NewText returns a text content block.
func NewText(text string) ContentBlock { return ContentBlock{Type: BlockText, Text: text} }

// NewThinking returns a thinking (reasoning) content block.
func NewThinking(thinking, signature string) ContentBlock {
	return ContentBlock{Type: BlockThinking, Thinking: thinking, ThinkingSignature: signature}
}

// NewImage returns a base64 image content block.
func NewImage(data, mimeType string) ContentBlock {
	return ContentBlock{Type: BlockImage, Data: data, MimeType: mimeType}
}

// NewDocumentRef returns a document reference block. It carries only a
// store+key reference, never bytes, so conversation history stays byte-free.
func NewDocumentRef(store, key, mediaType, filename string, sizeBytes int64, pageCount int) ContentBlock {
	return ContentBlock{
		Type:         BlockDocumentRef,
		RefStore:     store,
		RefKey:       key,
		RefMediaType: mediaType,
		RefFilename:  filename,
		RefSizeBytes: sizeBytes,
		RefPageCount: pageCount,
	}
}

// NewImageRef returns an image reference block. It carries only a store+key
// reference, never bytes, so conversation history stays byte-free.
func NewImageRef(store, key, mediaType, filename string, sizeBytes int64) ContentBlock {
	return ContentBlock{
		Type:         BlockImageRef,
		RefStore:     store,
		RefKey:       key,
		RefMediaType: mediaType,
		RefFilename:  filename,
		RefSizeBytes: sizeBytes,
	}
}

// NewToolCall returns an assistant tool-call block. args is the raw JSON of the
// tool arguments (may be nil; callers should default to `{}` when emitting).
func NewToolCall(id, name string, args json.RawMessage) ContentBlock {
	return ContentBlock{Type: BlockToolCall, ID: id, Name: name, Arguments: args}
}

// StopReason is why a model turn ended.
type StopReason string

const (
	StopStop    StopReason = "stop"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "toolUse"
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
)

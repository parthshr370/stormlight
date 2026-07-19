// Package resource defines typed content returned by internal resource resolvers.
package resource

const (
	// MarkdownMediaType identifies Markdown resources.
	MarkdownMediaType = "text/markdown"
	// TextMediaType identifies plain-text resources.
	TextMediaType = "text/plain"
)

// Content is a resource value whose data is owned by the caller.
type Content struct {
	URI       string
	MediaType string
	Data      []byte
}

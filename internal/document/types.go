// Package document ingests provider-neutral attachments: it resolves them, validates their contents, content-addresses the bytes, and delivers them to provider routes.
//
// resolver.go downloads, validates, and stages legacy URLs in the content-addressed store; inspector.go decides the authoritative media type and page count while guarding against decompression bombs. extract.go derives readable text from OOXML, and blobstore.go is the sandbox-local content-addressed store that writes both raw and derived bytes through CreateTemp then Promote.
//
// blobreader.go reads promoted blobs back for delivery. attachment_registry.go keeps a session's resolved attachments available to the read-only attachment tool, while types.go and errors.go define the shared handles and typed failures.
package document

// BlobRef identifies a blob in a byte store.
type BlobRef struct {
	// Store names the byte store.
	Store string
	// Key identifies the blob within the store.
	Key string
}

// StoreSessionLocal names the sandbox-local session blob store.
const StoreSessionLocal = "session_local"

// DeliveredContent is a route-ready derived representation of an attachment,
// produced when the raw bytes are not natively deliverable (for example, OOXML
// text extraction). It references a separate cached blob; the raw bytes stay
// cached under MediaRef.Blob for identity and records.
type DeliveredContent struct {
	// MediaType is the delivered representation's media type.
	MediaType string
	// Blob identifies the cached delivered bytes.
	Blob BlobRef
	// SizeBytes is the delivered representation size in bytes.
	SizeBytes int64
}

// MediaRef is the fully resolved, integrity-verified internal handle to a cached attachment.
// PageCount is 0 for non-paged media such as images. SHA256 is the raw 32-byte digest.
type MediaRef struct {
	// ID identifies the attachment.
	ID string
	// Filename is the attachment's display name.
	Filename string
	// MediaType is the attachment's media type.
	MediaType string
	// SizeBytes is the attachment size in bytes.
	SizeBytes int64
	// PageCount is the attachment page count, or 0 for non-paged media.
	PageCount int
	// SHA256 is the attachment's raw SHA-256 digest.
	SHA256 [32]byte
	// Blob identifies the cached attachment bytes.
	Blob BlobRef
	// Delivery holds a route-ready representation, such as extracted OOXML text, when a route can't send the native bytes.
	Delivery *DeliveredContent
}

// DocumentRef is the history-safe persisted reference. It carries no hash, bytes, or URL.
type DocumentRef struct {
	// ID identifies the attachment.
	ID string
	// Filename is the attachment's display name.
	Filename string
	// MediaType is the attachment's media type.
	MediaType string
	// SizeBytes is the attachment size in bytes.
	SizeBytes int64
	// PageCount is the attachment page count, or 0 for non-paged media.
	PageCount int
	// Blob identifies the cached attachment bytes.
	Blob BlobRef
}

// Ref returns the history-safe DocumentRef for m, dropping the SHA-256.
func (m MediaRef) Ref() DocumentRef {
	return DocumentRef{
		ID:        m.ID,
		Filename:  m.Filename,
		MediaType: m.MediaType,
		SizeBytes: m.SizeBytes,
		PageCount: m.PageCount,
		Blob:      m.Blob,
	}
}

// AttachmentSourceKind identifies the kind of attachment source.
type AttachmentSourceKind string

const (
	// SourceLegacyAssetURL identifies a legacy asset URL source.
	SourceLegacyAssetURL AttachmentSourceKind = "legacy_asset_url"
)

// AttachmentSource identifies where an attachment came from.
type AttachmentSource struct {
	// Kind identifies the source kind.
	Kind AttachmentSourceKind
	// Reference contains the source-specific reference.
	Reference string
}

// AttachmentInput describes one ordered attachment that the user message parser found. Position is its 0-based order.
type AttachmentInput struct {
	// Source identifies where the attachment came from.
	Source AttachmentSource
	// Filename is the attachment's display name.
	Filename string
	// Position is the attachment's 0-based order in the message.
	Position int
}

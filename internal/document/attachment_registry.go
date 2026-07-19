package document

import (
	"sort"
	"sync"
)

// AttachmentEntry describes one resolved attachment the read-only attachment
// tool can inspect. Blob points at the delivered representation cached in the
// session blob store: extracted text for OOXML, the raw bytes for plain text,
// or the native file for PDFs and images. TextReadable marks the entries the
// tool can slice and grep; native binary (PDF, image) is not text-readable.
type AttachmentEntry struct {
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
	// Blob identifies the delivered representation cached in the session blob store.
	Blob BlobRef
	// TextReadable reports whether the attachment tool can slice and grep the blob.
	TextReadable bool
}

// AttachmentRegistry is a concurrency-safe map of a session's resolved
// attachments, keyed by ID. The server upserts an entry each time it resolves
// an input; the read-only attachment tool reads from it. One registry instance
// is shared by pointer between the server and the once-built agent stack, so a
// document attached on an early turn stays readable on later turns.
type AttachmentRegistry struct {
	mu      sync.RWMutex
	entries map[string]AttachmentEntry
}

// NewAttachmentRegistry returns an empty registry ready for concurrent use.
func NewAttachmentRegistry() *AttachmentRegistry {
	return &AttachmentRegistry{entries: map[string]AttachmentEntry{}}
}

// Put stores or replaces an entry, keyed by its ID. A blank ID is ignored so a
// malformed entry never shadows a real one.
func (r *AttachmentRegistry) Put(entry AttachmentEntry) {
	if entry.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[entry.ID] = entry
}

// Get returns the entry matching key, trying the ID first and then the
// filename, so the agent can reference an attachment by either. The bool
// reports whether a match was found.
func (r *AttachmentRegistry) Get(key string) (AttachmentEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.entries[key]; ok {
		return entry, true
	}
	for _, entry := range r.entries {
		if entry.Filename == key {
			return entry, true
		}
	}
	return AttachmentEntry{}, false
}

// List returns every entry ordered by filename then ID, a stable order for the
// tool's listing output.
func (r *AttachmentRegistry) List() []AttachmentEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AttachmentEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Filename != out[j].Filename {
			return out[i].Filename < out[j].Filename
		}
		return out[i].ID < out[j].ID
	})
	return out
}

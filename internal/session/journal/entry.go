// Package journal persists linear session transcripts as JSONL.
package journal

import (
	"encoding/json"
	"fmt"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

// JournalVersion is the on-disk format version this package writes.
const JournalVersion = 1

// Header is the first line of a session journal.
type Header struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Cwd       string    `json:"cwd"`
	Title     string    `json:"title,omitempty"`
}

// Meta is the base every entry carries.
type Meta struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Entry is one appended journal record.
type Entry interface {
	kind() EntryType
	meta() Meta
}

// EntryType identifies an entry payload.
type EntryType string

const (
	// KindMessage identifies an entry that adds a transcript message.
	KindMessage EntryType = "message"
	// KindModelChange identifies an entry that changes the active model.
	KindModelChange EntryType = "model_change"
	// KindCompaction identifies an entry that summarizes older messages.
	KindCompaction EntryType = "compaction"
)

// MessageEntry adds a transcript message.
type MessageEntry struct {
	Meta
	Message ptypes.Message
}

func (MessageEntry) kind() EntryType { return KindMessage }
func (e MessageEntry) meta() Meta    { return e.Meta }

// ModelChangeEntry changes the active model and optional role.
type ModelChangeEntry struct {
	Meta
	Model string
	Role  string
}

func (ModelChangeEntry) kind() EntryType { return KindModelChange }
func (e ModelChangeEntry) meta() Meta    { return e.Meta }

// CompactionEntry records a summary and the earliest message retained after it.
type CompactionEntry struct {
	Meta
	Summary      string
	FirstKeptID  string
	TokensBefore int
}

func (CompactionEntry) kind() EntryType { return KindCompaction }
func (e CompactionEntry) meta() Meta    { return e.Meta }

// wireEntry is the stable JSONL shape that lets us keep typed message payloads
// as raw JSON until their entry kind tells us how to decode them.
type wireEntry struct {
	Type         EntryType       `json:"type"`
	ID           string          `json:"id"`
	ParentID     string          `json:"parent_id,omitempty"`
	Timestamp    time.Time       `json:"timestamp"`
	Message      json.RawMessage `json:"message,omitempty"`
	Model        string          `json:"model,omitempty"`
	Role         string          `json:"role,omitempty"`
	Summary      string          `json:"summary,omitempty"`
	FirstKeptID  string          `json:"first_kept_id,omitempty"`
	TokensBefore int             `json:"tokens_before,omitempty"`
}

// unknownEntryError lets [Load] retain a forward-compatible journal tail as a
// diagnostic instead of discarding the whole transcript.
type unknownEntryError struct {
	typeName EntryType
	id       string
}

func (e unknownEntryError) Error() string {
	return fmt.Sprintf("unknown journal entry type %q", e.typeName)
}

// MarshalEntry encodes a typed entry for one journal line.
func MarshalEntry(entry Entry) ([]byte, error) {
	switch e := entry.(type) {
	case MessageEntry:
		return marshalMessageEntry(e)
	case *MessageEntry:
		if e == nil {
			return nil, fmt.Errorf("marshal message entry: nil entry")
		}
		return marshalMessageEntry(*e)
	case ModelChangeEntry:
		return json.Marshal(wireEntry{
			Type:      KindModelChange,
			ID:        e.ID,
			ParentID:  e.ParentID,
			Timestamp: e.Timestamp,
			Model:     e.Model,
			Role:      e.Role,
		})
	case *ModelChangeEntry:
		if e == nil {
			return nil, fmt.Errorf("marshal model change entry: nil entry")
		}
		return MarshalEntry(*e)
	case CompactionEntry:
		return json.Marshal(wireEntry{
			Type:         KindCompaction,
			ID:           e.ID,
			ParentID:     e.ParentID,
			Timestamp:    e.Timestamp,
			Summary:      e.Summary,
			FirstKeptID:  e.FirstKeptID,
			TokensBefore: e.TokensBefore,
		})
	case *CompactionEntry:
		if e == nil {
			return nil, fmt.Errorf("marshal compaction entry: nil entry")
		}
		return MarshalEntry(*e)
	default:
		return nil, fmt.Errorf("marshal journal entry: unsupported entry type %T", entry)
	}
}

// marshalMessageEntry keeps the polymorphic message encoding inside one raw
// field, rather than flattening it through the journal's Entry interface.
func marshalMessageEntry(entry MessageEntry) ([]byte, error) {
	if entry.Message == nil {
		return nil, fmt.Errorf("marshal message entry: nil message")
	}
	message, err := json.Marshal(entry.Message)
	if err != nil {
		return nil, fmt.Errorf("marshal message entry: %w", err)
	}
	return json.Marshal(wireEntry{
		Type:      KindMessage,
		ID:        entry.ID,
		ParentID:  entry.ParentID,
		Timestamp: entry.Timestamp,
		Message:   message,
	})
}

// decodeEntry validates the shared envelope before choosing the concrete replay
// record, leaving unknown kinds recoverable to the loader.
func decodeEntry(raw []byte) (Entry, error) {
	var wire wireEntry
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("decode journal entry: %w", err)
	}
	if wire.Type == "" {
		return nil, fmt.Errorf("decode journal entry: missing type")
	}
	if wire.ID == "" {
		return nil, fmt.Errorf("decode journal entry: missing id")
	}
	if wire.Timestamp.IsZero() {
		return nil, fmt.Errorf("decode journal entry: missing timestamp")
	}
	meta := Meta{ID: wire.ID, ParentID: wire.ParentID, Timestamp: wire.Timestamp}
	switch wire.Type {
	case KindMessage:
		message, err := ptypes.UnmarshalMessage(wire.Message)
		if err != nil {
			return nil, fmt.Errorf("decode journal message: %w", err)
		}
		return MessageEntry{Meta: meta, Message: message}, nil
	case KindModelChange:
		return ModelChangeEntry{Meta: meta, Model: wire.Model, Role: wire.Role}, nil
	case KindCompaction:
		return CompactionEntry{
			Meta:         meta,
			Summary:      wire.Summary,
			FirstKeptID:  wire.FirstKeptID,
			TokensBefore: wire.TokensBefore,
		}, nil
	default:
		return nil, unknownEntryError{typeName: wire.Type, id: wire.ID}
	}
}

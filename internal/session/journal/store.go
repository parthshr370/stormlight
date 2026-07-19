package journal

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

// Options configures a new or resumed journal.
type Options struct {
	Clock func() time.Time
	NewID func() string
	Title string
}

// Store is an append-only session journal writer.
type Store struct {
	path   string
	id     string
	file   *os.File
	mu     sync.Mutex
	lastID string
	clock  func() time.Time
	newID  func() string
}

// Create opens a new journal for cwd under dir and writes its header.
func Create(ctx context.Context, dir, cwd string, opts Options) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	clock, newID := resolveOptions(opts)
	timestamp := clock()
	id := newID()
	journalDir := filepath.Join(dir, encodeCwd(cwd))
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	path := filepath.Join(journalDir, timestamp.UTC().Format("20060102T150405Z")+"_"+id+".jsonl")
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	header, err := json.Marshal(Header{
		Type:      "session",
		Version:   JournalVersion,
		ID:        id,
		Timestamp: timestamp,
		Cwd:       cwd,
		Title:     opts.Title,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("marshal journal header: %w", err)
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, fmt.Errorf("create journal: %w", err)
	}
	if err := writeLine(file, header); err != nil {
		cleanup()
		return nil, fmt.Errorf("write journal header: %w", err)
	}
	return &Store{path: path, id: id, file: file, clock: clock, newID: newID}, nil
}

// ID returns the session id.
func (s *Store) ID() string { return s.id }

// Path returns the journal file path.
func (s *Store) Path() string { return s.path }

// AppendMessage writes a message entry.
func (s *Store) AppendMessage(ctx context.Context, message ptypes.Message) error {
	return s.append(ctx, MessageEntry{Message: message})
}

// AppendModelChange writes a model-change entry.
func (s *Store) AppendModelChange(ctx context.Context, model, role string) error {
	return s.append(ctx, ModelChangeEntry{Model: model, Role: role})
}

// AppendCompaction writes a compaction entry.
func (s *Store) AppendCompaction(ctx context.Context, summary, firstKeptID string, tokensBefore int) error {
	return s.append(ctx, CompactionEntry{
		Summary:      summary,
		FirstKeptID:  firstKeptID,
		TokensBefore: tokensBefore,
	})
}

func (s *Store) append(ctx context.Context, entry Entry) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("append journal entry: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("append journal entry: %w", err)
	}
	if s.file == nil {
		return fmt.Errorf("append journal entry: store closed")
	}
	meta := Meta{ID: s.newID(), ParentID: s.lastID, Timestamp: s.clock()}
	var withMeta Entry
	switch value := entry.(type) {
	case MessageEntry:
		value.Meta = meta
		withMeta = value
	case ModelChangeEntry:
		value.Meta = meta
		withMeta = value
	case CompactionEntry:
		value.Meta = meta
		withMeta = value
	default:
		return fmt.Errorf("append journal entry: unsupported entry type %T", entry)
	}
	line, err := MarshalEntry(withMeta)
	if err != nil {
		return fmt.Errorf("marshal journal entry: %w", err)
	}
	if err := writeLine(s.file, line); err != nil {
		return fmt.Errorf("write journal entry: %w", err)
	}
	s.lastID = meta.ID
	return nil
}

// Close flushes and closes the journal. It is safe to call more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	file := s.file
	s.file = nil
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return fmt.Errorf("sync journal: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close journal: %w", closeErr)
	}
	return nil
}

func resolveOptions(opts Options) (func() time.Time, func() string) {
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	newID := opts.NewID
	if newID == nil {
		newID = randomID
	}
	return clock, newID
}

// randomID falls back to the current nanosecond when entropy is unavailable, so
// a transient random-source failure doesn't prevent journaling.
func randomID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err == nil && bytes != [8]byte{} {
		return hex.EncodeToString(bytes[:])
	}
	value := uint64(time.Now().UnixNano())
	if value == 0 {
		value = 1
	}
	binary.BigEndian.PutUint64(bytes[:], value)
	return hex.EncodeToString(bytes[:])
}

// encodeCwd groups journals under a readable workspace slug. Header.Cwd remains
// authoritative because different paths can share a slug.
func encodeCwd(cwd string) string {
	encoded := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(cwd)
	encoded = strings.TrimLeft(encoded, "-")
	if encoded == "" {
		return "root"
	}
	return encoded
}

// writeLine makes a short write an append failure, so callers don't advance the
// in-memory parent ID after an incomplete JSONL record.
func writeLine(file *os.File, line []byte) error {
	buffer := make([]byte, len(line)+1)
	copy(buffer, line)
	buffer[len(line)] = '\n'
	written, err := file.Write(buffer)
	if err != nil {
		return err
	}
	if written != len(buffer) {
		return io.ErrShortWrite
	}
	return nil
}

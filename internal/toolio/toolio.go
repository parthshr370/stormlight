// Package toolio provides concurrency-safe file-mutation serialization,
// rolling output accumulation with temp-file spill (for large bash output),
// and tool-binary resolution (EnsureTool for rg/fd with fdfind alias).
// FileMutationQueue serializes operations per real path; different files
// are parallel-safe, same-file writes are ordered.
package toolio

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"unicode/utf8"

	"go.harness.dev/harness/internal/truncate"
)

// FileMutationQueue serializes file-mutation operations per real path.
type FileMutationQueue struct {
	mu    sync.Mutex
	locks map[string]*fileLock
}

// fileLock keeps a path's mutex alive until every queued caller has released it.
type fileLock struct {
	mu   sync.Mutex
	refs int
}

// NewFileMutationQueue starts a queue with no file locks.
func NewFileMutationQueue() *FileMutationQueue {
	return &FileMutationQueue{locks: map[string]*fileLock{}}
}

// Do serializes fn under filePath's mutex, discarding the return value.
func (q *FileMutationQueue) Do(filePath string, fn func() error) error {
	_, err := DoValue(q, filePath, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

// DoValue serializes fn under filePath's mutex, returning fn's result.
func DoValue[T any](q *FileMutationQueue, filePath string, fn func() (T, error)) (T, error) {
	if q == nil {
		q = NewFileMutationQueue()
	}
	key, err := mutationQueueKey(filePath)
	var zero T
	if err != nil {
		return zero, err
	}
	lock := q.acquire(key)
	lock.mu.Lock()
	defer func() {
		lock.mu.Unlock()
		q.release(key, lock)
	}()
	return fn()
}

// acquire takes a reference before the caller waits, so another finisher can't evict that path's lock.
func (q *FileMutationQueue) acquire(key string) *fileLock {
	q.mu.Lock()
	defer q.mu.Unlock()
	lock := q.locks[key]
	if lock == nil {
		lock = &fileLock{}
		q.locks[key] = lock
	}
	lock.refs++
	return lock
}

// release evicts idle locks so a long-lived queue doesn't retain every path it's seen.
func (q *FileMutationQueue) release(key string, lock *fileLock) {
	q.mu.Lock()
	defer q.mu.Unlock()
	lock.refs--
	if lock.refs == 0 && q.locks[key] == lock {
		delete(q.locks, key)
	}
}

// mutationQueueKey resolves existing symlinks so aliases serialize together, but keeps paths for targets that don't exist yet.
func mutationQueueKey(filePath string) (string, error) {
	resolved, err := filepath.Abs(filePath)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return resolved, nil
		}
		return "", err
	}
	return real, nil
}

// OutputAccumulatorOptions configures truncation limits for the output accumulator.
type OutputAccumulatorOptions struct {
	MaxLines       int
	MaxBytes       int
	TempFilePrefix string
}

// OutputSnapshot is a point-in-time view of the accumulated output.
type OutputSnapshot struct {
	Content        string
	Truncation     truncate.Result
	FullOutputPath string
}

// SnapshotOptions controls snapshot behavior.
type SnapshotOptions struct {
	PersistIfTruncated bool
}

// OutputAccumulator collects streamed output, keeping a rolling tail in memory and spilling to disk when limits are hit.
type OutputAccumulator struct {
	maxLines                 int
	maxBytes                 int
	maxRollingBytes          int
	tempFilePrefix           string
	pendingUTF8              []byte
	rawChunks                [][]byte
	tailText                 string
	tailBytes                int
	tailStartsAtLineBoundary bool
	totalRawBytes            int
	totalDecodedBytes        int
	completedLines           int
	totalLines               int
	currentLineBytes         int
	hasOpenLine              bool
	finished                 bool
	tempFilePath             string
	tempFile                 *os.File
}

// NewOutputAccumulator starts a rolling output buffer with the supplied limits.
func NewOutputAccumulator(options OutputAccumulatorOptions) *OutputAccumulator {
	maxLines := options.MaxLines
	if maxLines == 0 {
		maxLines = truncate.DefaultMaxLines
	}
	maxBytes := options.MaxBytes
	if maxBytes == 0 {
		maxBytes = truncate.DefaultMaxBytes
	}
	prefix := options.TempFilePrefix
	if prefix == "" {
		prefix = "harness-output"
	}
	maxRollingBytes := maxBytes * 2
	if maxRollingBytes < 1 {
		maxRollingBytes = 1
	}
	return &OutputAccumulator{maxLines: maxLines, maxBytes: maxBytes, maxRollingBytes: maxRollingBytes, tempFilePrefix: prefix, tailStartsAtLineBoundary: true}
}

// Append feeds raw bytes into the accumulator.
func (a *OutputAccumulator) Append(data []byte) error {
	if a.finished {
		return errors.New("Cannot append to a finished output accumulator")
	}
	a.totalRawBytes += len(data)
	a.appendDecodedText(a.decode(data, false))
	if a.tempFile != nil || a.shouldUseTempFile() {
		if err := a.ensureTempFile(); err != nil {
			return err
		}
		if a.tempFile == nil {
			return nil
		}
		_, err := a.tempFile.Write(data)
		if err != nil {
			a.cleanupTempFile()
		}
		return err
	}
	if len(data) > 0 {
		copyData := append([]byte(nil), data...)
		a.rawChunks = append(a.rawChunks, copyData)
	}
	return nil
}

// Finish marks the accumulator complete and drains pending bytes.
func (a *OutputAccumulator) Finish() error {
	if a.finished {
		return nil
	}
	a.finished = true
	a.appendDecodedText(a.decode(nil, true))
	if a.shouldUseTempFile() {
		return a.ensureTempFile()
	}
	return nil
}

// Snapshot returns the current tail content with truncation metadata.
func (a *OutputAccumulator) Snapshot(options SnapshotOptions) (OutputSnapshot, error) {
	tailTruncation := truncate.Tail(a.getSnapshotText(), truncate.Options{MaxLines: truncate.Int(a.maxLines), MaxBytes: truncate.Int(a.maxBytes)})
	truncated := a.totalLines > a.maxLines || a.totalDecodedBytes > a.maxBytes
	truncatedBy := tailTruncation.TruncatedBy
	if truncated && truncatedBy == "" {
		if a.totalDecodedBytes > a.maxBytes {
			truncatedBy = truncate.TruncatedByBytes
		} else {
			truncatedBy = truncate.TruncatedByLines
		}
	}
	tailTruncation.Truncated = truncated
	tailTruncation.TruncatedBy = truncatedBy
	tailTruncation.TotalLines = a.totalLines
	tailTruncation.TotalBytes = a.totalDecodedBytes
	tailTruncation.MaxLines = a.maxLines
	tailTruncation.MaxBytes = a.maxBytes
	if options.PersistIfTruncated && tailTruncation.Truncated {
		if err := a.ensureTempFile(); err != nil {
			return OutputSnapshot{}, err
		}
	}
	return OutputSnapshot{Content: tailTruncation.Content, Truncation: tailTruncation, FullOutputPath: a.tempFilePath}, nil
}

// CloseTempFile closes the spill temp file if one was opened.
func (a *OutputAccumulator) CloseTempFile() error {
	if a.tempFile == nil {
		return nil
	}
	err := a.tempFile.Close()
	a.tempFile = nil
	return err
}

// GetLastLineBytes returns the byte count of the current incomplete line.
func (a *OutputAccumulator) GetLastLineBytes() int {
	return a.currentLineBytes
}

// decode saves an incomplete UTF-8 suffix between Append calls instead of emitting a replacement rune.
func (a *OutputAccumulator) decode(data []byte, final bool) string {
	buf := append(a.pendingUTF8, data...)
	a.pendingUTF8 = nil
	if len(buf) == 0 {
		return ""
	}
	out := make([]rune, 0, len(buf))
	for len(buf) > 0 {
		if !final && !utf8.FullRune(buf) {
			a.pendingUTF8 = append(a.pendingUTF8, buf...)
			break
		}
		r, size := utf8.DecodeRune(buf)
		out = append(out, r)
		if size <= 0 {
			break
		}
		buf = buf[size:]
	}
	return string(out)
}

// appendDecodedText tracks decoded output separately from raw input because limits and line state must reflect valid text.
func (a *OutputAccumulator) appendDecodedText(text string) {
	if text == "" {
		return
	}
	bytes := len([]byte(text))
	a.totalDecodedBytes += bytes
	a.tailText += text
	a.tailBytes += bytes
	if a.tailBytes > a.maxRollingBytes*2 {
		a.trimTail()
	}

	newlines := 0
	lastNewline := -1
	for i, r := range text {
		if r == '\n' {
			newlines++
			lastNewline = i
		}
	}
	if newlines == 0 {
		a.currentLineBytes += bytes
		a.hasOpenLine = true
	} else {
		a.completedLines += newlines
		tail := text[lastNewline+1:]
		a.currentLineBytes = len([]byte(tail))
		a.hasOpenLine = tail != ""
	}
	a.totalLines = a.completedLines
	if a.hasOpenLine {
		a.totalLines++
	}
}

// trimTail shifts its cut to a rune boundary and remembers whether the retained text starts mid-line.
func (a *OutputAccumulator) trimTail() {
	buffer := []byte(a.tailText)
	if len(buffer) <= a.maxRollingBytes {
		a.tailBytes = len(buffer)
		return
	}
	start := len(buffer) - a.maxRollingBytes
	for start < len(buffer) && (buffer[start]&0xc0) == 0x80 {
		start++
	}
	if start != 0 {
		a.tailStartsAtLineBoundary = buffer[start-1] == 0x0a
	}
	a.tailText = string(buffer[start:])
	a.tailBytes = len([]byte(a.tailText))
}

// getSnapshotText removes a partial leading line after a rolling byte cut, unless no full line survived.
func (a *OutputAccumulator) getSnapshotText() string {
	if a.tailStartsAtLineBoundary {
		return a.tailText
	}
	firstNewline := -1
	for i, r := range a.tailText {
		if r == '\n' {
			firstNewline = i
			break
		}
	}
	if firstNewline == -1 {
		return a.tailText
	}
	return a.tailText[firstNewline+1:]
}

func (a *OutputAccumulator) shouldUseTempFile() bool {
	return a.totalRawBytes > a.maxBytes || a.totalDecodedBytes > a.maxBytes || a.totalLines > a.maxLines
}

// ensureTempFile copies chunks collected before the spill threshold, so persisted snapshots contain the whole stream.
func (a *OutputAccumulator) ensureTempFile() error {
	if a.tempFilePath != "" {
		return nil
	}
	file, err := os.CreateTemp("", a.tempFilePrefix+"-*.log")
	if err != nil {
		return err
	}
	a.tempFilePath = file.Name()
	a.tempFile = file
	for _, chunk := range a.rawChunks {
		if _, err := a.tempFile.Write(chunk); err != nil {
			a.cleanupTempFile()
			return err
		}
	}
	a.rawChunks = nil
	return nil
}

func (a *OutputAccumulator) cleanupTempFile() {
	if a.tempFile != nil {
		_ = a.tempFile.Close()
		a.tempFile = nil
	}
	if a.tempFilePath != "" {
		_ = os.Remove(a.tempFilePath)
		a.tempFilePath = ""
	}
}

// EnsureTool finds the binary for fd or rg on PATH, trying fdfind as a fallback for fd.
func EnsureTool(tool string) (string, bool) {
	if tool != "fd" && tool != "rg" {
		return "", false
	}
	candidates := []string{tool}
	if tool == "fd" {
		candidates = append(candidates, "fdfind")
	}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path, true
		}
	}
	return "", false
}

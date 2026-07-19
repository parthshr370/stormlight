package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

	"go.harness.dev/harness/internal/obs"
)

// defaultLogFileMaxBytes is the rotation threshold for HARNESS_LOG_FILE when
// HARNESS_LOG_FILE_MAX_BYTES is unset.
const defaultLogFileMaxBytes int64 = 100 << 20 // 100 MiB

// logFileMaxBytes returns the rotation threshold for HARNESS_LOG_FILE, taken
// from HARNESS_LOG_FILE_MAX_BYTES (bytes) when set and positive, else the
// default.
func logFileMaxBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("HARNESS_LOG_FILE_MAX_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultLogFileMaxBytes
}

// rotatingWriter is an io.Writer for the opt-in diagnostic log file. It bounds
// disk use to roughly 2*max: when a write would cross max, it closes the file,
// renames it to path+".1" (replacing any previous rollover), and reopens a
// fresh one. A single write larger than max is never split, so the exact bound
// is max plus the largest record; harness records are small (debug previews are
// capped and error attributes truncated), so in practice this is ~max. It never
// touches the supervisor-owned session log on stderr.
type rotatingWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

// newRotatingWriter opens path for append and seeds the size from the existing
// file, so an established log rotates at the right point after a restart.
func newRotatingWriter(path string, max int64) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	return &rotatingWriter{path: path, max: max, f: f, size: size}, nil
}

// Write appends p, rotating first if the write would push the file past max.
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.max > 0 && w.size+int64(len(p)) > w.max {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate closes the active file, moves it aside to path+".1", and opens a fresh
// truncated file. Callers hold w.mu. A rename failure is tolerated: the reopen
// below uses O_TRUNC, so the file is bounded even when the rollover copy could
// not be kept.
func (w *rotatingWriter) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	_ = os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	w.size = 0
	return nil
}

// redactAttr scrubs secrets from every string or error attribute value logged
// through the default handler. Wiring it via ReplaceAttr prevents uninstrumented
// call sites from leaking credentials into logs.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(obs.Redact(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(obs.Redact(err.Error()))
		}
	}
	return a
}

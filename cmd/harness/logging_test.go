package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriterRotatesAtMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	w, err := newRotatingWriter(path, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("a", 15))); err != nil { // under max
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("b", 15))); err != nil { // crosses max -> rotate first
		t.Fatal(err)
	}
	rolled, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("rollover file missing: %v", err)
	}
	if string(rolled) != strings.Repeat("a", 15) {
		t.Fatalf("rollover content = %q", rolled)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != strings.Repeat("b", 15) {
		t.Fatalf("active content = %q", active)
	}
}

func TestRotatingWriterReplacesExistingRollover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	w, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, w, strings.Repeat("a", 8)) // active=8
	mustWrite(t, w, strings.Repeat("b", 8)) // rotate: .1=aaaaaaaa, active=bbbbbbbb
	mustWrite(t, w, strings.Repeat("c", 8)) // rotate: .1=bbbbbbbb (replaced), active=cccccccc
	if rolled, _ := os.ReadFile(path + ".1"); string(rolled) != strings.Repeat("b", 8) {
		t.Fatalf(".1 not replaced: %q", rolled)
	}
	if active, _ := os.ReadFile(path); string(active) != strings.Repeat("c", 8) {
		t.Fatalf("active = %q", active)
	}
}

func TestRotatingWriterOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	w, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("z", 50) // single record larger than max: written whole, not split
	mustWrite(t, w, big)
	if active, _ := os.ReadFile(path); string(active) != big {
		t.Fatalf("oversized record not written whole: %d bytes", len(active))
	}
	mustWrite(t, w, "small") // next write rotates the oversized file aside
	rolled, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("expected rotation after oversized record: %v", err)
	}
	if string(rolled) != big {
		t.Fatalf("rolled content = %d bytes", len(rolled))
	}
}

func TestLogFileMaxBytes(t *testing.T) {
	t.Setenv("HARNESS_LOG_FILE_MAX_BYTES", "")
	if logFileMaxBytes() != defaultLogFileMaxBytes {
		t.Fatal("empty should yield default")
	}
	t.Setenv("HARNESS_LOG_FILE_MAX_BYTES", "4096")
	if logFileMaxBytes() != 4096 {
		t.Fatal("override not honored")
	}
	t.Setenv("HARNESS_LOG_FILE_MAX_BYTES", "garbage")
	if logFileMaxBytes() != defaultLogFileMaxBytes {
		t.Fatal("garbage should yield default")
	}
	t.Setenv("HARNESS_LOG_FILE_MAX_BYTES", "-5")
	if logFileMaxBytes() != defaultLogFileMaxBytes {
		t.Fatal("non-positive should yield default")
	}
}

func mustWrite(t *testing.T, w *rotatingWriter, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

// TestRedactAttrScrubs proves the default-handler ReplaceAttr scrubs secrets
// from both string and error attribute values — the safety net that covers
// un-instrumented call sites (e.g. the HARNESS_LOG_FILE-open warning, which
// carries the env-controlled path/error).
func TestRedactAttrScrubs(t *testing.T) {
	got := redactAttr(nil, slog.String("path", "/logs?token=supersecret12345"))
	if strings.Contains(got.Value.String(), "supersecret12345") {
		t.Fatalf("string attr not redacted: %q", got.Value.String())
	}
	gotErr := redactAttr(nil, slog.Any("error", fmt.Errorf("open failed: api_key=leaked9876543")))
	if strings.Contains(gotErr.Value.String(), "leaked9876543") {
		t.Fatalf("error attr not redacted: %q", gotErr.Value.String())
	}
}

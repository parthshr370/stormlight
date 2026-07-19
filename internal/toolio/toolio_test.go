package toolio

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileMutationQueueSerializesSameFile(t *testing.T) {
	q := NewFileMutationQueue()
	file := filepath.Join(t.TempDir(), "file.txt")
	var active int32
	var violations int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := q.Do(file, func() error {
				if atomic.AddInt32(&active, 1) != 1 {
					atomic.AddInt32(&violations, 1)
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&active, -1)
				return nil
			}); err != nil {
				t.Errorf("Do: %v", err)
			}
		}()
	}
	wg.Wait()
	if violations != 0 {
		t.Fatalf("same-file critical sections overlapped %d times", violations)
	}
}

func TestFileMutationQueueENOTDIRFallsBackToResolvedPath(t *testing.T) {
	q := NewFileMutationQueue()
	dir := t.TempDir()
	regularFile := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regularFile, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("fn ran")
	err := q.Do(filepath.Join(regularFile, "child"), func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected callback error after ENOTDIR fallback, got %v", err)
	}
}

func TestFileMutationQueueAllowsDifferentFilesInParallel(t *testing.T) {
	q := NewFileMutationQueue()
	dir := t.TempDir()
	started := make(chan string, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, name := range []string{"a.txt", "b.txt"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = q.Do(filepath.Join(dir, name), func() error {
				started <- name
				<-release
				return nil
			})
		}()
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			close(release)
			t.Fatal("different files did not run in parallel")
		}
	}
	close(release)
	wg.Wait()
	if !seen["a.txt"] || !seen["b.txt"] {
		t.Fatalf("started = %v", seen)
	}
}

func TestOutputAccumulatorUTF8SplitAndSnapshot(t *testing.T) {
	acc := NewOutputAccumulator(OutputAccumulatorOptions{MaxBytes: 10, MaxLines: 10, TempFilePrefix: "harness-test"})
	if err := acc.Append([]byte{0xc3}); err != nil {
		t.Fatal(err)
	}
	if err := acc.Append([]byte{0xa9}); err != nil {
		t.Fatal(err)
	}
	if err := acc.Finish(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := acc.Snapshot(SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Content != "é" || snapshot.Truncation.TotalBytes != 2 || snapshot.Truncation.TotalLines != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestOutputAccumulatorInvalidUTF8UsesReplacement(t *testing.T) {
	acc := NewOutputAccumulator(OutputAccumulatorOptions{MaxBytes: 100, MaxLines: 10, TempFilePrefix: "harness-test"})
	if err := acc.Append([]byte{0xff, 'a'}); err != nil {
		t.Fatal(err)
	}
	if err := acc.Append([]byte{0xc3}); err != nil {
		t.Fatal(err)
	}
	if err := acc.Finish(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := acc.Snapshot(SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Content != "�a�" {
		t.Fatalf("snapshot content = %q", snapshot.Content)
	}
}

func TestOutputAccumulatorSpillsAndTracksTail(t *testing.T) {
	acc := NewOutputAccumulator(OutputAccumulatorOptions{MaxBytes: 5, MaxLines: 2, TempFilePrefix: "harness-test"})
	for _, chunk := range [][]byte{[]byte("one\n"), []byte("two\n"), []byte("three\n")} {
		if err := acc.Append(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := acc.Finish(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := acc.Snapshot(SnapshotOptions{PersistIfTruncated: true})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Truncation.Truncated || snapshot.Truncation.TotalLines != 3 || snapshot.Truncation.TotalBytes != len("one\ntwo\nthree\n") || snapshot.Content != "three" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.FullOutputPath == "" {
		t.Fatal("expected temp file path")
	}
	if err := acc.CloseTempFile(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot.FullOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\nthree\n" {
		t.Fatalf("temp file = %q", string(data))
	}
	_ = os.Remove(snapshot.FullOutputPath)
}

func TestOutputAccumulatorAppendAfterCloseTempFileDoesNotPanic(t *testing.T) {
	acc := NewOutputAccumulator(OutputAccumulatorOptions{MaxBytes: 1, MaxLines: 10, TempFilePrefix: "harness-test"})
	if err := acc.Append([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := acc.Snapshot(SnapshotOptions{PersistIfTruncated: true})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FullOutputPath == "" {
		t.Fatal("expected spill file")
	}
	if err := acc.CloseTempFile(); err != nil {
		t.Fatal(err)
	}
	if err := acc.Append([]byte("def")); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(snapshot.FullOutputPath)
}

func TestOutputAccumulatorLastLineAndFinishedAppend(t *testing.T) {
	acc := NewOutputAccumulator(OutputAccumulatorOptions{})
	if err := acc.Append([]byte("ab\ncé")); err != nil {
		t.Fatal(err)
	}
	if got := acc.GetLastLineBytes(); got != len([]byte("cé")) {
		t.Fatalf("GetLastLineBytes = %d", got)
	}
	if err := acc.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := acc.Append([]byte("x")); err == nil || err.Error() != "Cannot append to a finished output accumulator" {
		t.Fatalf("append after finish err = %v", err)
	}
}

func TestEnsureTool(t *testing.T) {
	if path, ok := EnsureTool("definitely-not-a-supported-tool"); ok || path != "" {
		t.Fatalf("EnsureTool unsupported = %q %v", path, ok)
	}
}

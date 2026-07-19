package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalBlobStorePutOpenStatDelete(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalBlobStore(root)
	if err != nil {
		t.Fatalf("NewLocalBlobStore() error = %v", err)
	}
	ctx := context.Background()
	firstBytes := []byte("first complete attachment")
	secondBytes := []byte("a different attachment")

	first, err := store.Put(ctx, bytes.NewReader(firstBytes), UploadMetadata{
		Filename:  "first.bin",
		MediaType: "application/x-test",
	})
	if err != nil {
		t.Fatalf("Put(first) error = %v", err)
	}
	second, err := store.Put(ctx, bytes.NewReader(secondBytes), UploadMetadata{
		Filename:  "second.bin",
		MediaType: "anything/the-caller-needs",
	})
	if err != nil {
		t.Fatalf("Put(second) error = %v", err)
	}
	if first.Key == second.Key {
		t.Fatalf("different bytes produced the same key %q", first.Key)
	}

	wantPath := filepath.Join(root, "blobs", first.Key[:2], first.Key)
	if info, err := os.Stat(wantPath); err != nil {
		t.Fatalf("content-addressed path %q missing: %v", wantPath, err)
	} else if !info.Mode().IsRegular() {
		t.Fatalf("content-addressed path mode = %v, want regular file", info.Mode())
	}

	opened, err := store.Open(ctx, first)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	gotBytes, readErr := io.ReadAll(opened)
	closeErr := opened.Close()
	if readErr != nil {
		t.Fatalf("ReadAll(Open()) error = %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("Close(Open()) error = %v", closeErr)
	}
	if !bytes.Equal(gotBytes, firstBytes) {
		t.Fatalf("Open() bytes = %q, want %q", gotBytes, firstBytes)
	}

	metadata, err := store.Stat(ctx, first)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if metadata.Store != StoreSessionLocal || metadata.Key != first.Key {
		t.Fatalf("Stat() identity = {%q %q}, want {%q %q}", metadata.Store, metadata.Key, StoreSessionLocal, first.Key)
	}
	if metadata.SizeBytes != int64(len(firstBytes)) {
		t.Fatalf("Stat().SizeBytes = %d, want %d", metadata.SizeBytes, len(firstBytes))
	}
	if got := hex.EncodeToString(metadata.SHA256[:]); got != first.Key {
		t.Fatalf("Stat() SHA256 = %q, want key %q", got, first.Key)
	}

	if err := store.Delete(ctx, first); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Fatalf("deleted path Stat() error = %v, want not-exist error", err)
	}
	if err := store.Delete(ctx, first); err != nil {
		t.Fatalf("second Delete() error = %v, want nil", err)
	}
}

func TestLocalBlobStoreDeduplicatesIdenticalBytes(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalBlobStore(root)
	if err != nil {
		t.Fatalf("NewLocalBlobStore() error = %v", err)
	}
	ctx := context.Background()
	contents := []byte("same trusted bytes")

	first, err := store.Put(ctx, bytes.NewReader(contents), UploadMetadata{Filename: "one"})
	if err != nil {
		t.Fatalf("first Put() error = %v", err)
	}
	second, err := store.Put(ctx, bytes.NewReader(contents), UploadMetadata{Filename: "two"})
	if err != nil {
		t.Fatalf("second Put() error = %v", err)
	}
	if first != second {
		t.Fatalf("identical Put() refs differ: first = %#v, second = %#v", first, second)
	}
	if got := countRegularFiles(t, filepath.Join(root, "blobs")); got != 1 {
		t.Fatalf("regular files under blobs = %d, want 1", got)
	}
}

func TestLocalBlobStoreStagedPromote(t *testing.T) {
	t.Run("promotes validated bytes", func(t *testing.T) {
		root := t.TempDir()
		store, err := NewLocalBlobStore(root)
		if err != nil {
			t.Fatalf("NewLocalBlobStore() error = %v", err)
		}
		contents := []byte("downloaded bytes after integrity validation")
		digest := sha256.Sum256(contents)

		tmp, err := store.CreateTemp()
		if err != nil {
			t.Fatalf("CreateTemp() error = %v", err)
		}
		assertInsideBlobsDir(t, root, tmp.Name())
		if _, err := tmp.Write(contents); err != nil {
			t.Fatalf("temp Write() error = %v", err)
		}
		tmpPath := tmp.Name()
		if err := tmp.Close(); err != nil {
			t.Fatalf("temp Close() error = %v", err)
		}

		ref, err := store.Promote(tmpPath, digest)
		if err != nil {
			t.Fatalf("Promote() error = %v", err)
		}
		wantKey := hex.EncodeToString(digest[:])
		if ref != (BlobRef{Store: StoreSessionLocal, Key: wantKey}) {
			t.Fatalf("Promote() ref = %#v, want store %q key %q", ref, StoreSessionLocal, wantKey)
		}
		if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
			t.Fatalf("old temp path Stat() error = %v, want not-exist error after rename", err)
		}
		wantPath := filepath.Join(root, "blobs", wantKey[:2], wantKey)
		if _, err := os.Stat(wantPath); err != nil {
			t.Fatalf("promoted path %q missing: %v", wantPath, err)
		}

		opened, err := store.Open(context.Background(), ref)
		if err != nil {
			t.Fatalf("Open(promoted) error = %v", err)
		}
		got, readErr := io.ReadAll(opened)
		closeErr := opened.Close()
		if readErr != nil {
			t.Fatalf("ReadAll(promoted) error = %v", readErr)
		}
		if closeErr != nil {
			t.Fatalf("Close(promoted) error = %v", closeErr)
		}
		if !bytes.Equal(got, contents) {
			t.Fatalf("promoted bytes = %q, want %q", got, contents)
		}
	})

	t.Run("abandoned temp leaves no blob", func(t *testing.T) {
		root := t.TempDir()
		store, err := NewLocalBlobStore(root)
		if err != nil {
			t.Fatalf("NewLocalBlobStore() error = %v", err)
		}
		tmp, err := store.CreateTemp()
		if err != nil {
			t.Fatalf("CreateTemp() error = %v", err)
		}
		assertInsideBlobsDir(t, root, tmp.Name())
		if _, err := tmp.Write([]byte("unvalidated bytes")); err != nil {
			t.Fatalf("temp Write() error = %v", err)
		}
		tmpPath := tmp.Name()
		if err := tmp.Close(); err != nil {
			t.Fatalf("temp Close() error = %v", err)
		}
		if err := os.Remove(tmpPath); err != nil {
			t.Fatalf("remove abandoned temp error = %v", err)
		}
		if got := countRegularFiles(t, filepath.Join(root, "blobs")); got != 0 {
			t.Fatalf("regular files under blobs after abandoning temp = %d, want 0", got)
		}
	})
}

func TestLocalBlobStoreRejectsMalformedKeys(t *testing.T) {
	store, err := NewLocalBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBlobStore() error = %v", err)
	}
	bad := BlobRef{Store: StoreSessionLocal, Key: "nothex"}
	if _, err := store.Open(context.Background(), bad); err == nil {
		t.Fatal("Open(malformed key) error = nil, want an error")
	}
	if _, err := store.Stat(context.Background(), bad); err == nil {
		t.Fatal("Stat(malformed key) error = nil, want an error")
	}
}

func assertInsideBlobsDir(t *testing.T, root, path string) {
	t.Helper()
	blobsDir := filepath.Join(root, "blobs")
	rel, err := filepath.Rel(blobsDir, path)
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q) error = %v", blobsDir, path, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("temp path %q is not inside blobs dir %q", path, blobsDir)
	}
}

func countRegularFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%q) error = %v", root, err)
	}
	return count
}

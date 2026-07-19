package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// UploadMetadata describes bytes that a caller writes to a BlobStore.
type UploadMetadata struct {
	// Filename is the attachment's display name.
	Filename string
	// MediaType is the attachment's media type.
	MediaType string
}

// BlobMetadata describes a stored blob.
type BlobMetadata struct {
	// Store names the byte store.
	Store string
	// Key identifies the blob within the store.
	Key string
	// SizeBytes is the blob size in bytes.
	SizeBytes int64
	// SHA256 is the blob's raw SHA-256 digest.
	SHA256 [32]byte
}

// BlobStore is content-addressed byte storage for resolved attachments.
type BlobStore interface {
	// Put stores bytes and returns their content-addressed reference.
	Put(ctx context.Context, r io.Reader, meta UploadMetadata) (BlobRef, error)
	// Open opens the stored bytes for a reference.
	Open(ctx context.Context, ref BlobRef) (io.ReadCloser, error)
	// Stat returns metadata for a stored blob.
	Stat(ctx context.Context, ref BlobRef) (BlobMetadata, error)
	// Delete removes a stored blob.
	Delete(ctx context.Context, ref BlobRef) error
}

// LocalBlobStore stores blobs on the sandbox filesystem under <root>/blobs/<aa>/<sha256hex>,
// content-addressed by SHA-256. It satisfies BlobStore.
type LocalBlobStore struct {
	root    string
	storeID string
}

// NewLocalBlobStore returns a store rooted at root, creating <root>/blobs if needed.
// Its blobs carry the StoreSessionLocal store ID.
func NewLocalBlobStore(root string) (*LocalBlobStore, error) {
	return newScopedBlobStore(root, StoreSessionLocal)
}

// newScopedBlobStore returns a store rooted at root whose blobs carry storeID.
// The session resolver uses a per-session namespace as storeID so a router-wide
// reader can select the right root from the BlobRef alone.
func newScopedBlobStore(root, storeID string) (*LocalBlobStore, error) {
	store := &LocalBlobStore{root: root, storeID: storeID}
	if err := os.MkdirAll(store.blobsDir(), 0o755); err != nil {
		return nil, fmt.Errorf("create local blob store: %w", err)
	}
	return store, nil
}

// Put stores only already-trusted, complete bytes for in-process and test use.
// The download-integrity gate never calls Put; it stages with CreateTemp, validates, and calls
// Promote only after validation, so unvalidated bytes never enter the content-addressed tree.
func (s *LocalBlobStore) Put(ctx context.Context, r io.Reader, meta UploadMetadata) (BlobRef, error) {
	if err := ctx.Err(); err != nil {
		return BlobRef{}, err
	}

	tmp, err := s.CreateTemp()
	if err != nil {
		return BlobRef{}, err
	}
	tmpPath := tmp.Name()
	promoted := false
	defer func() {
		if !promoted {
			_ = os.Remove(tmpPath)
		}
	}()

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hash), r); err != nil {
		_ = tmp.Close()
		return BlobRef{}, fmt.Errorf("write blob temp file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = tmp.Close()
		return BlobRef{}, err
	}
	if err := tmp.Close(); err != nil {
		return BlobRef{}, fmt.Errorf("close blob temp file: %w", err)
	}

	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	ref, err := s.Promote(tmpPath, digest)
	if err != nil {
		return BlobRef{}, err
	}
	promoted = true
	return ref, nil
}

// Open opens the stored bytes for ref.
func (s *LocalBlobStore) Open(ctx context.Context, ref BlobRef) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, _, err := s.pathForRef(ref)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	return file, nil
}

// Stat returns metadata for the stored bytes at ref.
func (s *LocalBlobStore) Stat(ctx context.Context, ref BlobRef) (BlobMetadata, error) {
	if err := ctx.Err(); err != nil {
		return BlobMetadata{}, err
	}
	path, digest, err := s.pathForRef(ref)
	if err != nil {
		return BlobMetadata{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return BlobMetadata{}, fmt.Errorf("stat blob: %w", err)
	}
	return BlobMetadata{
		Store:     s.storeID,
		Key:       ref.Key,
		SizeBytes: info.Size(),
		SHA256:    digest,
	}, nil
}

// Delete removes the stored bytes at ref. A missing blob is already deleted and returns nil.
func (s *LocalBlobStore) Delete(ctx context.Context, ref BlobRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, _, err := s.pathForRef(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete blob: %w", err)
	}
	return nil
}

// CreateTemp returns an open temp file inside the blobs dir. It lives on the same filesystem as the
// final blobs so Promote is a rename, never a cross-device copy. Callers own closing and removing it.
func (s *LocalBlobStore) CreateTemp() (*os.File, error) {
	file, err := os.CreateTemp(s.blobsDir(), ".blob-*")
	if err != nil {
		return nil, fmt.Errorf("create blob temp file: %w", err)
	}
	return file, nil
}

// Promote atomically renames tmpPath from CreateTemp to the content-addressed path for sha and
// returns its BlobRef. It is the last step of the download-integrity gate: validate, then promote.
func (s *LocalBlobStore) Promote(tmpPath string, sha [32]byte) (BlobRef, error) {
	key := hex.EncodeToString(sha[:])
	dir := filepath.Join(s.blobsDir(), key[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return BlobRef{}, fmt.Errorf("create blob prefix directory: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, key)); err != nil {
		return BlobRef{}, fmt.Errorf("promote blob temp file: %w", err)
	}
	return BlobRef{Store: s.storeID, Key: key}, nil
}

func (s *LocalBlobStore) blobsDir() string {
	return filepath.Join(s.root, "blobs")
}

func (s *LocalBlobStore) pathForRef(ref BlobRef) (string, [32]byte, error) {
	if ref.Store != s.storeID {
		return "", [32]byte{}, fmt.Errorf("unsupported blob store %q", ref.Store)
	}
	digest, err := decodeKey(ref.Key)
	if err != nil {
		return "", [32]byte{}, err
	}
	return filepath.Join(s.blobsDir(), ref.Key[:2], ref.Key), digest, nil
}

// decodeKey enforces canonical lowercase SHA-256 before a key can join a storage path.
func decodeKey(key string) ([32]byte, error) {
	if len(key) != sha256.Size*2 {
		return [32]byte{}, fmt.Errorf("invalid blob key length: got %d, want %d", len(key), sha256.Size*2)
	}
	decoded, err := hex.DecodeString(key)
	if err != nil || hex.EncodeToString(decoded) != key {
		return [32]byte{}, fmt.Errorf("invalid blob key %q: must be lowercase SHA-256 hex", key)
	}
	var digest [sha256.Size]byte
	copy(digest[:], decoded)
	return digest, nil
}

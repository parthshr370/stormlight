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

// SessionNamespace derives the per-session blob-store namespace from a session
// ID. It is the lowercase hex SHA-256 of the ID: a fixed 64-character,
// path-separator-free token safe to use as a single filesystem path element.
// The same session ID always maps to the same namespace, so a build turn and a
// plan turn for one session share their cached blobs.
func SessionNamespace(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])
}

// isHex64 reports whether s is exactly 64 lowercase hex characters. Both a
// session namespace and a content-addressed blob key have this shape, so it
// guards every path element the reader joins.
func isHex64(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// errInvalidBlobRef reports a BlobRef whose store or key is not the expected
// fixed hex shape. Returning it (rather than touching the filesystem) keeps a
// malformed reference from ever reaching os.Open.
var errInvalidBlobRef = errors.New("invalid blob reference")

// CacheRootBlobReader reads promoted blobs from a session-scoped cache tree
// rooted at a single directory. It resolves a blob from its (store, key)
// reference alone — store is the per-session namespace, key the content
// address — so one reader serves every session behind a provider route.
//
// It is strictly read-only: it validates that store and key are fixed 64-char
// hex tokens, joins them under the cache root, and opens the existing file. It
// never creates directories, so a well-formed but nonexistent reference yields
// an error instead of materializing an empty cache namespace.
type CacheRootBlobReader struct {
	cacheRoot string
}

// NewCacheRootBlobReader returns a reader over the blob cache rooted at
// cacheRoot. cacheRoot is the same directory the session resolver writes under.
func NewCacheRootBlobReader(cacheRoot string) *CacheRootBlobReader {
	return &CacheRootBlobReader{cacheRoot: cacheRoot}
}

// blobPath validates store and key and returns the on-disk path of the blob.
// The layout mirrors the per-session LocalBlobStore: <root>/<store>/blobs/<aa>/<key>.
func (r *CacheRootBlobReader) blobPath(store, key string) (string, error) {
	if r.cacheRoot == "" || !isHex64(store) || !isHex64(key) {
		return "", errInvalidBlobRef
	}
	return filepath.Join(r.cacheRoot, store, "blobs", key[:2], key), nil
}

// StatBlob returns the size in bytes of the blob referenced by store and key.
func (r *CacheRootBlobReader) StatBlob(ctx context.Context, store, key string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	path, err := r.blobPath(store, key)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat blob: %w", err)
	}
	return info.Size(), nil
}

// OpenBlob opens the blob referenced by store and key for reading. The caller
// owns closing the returned reader.
func (r *CacheRootBlobReader) OpenBlob(ctx context.Context, store, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := r.blobPath(store, key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	return file, nil
}

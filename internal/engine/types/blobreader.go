package types

import (
	"context"
	"io"
)

// BlobReader opens provider-ready attachment bytes by primitive store and key.
type BlobReader interface {
	StatBlob(ctx context.Context, store, key string) (int64, error)
	OpenBlob(ctx context.Context, store, key string) (io.ReadCloser, error)
}

//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package skills

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestResolverRejectsFIFOWithoutBlocking(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "SKILL.md"), "body")
	fifo := filepath.Join(base, "stream")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	resolver, err := NewResolver([]Skill{{Name: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := resolver.Resolve(ctx, "skill://alpha/stream")
		result <- err
	}()
	select {
	case err := <-result:
		var resolveErr *ResolveError
		if !errors.As(err, &resolveErr) || resolveErr.Code != ResolveInvalidTarget {
			t.Fatalf("Resolve FIFO error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Resolve FIFO did not return before the deadline")
	}
}

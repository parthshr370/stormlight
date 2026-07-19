package skills

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/resource"
)

func TestResolverResolve(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "SKILL.md"), "skill body")
	mustWrite(t, filepath.Join(base, "notes", "a b.md"), "asset")
	mustWrite(t, filepath.Join(base, "notes", "plain.txt"), "plain")
	mustWrite(t, filepath.Join(base, "notes", "hash#.md"), "hash")
	if err := os.Mkdir(filepath.Join(base, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver([]Skill{{Name: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		uri  string
		want string
		code ResolveErrorCode
	}{
		{"root", "skill://alpha", "skill body", ""},
		{"root slash", "skill://alpha/", "skill body", ""},
		{"escaped asset", "skill://alpha/notes/a%20b.md", "asset", ""},
		{"unknown", "skill://ALPHA", "", ResolveUnknownSkill},
		{"userinfo", "skill://user@alpha", "", ResolveInvalidURI},
		{"query", "skill://alpha?x=1", "", ResolveInvalidURI},
		{"empty query", "skill://alpha?", "", ResolveInvalidURI},
		{"fragment", "skill://alpha#fragment", "", ResolveInvalidURI},
		{"empty fragment", "skill://alpha#", "", ResolveInvalidURI},
		{"bad escape", "skill://alpha/%zz", "", ResolveInvalidURI},
		{"nul", "skill://alpha/%00", "", ResolveInvalidPath},
		{"backslash", "skill://alpha/notes%5Csecret.md", "", ResolveInvalidPath},
		{"raw traversal", "skill://alpha/../secret", "", ResolvePathEscape},
		{"decoded traversal", "skill://alpha/%2e%2e/secret", "", ResolvePathEscape},
		{"absolute path", "skill://alpha//absolute", "", ResolveInvalidPath},
		{"directory", "skill://alpha/directory", "", ResolveInvalidTarget},
		{"missing does not fall back", "skill://alpha/missing.md", "", ResolveNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, err := resolver.Resolve(context.Background(), tc.uri)
			if tc.code != "" {
				var resolveErr *ResolveError
				if !errors.As(err, &resolveErr) || resolveErr.Code != tc.code {
					t.Fatalf("Resolve(%q) error = %v, want %s", tc.uri, err, tc.code)
				}
				if tc.code == ResolveNotFound && !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("missing error does not unwrap fs.ErrNotExist: %v", err)
				}
				return
			}
			if err != nil || string(content.Data) != tc.want || content.MediaType != resource.MarkdownMediaType {
				t.Fatalf("Resolve(%q) = %#v, %v", tc.uri, content, err)
			}
			if strings.Contains(content.URI, base) {
				t.Fatalf("resource URI leaked base path: %q", content.URI)
			}
		})
	}

	content, err := resolver.Resolve(context.Background(), "skill://alpha/notes/plain.txt")
	if err != nil {
		t.Fatal(err)
	}
	if content.MediaType != resource.TextMediaType || string(content.Data) != "plain" {
		t.Fatalf("plain asset = %#v", content)
	}
}

func TestResolverAllowsEscapedPathHash(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "SKILL.md"), "body")
	mustWrite(t, filepath.Join(base, "hash#.md"), "hash")
	resolver, err := NewResolver([]Skill{{Name: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}})
	if err != nil {
		t.Fatal(err)
	}
	content, err := resolver.Resolve(context.Background(), "skill://alpha/hash%23.md")
	if err != nil || string(content.Data) != "hash" || content.URI != "skill://alpha/hash%23.md" {
		t.Fatalf("escaped hash = %#v, %v", content, err)
	}
}

func TestResolverRejectsEscapingSymlinkAndCancellation(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(base, "SKILL.md"), "body")
	mustWrite(t, filepath.Join(outside, "secret.md"), "secret")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(base, "escape.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolver, err := NewResolver([]Skill{{Name: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(context.Background(), "skill://alpha/escape.md")
	var resolveErr *ResolveError
	if !errors.As(err, &resolveErr) || resolveErr.Code != ResolvePathEscape {
		t.Fatalf("escape error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = resolver.Resolve(ctx, "skill://alpha")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestResolverRejectsEscapingSymlinkToMissingTarget(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "SKILL.md"), "body")
	outside := filepath.Join(t.TempDir(), "missing.md")
	if err := os.Symlink(outside, filepath.Join(base, "escape.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolver, err := NewResolver([]Skill{{Name: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(context.Background(), "skill://alpha/escape.md")
	var resolveErr *ResolveError
	if !errors.As(err, &resolveErr) || resolveErr.Code != ResolvePathEscape {
		t.Fatalf("missing escaping symlink error = %v", err)
	}
}

func TestNewResolverRejectsSiblingPrefix(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "skill")
	sibling := base + "-other"
	mustWrite(t, filepath.Join(sibling, "SKILL.md"), "body")
	_, err := NewResolver([]Skill{{Name: "alpha", FilePath: filepath.Join(sibling, "SKILL.md"), BaseDir: base}})
	var resolveErr *ResolveError
	if !errors.As(err, &resolveErr) || resolveErr.Code != ResolveInvalidPath {
		t.Fatalf("NewResolver sibling path error = %v", err)
	}
}

func TestResolverReadFailure(t *testing.T) {
	base := filepath.Join(t.TempDir(), "missing")
	resolver, err := NewResolver([]Skill{{Name: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(context.Background(), "skill://alpha")
	var resolveErr *ResolveError
	if !errors.As(err, &resolveErr) || resolveErr.Code != ResolveReadFailed {
		t.Fatalf("read failure = %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

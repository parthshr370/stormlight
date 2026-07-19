package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	home := t.TempDir()
	no := false

	if got := NormalizePath("\t@~/a\u202fb\u00a0c ", PathInputOptions{Trim: true, HomeDir: home, StripAtPrefix: true, NormalizeUnicodeSpaces: true}); got != filepath.Join(home, "a b c") {
		t.Fatalf("NormalizePath tilde/unicode/@ = %q", got)
	}
	if got := NormalizePath("~/x", PathInputOptions{ExpandTilde: &no, HomeDir: home}); got != "~/x" {
		t.Fatalf("NormalizePath no tilde = %q", got)
	}
	if got := NormalizePath("file:///tmp/a%20b.txt", PathInputOptions{}); got != filepath.FromSlash("/tmp/a b.txt") {
		t.Fatalf("NormalizePath file URL = %q", got)
	}
}

func TestResolveAndCanonicalizePath(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "child")
	if got := ResolvePath("child/../child", dir, PathInputOptions{}); got != child {
		t.Fatalf("ResolvePath() = %q, want %q", got, child)
	}
	missing := filepath.Join(dir, "missing")
	if got := CanonicalizePath(missing); got != missing {
		t.Fatalf("CanonicalizePath missing = %q", got)
	}

	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := CanonicalizePath(link); got != want {
			t.Fatalf("CanonicalizePath symlink = %q, want %q", got, want)
		}
	}
}

func TestLocalAndRelativeFormatting(t *testing.T) {
	dir := t.TempDir()
	if !IsLocalPath("./x") || !IsLocalPath("file:///tmp/x") || IsLocalPath("https://example.com/x") || IsLocalPath("npm:pkg") {
		t.Fatal("IsLocalPath classification drift")
	}
	inside := filepath.Join(dir, "a", "b.txt")
	if rel, ok := GetCwdRelativePath(inside, dir); !ok || rel != filepath.Join("a", "b.txt") {
		t.Fatalf("GetCwdRelativePath inside = %q %v", rel, ok)
	}
	outside := filepath.Dir(dir)
	if _, ok := GetCwdRelativePath(outside, dir); ok {
		t.Fatal("GetCwdRelativePath outside should be false")
	}
	if got := FormatPathRelativeToCwdOrAbsolute(inside, dir); got != "a/b.txt" {
		t.Fatalf("FormatPathRelativeToCwdOrAbsolute = %q", got)
	}
}

func TestResolveReadPathVariants(t *testing.T) {
	dir := t.TempDir()

	amPMName := "Screenshot 2026-07-07 at 10.00.00" + narrowNoBreakSpace + "PM.png"
	writeFile(t, filepath.Join(dir, amPMName))
	if got := ResolveReadPath("Screenshot 2026-07-07 at 10.00.00 PM.png", dir); got != filepath.Join(dir, amPMName) {
		t.Fatalf("ResolveReadPath AM/PM variant = %q", got)
	}

	nfdName := "Cafe\u0301.txt"
	writeFile(t, filepath.Join(dir, nfdName))
	if got := ResolveReadPath("Café.txt", dir); got != filepath.Join(dir, nfdName) && got != filepath.Join(dir, "Café.txt") {
		t.Fatalf("ResolveReadPath NFD variant = %q", got)
	}
	if got := tryNFDVariant("Café.txt"); got != nfdName {
		t.Fatalf("tryNFDVariant = %q, want %q", got, nfdName)
	}

	curlyName := "Capture d\u2019écran.png"
	writeFile(t, filepath.Join(dir, curlyName))
	if got := ResolveReadPath("Capture d'écran.png", dir); got != filepath.Join(dir, curlyName) {
		t.Fatalf("ResolveReadPath curly quote variant = %q", got)
	}
}

func TestExpandPathAndResolveToCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	got := ExpandPath("@a\u202fb.txt")
	if got != "a b.txt" {
		t.Fatalf("ExpandPath = %q", got)
	}
	resolved := ResolveToCwd("@a\u202fb.txt", dir)
	if !strings.HasSuffix(resolved, filepath.Join("a b.txt")) || filepath.Dir(resolved) != dir {
		t.Fatalf("ResolveToCwd = %q", resolved)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
}

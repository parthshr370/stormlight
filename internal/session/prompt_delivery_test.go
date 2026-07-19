package session

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/document"
	ptypes "go.harness.dev/harness/internal/engine/types"
)

func writeTestBlob(t *testing.T, cacheRoot, store string, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	key := hex.EncodeToString(sum[:])
	dir := filepath.Join(cacheRoot, store, "blobs", key[:2])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, key), content, 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

func newTestDelivery(t *testing.T, cacheRoot string, contextWindow int) *MediaDelivery {
	t.Helper()
	return &MediaDelivery{
		Reader:        document.NewCacheRootBlobReader(cacheRoot),
		Registry:      document.NewAttachmentRegistry(),
		ContextWindow: contextWindow,
		Sanitize:      func(s string) string { return s },
	}
}

func TestMediaRefToBlocksSmallTextInlinesAsDocumentRef(t *testing.T) {
	store := document.SessionNamespace("sess")
	deliver := newTestDelivery(t, t.TempDir(), 200000)
	media := document.MediaRef{ID: "id-small", Filename: "notes.txt", MediaType: "text/plain", SizeBytes: 20, Blob: document.BlobRef{Store: store, Key: strings.Repeat("a", 64)}}

	blocks := mediaRefToBlocks(media, deliver)
	if len(blocks) != 1 || blocks[0].Type != ptypes.BlockDocumentRef {
		t.Fatalf("blocks = %#v, want one documentRef", blocks)
	}
	if entry, ok := deliver.Registry.Get("id-small"); !ok || !entry.TextReadable {
		t.Fatalf("registry entry = %+v ok=%v, want text-readable", entry, ok)
	}
}

func TestMediaRefToBlocksLargeTextBecomesExcerptPlusTool(t *testing.T) {
	cacheRoot := t.TempDir()
	store := document.SessionNamespace("sess")
	head := strings.Repeat("H", 20*1024)
	mid := strings.Repeat("M", 60*1024)
	tail := strings.Repeat("T", 20*1024)
	content := []byte(head + mid + tail)
	key := writeTestBlob(t, cacheRoot, store, content)
	deliver := newTestDelivery(t, cacheRoot, 200000)
	media := document.MediaRef{ID: "id-big", Filename: "big.txt", MediaType: "text/plain", SizeBytes: int64(len(content)), Blob: document.BlobRef{Store: store, Key: key}}

	blocks := mediaRefToBlocks(media, deliver)
	if len(blocks) != 1 || blocks[0].Type != ptypes.BlockText {
		t.Fatalf("blocks = %#v, want one text excerpt", blocks)
	}
	text := blocks[0].Text
	for _, want := range []string{"attachment id=id-big", "attachment tool", "UNTRUSTED", "elided"} {
		if !strings.Contains(text, want) {
			t.Fatalf("excerpt missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "H") || !strings.Contains(text, "T") {
		t.Fatalf("excerpt missing head/tail content")
	}
	if strings.Count(text, "M") > 1024 {
		t.Fatalf("excerpt inlined the middle; should be elided")
	}
	entry, ok := deliver.Registry.Get("id-big")
	if !ok || !entry.TextReadable || entry.SizeBytes != int64(len(content)) {
		t.Fatalf("registry entry = %+v ok=%v", entry, ok)
	}
}

func TestMediaRefToBlocksImageStaysNative(t *testing.T) {
	deliver := newTestDelivery(t, t.TempDir(), 200000)
	media := document.MediaRef{ID: "id-img", Filename: "chart.png", MediaType: "image/png", SizeBytes: 100, Blob: document.BlobRef{Store: document.SessionNamespace("sess"), Key: strings.Repeat("b", 64)}}

	blocks := mediaRefToBlocks(media, deliver)
	if len(blocks) != 1 || blocks[0].Type != ptypes.BlockImageRef {
		t.Fatalf("blocks = %#v, want one imageRef", blocks)
	}
	if entry, _ := deliver.Registry.Get("id-img"); entry.TextReadable {
		t.Fatalf("image entry should not be text-readable")
	}
}

func TestMediaRefToBlocksAdmissiblePDFStaysNative(t *testing.T) {
	deliver := newTestDelivery(t, t.TempDir(), 200000)
	media := document.MediaRef{ID: "id-pdf", Filename: "small.pdf", MediaType: "application/pdf", SizeBytes: 1000, PageCount: 3, Blob: document.BlobRef{Store: document.SessionNamespace("sess"), Key: strings.Repeat("c", 64)}}

	blocks := mediaRefToBlocks(media, deliver)
	if len(blocks) != 1 || blocks[0].Type != ptypes.BlockDocumentRef || blocks[0].RefPageCount != 3 {
		t.Fatalf("blocks = %#v, want native 3-page documentRef", blocks)
	}
}

func TestMediaRefToBlocksOversizePDFNotAttached(t *testing.T) {
	deliver := newTestDelivery(t, t.TempDir(), 200000)
	media := document.MediaRef{ID: "id-huge-pdf", Filename: "huge.pdf", MediaType: "application/pdf", SizeBytes: 5_000_000, PageCount: 1000, Blob: document.BlobRef{Store: document.SessionNamespace("sess"), Key: strings.Repeat("d", 64)}}

	blocks := mediaRefToBlocks(media, deliver)
	if len(blocks) != 1 || blocks[0].Type != ptypes.BlockText {
		t.Fatalf("blocks = %#v, want a text note", blocks)
	}
	if !strings.Contains(blocks[0].Text, "too large") {
		t.Fatalf("note missing 'too large': %s", blocks[0].Text)
	}
}

func TestMediaRefToBlocksNilDeliverFallsBackToNative(t *testing.T) {
	media := document.MediaRef{ID: "id-legacy", Filename: "report.pdf", MediaType: "application/pdf", SizeBytes: 22, PageCount: 3, Blob: document.BlobRef{Store: document.SessionNamespace("sess"), Key: strings.Repeat("e", 64)}}

	blocks := mediaRefToBlocks(media, nil)
	if len(blocks) != 1 || blocks[0].Type != ptypes.BlockDocumentRef || blocks[0].RefPageCount != 3 {
		t.Fatalf("blocks = %#v, want single native documentRef", blocks)
	}
}

func TestMediaRefToBlocksAggregatePDFBudgetDegradesSecond(t *testing.T) {
	deliver := newTestDelivery(t, t.TempDir(), 200000)
	store := document.SessionNamespace("sess")
	first := document.MediaRef{ID: "pdf-1", Filename: "a.pdf", MediaType: "application/pdf", SizeBytes: 1000, PageCount: 40, Blob: document.BlobRef{Store: store, Key: strings.Repeat("1", 64)}}
	second := document.MediaRef{ID: "pdf-2", Filename: "b.pdf", MediaType: "application/pdf", SizeBytes: 1000, PageCount: 40, Blob: document.BlobRef{Store: store, Key: strings.Repeat("2", 64)}}

	if b1 := mediaRefToBlocks(first, deliver); b1[0].Type != ptypes.BlockDocumentRef {
		t.Fatalf("first PDF = %#v, want native documentRef", b1)
	}
	b2 := mediaRefToBlocks(second, deliver)
	if b2[0].Type != ptypes.BlockText || !strings.Contains(b2[0].Text, "too large") {
		t.Fatalf("second PDF = %#v, want not-attached note (aggregate budget)", b2)
	}
}

func TestMediaRefToBlocksLargeTextExhaustedBudgetToolOnly(t *testing.T) {
	cacheRoot := t.TempDir()
	store := document.SessionNamespace("sess")
	content := []byte(strings.Repeat("X", 100*1024))
	key := writeTestBlob(t, cacheRoot, store, content)
	// remaining() = ContextWindow - turnReserveTokens = 100 tokens, below the
	// head+tail excerpt cost, so even the excerpt cannot be attached.
	deliver := newTestDelivery(t, cacheRoot, turnReserveTokens+100)
	media := document.MediaRef{ID: "id-exh", Filename: "big.txt", MediaType: "text/plain", SizeBytes: int64(len(content)), Blob: document.BlobRef{Store: store, Key: key}}

	blocks := mediaRefToBlocks(media, deliver)
	if len(blocks) != 1 || blocks[0].Type != ptypes.BlockText {
		t.Fatalf("blocks = %#v, want one text note", blocks)
	}
	text := blocks[0].Text
	if !strings.Contains(text, "attachment tool") || !strings.Contains(text, "Not shown inline") {
		t.Fatalf("want tool-only note, got:\n%s", text)
	}
	if strings.Contains(text, "UNTRUSTED") || strings.Contains(text, "elided") {
		t.Fatalf("should not embed excerpt when budget exhausted:\n%s", text)
	}
	if entry, ok := deliver.Registry.Get("id-exh"); !ok || !entry.TextReadable {
		t.Fatalf("registry entry = %+v ok=%v, want text-readable for tool access", entry, ok)
	}
}

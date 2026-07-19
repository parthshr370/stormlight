package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/document"
)

func writeToolBlob(t *testing.T, cacheRoot, store string, content []byte) string {
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

func runAttachment(t *testing.T, tool agent.AgentTool, params string) string {
	t.Helper()
	res, err := tool.Execute(context.Background(), "call", json.RawMessage(params), nil)
	if err != nil {
		t.Fatalf("Execute(%s) error = %v", params, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("Execute(%s) returned no content", params)
	}
	return res.Content[0].Text
}

func newAttachmentFixture(t *testing.T) (agent.AgentTool, *document.AttachmentRegistry) {
	t.Helper()
	cacheRoot := t.TempDir()
	store := document.SessionNamespace("sess")
	content := []byte("alpha line\nbeta needle line\ngamma line\n")
	key := writeToolBlob(t, cacheRoot, store, content)
	reg := document.NewAttachmentRegistry()
	reg.Put(document.AttachmentEntry{ID: "a1", Filename: "doc.txt", MediaType: "text/plain", SizeBytes: int64(len(content)), Blob: document.BlobRef{Store: store, Key: key}, TextReadable: true})
	reg.Put(document.AttachmentEntry{ID: "p1", Filename: "scan.pdf", MediaType: "application/pdf", SizeBytes: 999, PageCount: 4, Blob: document.BlobRef{Store: store, Key: strings.Repeat("f", 64)}, TextReadable: false})
	tool := newAttachmentTool(reg, document.NewCacheRootBlobReader(cacheRoot), func(s string) string { return s })
	return tool, reg
}

func TestAttachmentToolList(t *testing.T) {
	tool, _ := newAttachmentFixture(t)
	out := runAttachment(t, tool, `{"op":"list"}`)
	for _, want := range []string{"id=a1", "doc.txt", "id=p1", "scan.pdf", "pages=4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list missing %q:\n%s", want, out)
		}
	}
}

func TestAttachmentToolRead(t *testing.T) {
	tool, _ := newAttachmentFixture(t)
	out := runAttachment(t, tool, `{"op":"read","id":"a1"}`)
	if !strings.Contains(out, "beta needle line") || !strings.Contains(out, "UNTRUSTED ATTACHMENT CONTENT") {
		t.Fatalf("read output = %s", out)
	}
}

func TestAttachmentToolReadByFilename(t *testing.T) {
	tool, _ := newAttachmentFixture(t)
	out := runAttachment(t, tool, `{"op":"read","id":"doc.txt","offset":2,"limit":1}`)
	if !strings.Contains(out, "beta needle line") || strings.Contains(out, "alpha line") {
		t.Fatalf("line-range read output = %s", out)
	}
}

func TestAttachmentToolGrep(t *testing.T) {
	tool, _ := newAttachmentFixture(t)
	out := runAttachment(t, tool, `{"op":"grep","id":"a1","pattern":"needle"}`)
	if !strings.Contains(out, "2:beta needle line") {
		t.Fatalf("grep output = %s", out)
	}
	miss := runAttachment(t, tool, `{"op":"grep","id":"a1","pattern":"zzz"}`)
	if !strings.Contains(miss, "No matches") {
		t.Fatalf("grep miss output = %s", miss)
	}
}

func TestAttachmentToolBinaryNotReadable(t *testing.T) {
	tool, _ := newAttachmentFixture(t)
	out := runAttachment(t, tool, `{"op":"read","id":"p1"}`)
	if !strings.Contains(out, "not text-readable") {
		t.Fatalf("binary read output = %s", out)
	}
}

func TestAttachmentToolUnknownID(t *testing.T) {
	tool, _ := newAttachmentFixture(t)
	out := runAttachment(t, tool, `{"op":"read","id":"nope"}`)
	if !strings.Contains(out, "No attachment") {
		t.Fatalf("unknown-id output = %s", out)
	}
}

func newCSVFixture(t *testing.T, rows int) agent.AgentTool {
	t.Helper()
	cacheRoot := t.TempDir()
	store := document.SessionNamespace("csvsess")
	lines := make([]string, 0, rows+1)
	lines = append(lines, "id,amount,date")
	for i := 1; i <= rows; i++ {
		lines = append(lines, fmt.Sprintf("%d,%d,2026-05-%02d", i, i*10, (i%28)+1))
	}
	content := []byte(strings.Join(lines, "\n"))
	key := writeToolBlob(t, cacheRoot, store, content)
	reg := document.NewAttachmentRegistry()
	reg.Put(document.AttachmentEntry{ID: "csv1", Filename: "export.csv", MediaType: "text/csv", SizeBytes: int64(len(content)), Blob: document.BlobRef{Store: store, Key: key}, TextReadable: true})
	return newAttachmentTool(reg, document.NewCacheRootBlobReader(cacheRoot), func(s string) string { return s })
}

func TestAttachmentReadPagesDeterministically(t *testing.T) {
	tool := newCSVFixture(t, 500) // 501 total lines: header + 500 rows, no trailing newline
	first := runAttachment(t, tool, `{"op":"read","id":"csv1","offset":1,"limit":100}`)
	if !strings.Contains(first, "[Showing lines 1-100 of 501. Use offset=101 to read the next page.]") {
		t.Fatalf("first page footer wrong:\n%s", first)
	}
	last := runAttachment(t, tool, `{"op":"read","id":"csv1","offset":501,"limit":100}`)
	if !strings.Contains(last, "of 501 (end of file)") {
		t.Fatalf("last page footer wrong:\n%s", last)
	}
	res, err := tool.Execute(context.Background(), "call", json.RawMessage(`{"op":"read","id":"csv1","offset":1,"limit":10}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if details := resultDetailsMap(t, res); details["total_lines"] != float64(501) {
		t.Fatalf("total_lines = %v, want 501", details["total_lines"])
	}
}

func TestCountLines(t *testing.T) {
	for in, want := range map[string]int{"": 0, "a": 1, "a\nb": 2, "a\nb\n": 3} {
		if got := countLines(in); got != want {
			t.Fatalf("countLines(%q) = %d, want %d", in, got, want)
		}
	}
}

func newStatsCSVFixture(t *testing.T, csv string) agent.AgentTool {
	t.Helper()
	cacheRoot := t.TempDir()
	store := document.SessionNamespace("statssess")
	content := []byte(csv)
	key := writeToolBlob(t, cacheRoot, store, content)
	reg := document.NewAttachmentRegistry()
	reg.Put(document.AttachmentEntry{ID: "s1", Filename: "data.csv", MediaType: "text/csv", SizeBytes: int64(len(content)), Blob: document.BlobRef{Store: store, Key: key}, TextReadable: true})
	return newAttachmentTool(reg, document.NewCacheRootBlobReader(cacheRoot), func(s string) string { return s })
}

func TestAttachmentStats(t *testing.T) {
	tool := newStatsCSVFixture(t, "region,amount\nnorth,100\nsouth,50\nnorth,25\nsouth,75\neast,10")

	base := runAttachment(t, tool, `{"op":"stats","id":"s1"}`)
	for _, want := range []string{"Rows: 5 (excluding the header row).", "Columns (2): region, amount"} {
		if !strings.Contains(base, want) {
			t.Fatalf("stats base missing %q:\n%s", want, base)
		}
	}

	val := runAttachment(t, tool, `{"op":"stats","id":"s1","value":"amount"}`)
	if !strings.Contains(val, "sum=260 min=10 max=100 mean=52") {
		t.Fatalf("value stats wrong:\n%s", val)
	}

	grp := runAttachment(t, tool, `{"op":"stats","id":"s1","group_by":"region","value":"amount"}`)
	for _, want := range []string{
		`By "region" (top 3 of 3 group(s), most rows first):`,
		"- north: count=2 sum=125 mean=62.5",
		"- south: count=2 sum=125 mean=62.5",
		"- east: count=1 sum=10 mean=10",
	} {
		if !strings.Contains(grp, want) {
			t.Fatalf("group stats missing %q:\n%s", want, grp)
		}
	}
	if strings.Index(grp, "north") > strings.Index(grp, "south") {
		t.Fatal("count tie should sort north before south")
	}

	res, err := tool.Execute(context.Background(), "call", json.RawMessage(`{"op":"stats","id":"s1","group_by":"region"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	d := resultDetailsMap(t, res)
	if d["total_rows"] != float64(5) || d["group_count"] != float64(3) {
		t.Fatalf("details = %+v", d)
	}

	miss := runAttachment(t, tool, `{"op":"stats","id":"s1","group_by":"nope"}`)
	if !strings.Contains(miss, "not found") {
		t.Fatalf("unknown column output = %s", miss)
	}
}

func TestParseNumericCell(t *testing.T) {
	cases := map[string]struct {
		f  float64
		ok bool
	}{
		"":        {0, false},
		"42":      {42, true},
		"1,234.5": {1234.5, true},
		"$50":     {50, true},
		"20%":     {20, true},
		"abc":     {0, false},
	}
	for in, want := range cases {
		f, ok := parseNumericCell(in)
		if ok != want.ok || (ok && f != want.f) {
			t.Fatalf("parseNumericCell(%q) = (%v,%v), want (%v,%v)", in, f, ok, want.f, want.ok)
		}
	}
}

func TestResolveColumn(t *testing.T) {
	h := []string{"Region", "Amount", "Date"}
	for spec, want := range map[string]int{"": -1, "amount": 1, "AMOUNT": 1, "2": 1, "0": -1, "9": -1, "nope": -1, "Date": 2} {
		if got := resolveColumn(h, spec); got != want {
			t.Fatalf("resolveColumn(%q) = %d, want %d", spec, got, want)
		}
	}
}

func TestSniffDelimiter(t *testing.T) {
	if sniffDelimiter("x.tsv", "a\tb") != '\t' {
		t.Fatal("tsv extension should be tab")
	}
	if sniffDelimiter("x.csv", "a,b,c") != ',' {
		t.Fatal("csv should be comma")
	}
	if sniffDelimiter("x.txt", "a\tb\tc") != '\t' {
		t.Fatal("tab-majority first line should sniff tab")
	}
}

package document

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractOOXMLTextDOCX(t *testing.T) {
	path := writeOOXMLFixture(t, []ooxmlTestPart{
		{name: "[Content_Types].xml", data: "<Types/>"},
		{name: "word/document.xml", data: `<w:document xmlns:w="urn:word"><w:body><w:p><w:r><w:t>Hello Ledger</w:t></w:r></w:p></w:body></w:document>`},
	})

	text, err := ExtractOOXMLText(path, docxMediaType)
	if err != nil {
		t.Fatalf("ExtractOOXMLText() error = %v", err)
	}
	if !strings.Contains(text, "Hello Ledger") {
		t.Fatalf("ExtractOOXMLText() text = %q, want Hello Ledger", text)
	}
}

func TestExtractOOXMLTextPPTX(t *testing.T) {
	path := writeOOXMLFixture(t, []ooxmlTestPart{
		{name: "[Content_Types].xml", data: "<Types/>"},
		{name: "ppt/slides/slide2.xml", data: `<p:sld xmlns:p="urn:presentation" xmlns:a="urn:drawing"><a:t>Second slide</a:t></p:sld>`},
		{name: "ppt/slides/slide1.xml", data: `<p:sld xmlns:p="urn:presentation" xmlns:a="urn:drawing"><a:t>First slide</a:t></p:sld>`},
	})

	text, err := ExtractOOXMLText(path, pptxMediaType)
	if err != nil {
		t.Fatalf("ExtractOOXMLText() error = %v", err)
	}
	first := strings.Index(text, "First slide")
	second := strings.Index(text, "Second slide")
	if first < 0 || second < 0 || first >= second {
		t.Fatalf("ExtractOOXMLText() text = %q, want ordered slide text", text)
	}
}

func TestExtractOOXMLTextXLSX(t *testing.T) {
	path := writeOOXMLFixture(t, []ooxmlTestPart{
		{name: "[Content_Types].xml", data: "<Types/>"},
		{name: "xl/sharedStrings.xml", data: `<sst xmlns="urn:spreadsheet"><si><r><t>Shared Ledger</t></r></si></sst>`},
		{name: "xl/worksheets/sheet1.xml", data: `<worksheet xmlns="urn:spreadsheet"><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1"><v>42</v></c></row></sheetData></worksheet>`},
	})

	text, err := ExtractOOXMLText(path, xlsxMediaType)
	if err != nil {
		t.Fatalf("ExtractOOXMLText() error = %v", err)
	}
	if !strings.Contains(text, "Shared Ledger") || !strings.Contains(text, "42") {
		t.Fatalf("ExtractOOXMLText() text = %q, want spreadsheet cell values", text)
	}
}

func TestExtractOOXMLTextXLSXInlineStrings(t *testing.T) {
	path := writeOOXMLFixture(t, []ooxmlTestPart{
		{name: "[Content_Types].xml", data: "<Types/>"},
		{name: "xl/worksheets/sheet1.xml", data: `<worksheet xmlns="urn:spreadsheet"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Inline Ledger</t></is></c></row></sheetData></worksheet>`},
	})

	text, err := ExtractOOXMLText(path, xlsxMediaType)
	if err != nil {
		t.Fatalf("ExtractOOXMLText() error = %v", err)
	}
	if !strings.Contains(text, "Inline Ledger") {
		t.Fatalf("ExtractOOXMLText() text = %q, want inline spreadsheet cell value", text)
	}
}

func TestExtractOOXMLTextXLSXPreservesEmptyColumns(t *testing.T) {
	path := writeOOXMLFixture(t, []ooxmlTestPart{
		{name: "[Content_Types].xml", data: "<Types/>"},
		{name: "xl/worksheets/sheet1.xml", data: `<worksheet xmlns="urn:spreadsheet"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>A</t></is></c><c r="C1" t="inlineStr"><is><t>C</t></is></c></row></sheetData></worksheet>`},
	})

	text, err := ExtractOOXMLText(path, xlsxMediaType)
	if err != nil {
		t.Fatalf("ExtractOOXMLText() error = %v", err)
	}
	if !strings.Contains(text, "A\t\tC") {
		t.Fatalf("ExtractOOXMLText() text = %q, want empty B column preserved", text)
	}
}

func TestExtractOOXMLTextRejectsCorruptDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.docx")
	if err := os.WriteFile(path, []byte("not a zip document"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractOOXMLText(path, docxMediaType)
	requireExtractDocumentErrorCode(t, err, CodeCorruptOrEncrypted)
}

func TestExtractOOXMLTextEnforcesDecodedByteLimit(t *testing.T) {
	path := writeOOXMLFixture(t, []ooxmlTestPart{
		{name: "[Content_Types].xml", data: "<Types/>"},
		{name: "word/document.xml", data: `<w:document xmlns:w="urn:word"><w:body><w:p>` + strings.Repeat("<w:t>x</w:t>", 64) + `</w:p></w:body></w:document>`},
	})

	_, err := extractOOXMLTextWithLimit(path, docxMediaType, 128, maxOOXMLExtractedBytes)
	requireExtractDocumentErrorCode(t, err, CodeCorruptOrEncrypted)
}

func TestExtractOOXMLTextEnforcesOutputLimit(t *testing.T) {
	path := writeOOXMLFixture(t, []ooxmlTestPart{
		{name: "[Content_Types].xml", data: "<Types/>"},
		{name: "word/document.xml", data: `<w:document xmlns:w="urn:word"><w:body><w:p><w:t>more than five bytes</w:t></w:p></w:body></w:document>`},
	})

	_, err := extractOOXMLTextWithLimit(path, docxMediaType, maxOOXMLDecodedBytes, 5)
	requireExtractDocumentErrorCode(t, err, CodeRequestSizeExceeded)
}

type ooxmlTestPart struct {
	name string
	data string
}

func writeOOXMLFixture(t *testing.T, parts []ooxmlTestPart) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, part := range parts {
		entry, err := writer.Create(part.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(part.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireExtractDocumentErrorCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	var documentErr *DocumentError
	if !errors.As(err, &documentErr) {
		t.Fatalf("error = %v, want *DocumentError", err)
	}
	if documentErr.Code != want {
		t.Fatalf("DocumentError.Code = %q, want %q", documentErr.Code, want)
	}
}

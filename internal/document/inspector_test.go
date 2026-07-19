package document

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
)

func TestInspectPDFPageCount(t *testing.T) {
	for _, test := range []struct {
		name      string
		pageCount int
	}{
		{name: "one page", pageCount: 1},
		{name: "multiple pages", pageCount: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeInspectionFile(t, buildTestPDF(test.pageCount))
			mediaType, pageCount, err := Inspect(path, DocumentHints{
				Filename:          "report.pdf",
				DeclaredMediaType: "application/pdf",
			})
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if mediaType != "application/pdf" {
				t.Fatalf("Inspect() mediaType = %q, want application/pdf", mediaType)
			}
			if pageCount != test.pageCount {
				t.Fatalf("Inspect() pageCount = %d, want %d", pageCount, test.pageCount)
			}
		})
	}
}

func TestInspectRejectsCorruptPDF(t *testing.T) {
	path := writeInspectionFile(t, []byte("%PDF-1.4\nnot a complete PDF"))
	_, _, err := Inspect(path, DocumentHints{DeclaredMediaType: "application/pdf"})
	requireDocumentErrorCode(t, err, CodeCorruptOrEncrypted)
}

func TestInspectPDFEnforcesDecodedByteBudget(t *testing.T) {
	// Three pages of ~4 decoded content bytes each exceed a 5-byte cumulative
	// budget, so the bound rejects the document (the decompression-bomb guard).
	overBudget := writeInspectionFile(t, buildTestPDF(3))
	_, err := inspectPDFWithLimit(overBudget, 5)
	requireDocumentErrorCode(t, err, CodeCorruptOrEncrypted)

	// A generous budget accepts the same document.
	withinBudget := writeInspectionFile(t, buildTestPDF(3))
	pageCount, err := inspectPDFWithLimit(withinBudget, maxPDFDecodedBytes)
	if err != nil {
		t.Fatalf("inspectPDFWithLimit() within budget error = %v", err)
	}
	if pageCount != 3 {
		t.Fatalf("inspectPDFWithLimit() pageCount = %d, want 3", pageCount)
	}
}

func TestInspectPNG(t *testing.T) {
	var encoded bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 3, 2))
	picture.Set(1, 1, color.RGBA{R: 20, G: 40, B: 60, A: 255})
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}

	path := writeInspectionFile(t, encoded.Bytes())
	mediaType, pageCount, err := Inspect(path, DocumentHints{Filename: "picture.png"})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if mediaType != "image/png" {
		t.Fatalf("Inspect() mediaType = %q, want image/png", mediaType)
	}
	if pageCount != 0 {
		t.Fatalf("Inspect() pageCount = %d, want 0", pageCount)
	}
}

func TestInspectWebP(t *testing.T) {
	path := writeInspectionFile(t, testWebPFixture(t))
	mediaType, pageCount, err := Inspect(path, DocumentHints{Filename: "picture.webp"})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if mediaType != "image/webp" {
		t.Fatalf("Inspect() mediaType = %q, want image/webp", mediaType)
	}
	if pageCount != 0 {
		t.Fatalf("Inspect() pageCount = %d, want 0", pageCount)
	}
}

func TestInspectRejectsTruncatedWebP(t *testing.T) {
	fixture := testWebPFixture(t)
	path := writeInspectionFile(t, fixture[:30])
	_, _, err := Inspect(path, DocumentHints{DeclaredMediaType: "image/webp"})
	requireDocumentErrorCode(t, err, CodeCorruptOrEncrypted)
}

func TestInspectRejectsDeclaredPDFWithPNGBytes(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}

	path := writeInspectionFile(t, encoded.Bytes())
	_, _, err := Inspect(path, DocumentHints{DeclaredMediaType: "application/pdf"})
	requireDocumentErrorCode(t, err, CodeUnsupportedMediaType)
}

func TestInspectRejectsImagePixelBomb(t *testing.T) {
	path := writeInspectionFile(t, pngWithDimensions(maxImageDimension+1, 1))
	_, _, err := Inspect(path, DocumentHints{DeclaredMediaType: "image/png"})
	requireDocumentErrorCode(t, err, CodeCorruptOrEncrypted)
}
func TestInspectOOXML(t *testing.T) {
	const mediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

	path := writeInspectionFile(t, buildTestDOCX(t, true))
	gotMediaType, pageCount, err := Inspect(path, DocumentHints{DeclaredMediaType: mediaType})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if gotMediaType != mediaType {
		t.Fatalf("Inspect() mediaType = %q, want %q", gotMediaType, mediaType)
	}
	if pageCount != 0 {
		t.Fatalf("Inspect() pageCount = %d, want 0", pageCount)
	}
}

func TestInspectRejectsRenamedZIP(t *testing.T) {
	const mediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

	path := writeInspectionFile(t, buildTestDOCX(t, false))
	_, _, err := Inspect(path, DocumentHints{DeclaredMediaType: mediaType})
	requireDocumentErrorCode(t, err, CodeCorruptOrEncrypted)
}

func TestInspectRejectsDeclaredOOXMLWithNonZIPBytes(t *testing.T) {
	const mediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

	path := writeInspectionFile(t, []byte("not an OOXML package"))
	_, _, err := Inspect(path, DocumentHints{DeclaredMediaType: mediaType})
	requireDocumentErrorCode(t, err, CodeUnsupportedMediaType)
}

func writeInspectionFile(t *testing.T, data []byte) string {
	t.Helper()
	path := t.TempDir() + "/attachment"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	return path
}

func testWebPFixture(t *testing.T) []byte {
	t.Helper()
	fixture, err := base64.StdEncoding.DecodeString(
		"UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZ" +
			"lgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAX" +
			"FOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5" +
			"I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8y" +
			"ZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wO" +
			"K3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8J" +
			"kfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvs" +
			"hXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA==",
	)
	if err != nil {
		t.Fatalf("base64.DecodeString() error = %v", err)
	}
	return fixture
}

func buildTestDOCX(t *testing.T, includeMainPart bool) []byte {
	t.Helper()
	var output bytes.Buffer
	packageWriter := zip.NewWriter(&output)
	contentTypes, err := packageWriter.Create("[Content_Types].xml")
	if err != nil {
		t.Fatalf("zip.Create(content types) error = %v", err)
	}
	_, err = contentTypes.Write([]byte(
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Override PartName="/word/document.xml" ` +
			`ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
			`</Types>`,
	))
	if err != nil {
		t.Fatalf("contentTypes.Write() error = %v", err)
	}
	if includeMainPart {
		document, err := packageWriter.Create("word/document.xml")
		if err != nil {
			t.Fatalf("zip.Create(document) error = %v", err)
		}
		if _, err := document.Write([]byte(`<w:document xmlns:w="urn:test"/>`)); err != nil {
			t.Fatalf("document.Write() error = %v", err)
		}
	}
	if err := packageWriter.Close(); err != nil {
		t.Fatalf("zip.Close() error = %v", err)
	}
	return output.Bytes()
}

func buildTestPDF(pageCount int) []byte {
	objectCount := 2 + pageCount*2
	objects := make([]string, objectCount+1)
	objects[1] = "<< /Type /Catalog /Pages 2 0 R >>"

	var kids strings.Builder
	for pageIndex := range pageCount {
		pageObject := 3 + pageIndex*2
		fmt.Fprintf(&kids, "%d 0 R ", pageObject)
		contentsObject := pageObject + 1
		objects[pageObject] = fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 72 72] /Contents %d 0 R >>",
			contentsObject,
		)
		objects[contentsObject] = "<< /Length 4 >>\nstream\nq\nQ\nendstream"
	}
	objects[2] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), pageCount)

	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n%test\n")
	offsets := make([]int, objectCount+1)
	for objectNumber := 1; objectNumber <= objectCount; objectNumber++ {
		offsets[objectNumber] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", objectNumber, objects[objectNumber])
	}
	xrefOffset := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n", objectCount+1)
	output.WriteString("0000000000 65535 f \n")
	for objectNumber := 1; objectNumber <= objectCount; objectNumber++ {
		fmt.Fprintf(&output, "%010d 00000 n \n", offsets[objectNumber])
	}
	fmt.Fprintf(
		&output,
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		objectCount+1,
		xrefOffset,
	)
	return output.Bytes()
}

func pngWithDimensions(width, height uint32) []byte {
	var output bytes.Buffer
	output.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8
	ihdr[9] = 2
	writePNGChunk(&output, "IHDR", ihdr)
	writePNGChunk(&output, "IEND", nil)
	return output.Bytes()
}

func writePNGChunk(output *bytes.Buffer, chunkType string, data []byte) {
	_ = binary.Write(output, binary.BigEndian, uint32(len(data)))
	output.WriteString(chunkType)
	output.Write(data)
	checksum := crc32.NewIEEE()
	_, _ = checksum.Write([]byte(chunkType))
	_, _ = checksum.Write(data)
	_ = binary.Write(output, binary.BigEndian, checksum.Sum32())
}

func requireDocumentErrorCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	var documentErr *DocumentError
	if !errors.As(err, &documentErr) {
		t.Fatalf("error = %v, want *DocumentError", err)
	}
	if documentErr.Code != want {
		t.Fatalf("DocumentError.Code = %q, want %q", documentErr.Code, want)
	}
}

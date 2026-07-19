package document

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/webp"
	"rsc.io/pdf"
)

const (
	maxImageDimension = 20_000
	maxImagePixels    = 100_000_000
	// maxPDFDecodedBytes caps cumulative decoded page-content bytes during PDF
	// inspection, bounding decompression bombs where a small compressed stream
	// inflates to an arbitrarily large size.
	maxPDFDecodedBytes = 256 << 20
)

// DocumentHints describes untrusted metadata that can help identify a document.
type DocumentHints struct {
	// Filename is the attachment's display name.
	Filename string
	// DeclaredMediaType is the media type supplied by the attachment source.
	DeclaredMediaType string
}

// Inspect validates a staged attachment and returns its authoritative media type and page count.
func Inspect(path string, hints DocumentHints) (mediaType string, pageCount int, err error) {
	header, err := readInspectionHeader(path)
	if err != nil {
		return "", 0, corruptDocumentError()
	}

	detected := detectMediaType(header)
	declared := normalizeMediaType(hints.DeclaredMediaType)
	if declared == "" {
		declared = normalizeMediaType(mime.TypeByExtension(strings.ToLower(filepath.Ext(hints.Filename))))
	}
	if declaredMediaConflict(declared, detected) {
		return "", 0, NewDocumentError(
			CodeUnsupportedMediaType,
			"The attachment type does not match its contents.",
			nil,
			nil,
		)
	}

	switch detected {
	case "application/pdf":
		pages, inspectErr := inspectPDF(path)
		if inspectErr != nil {
			return "", 0, inspectErr
		}
		return detected, pages, nil
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		if inspectErr := inspectImage(path, detected); inspectErr != nil {
			return "", 0, inspectErr
		}
		return detected, 0, nil
	case "application/zip":
		if isOOXMLMediaType(declared) {
			if inspectErr := inspectOOXML(path, declared); inspectErr != nil {
				return "", 0, inspectErr
			}
			return declared, 0, nil
		}
	case "application/octet-stream":
		if strings.HasPrefix(declared, "text/") {
			return declared, 0, nil
		}
	default:
		if strings.HasPrefix(detected, "text/") {
			if strings.HasPrefix(declared, "text/") {
				return declared, 0, nil
			}
			return detected, 0, nil
		}
	}

	return "", 0, NewDocumentError(
		CodeUnsupportedMediaType,
		"The attachment type is not supported.",
		nil,
		nil,
	)
}

func readInspectionHeader(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	header := make([]byte, 512)
	n, err := io.ReadFull(file, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return header[:n], nil
}

func detectMediaType(header []byte) string {
	switch {
	case bytes.HasPrefix(header, []byte("%PDF")):
		return "application/pdf"
	case bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(header) >= 3 && header[0] == 0xff && header[1] == 0xd8 && header[2] == 0xff:
		return "image/jpeg"
	case bytes.HasPrefix(header, []byte("GIF87a")), bytes.HasPrefix(header, []byte("GIF89a")):
		return "image/gif"
	case len(header) >= 12 && bytes.Equal(header[:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP")):
		return "image/webp"
	case bytes.HasPrefix(header, []byte("PK\x03\x04")), bytes.HasPrefix(header, []byte("PK\x05\x06")), bytes.HasPrefix(header, []byte("PK\x07\x08")):
		return "application/zip"
	default:
		return normalizeMediaType(http.DetectContentType(header))
	}
}

func normalizeMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	mediaType = strings.ToLower(mediaType)
	if mediaType == "image/jpg" {
		return "image/jpeg"
	}
	return mediaType
}

func declaredMediaConflict(declared, detected string) bool {
	if declared == "application/pdf" {
		return detected != "application/pdf"
	}
	if strings.HasPrefix(declared, "image/") {
		return declared != detected
	}
	if isOOXMLMediaType(declared) {
		return detected != "application/zip"
	}
	return false
}

// inspectOOXML proves this ZIP package contains the declared document kind before we accept it as OOXML.
func inspectOOXML(path, mediaType string) error {
	mainPart := ""
	mainContentType := ""
	mainRoot := ""
	switch mediaType {
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		mainPart = "word/document.xml"
		mainContentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"
		mainRoot = "document"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		mainPart = "xl/workbook.xml"
		mainContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
		mainRoot = "workbook"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		mainPart = "ppt/presentation.xml"
		mainContentType = "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"
		mainRoot = "presentation"
	default:
		return corruptDocumentError()
	}

	packageReader, err := zip.OpenReader(path)
	if err != nil {
		return corruptDocumentError()
	}
	defer packageReader.Close()

	var contentTypesFile *zip.File
	var mainPartFile *zip.File
	for _, file := range packageReader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		switch file.Name {
		case "[Content_Types].xml":
			if contentTypesFile != nil {
				return corruptDocumentError()
			}
			contentTypesFile = file
		case mainPart:
			if mainPartFile != nil {
				return corruptDocumentError()
			}
			mainPartFile = file
		}
	}
	if contentTypesFile == nil || mainPartFile == nil ||
		!validOOXMLContentTypes(contentTypesFile, "/"+mainPart, mainContentType) ||
		!validOOXMLMainPart(mainPartFile, mainRoot) {
		return corruptDocumentError()
	}
	return nil
}

// validOOXMLContentTypes requires the main part's exact override instead of trusting a ZIP entry name alone.
func validOOXMLContentTypes(file *zip.File, mainPart, mainContentType string) bool {
	const maxContentTypesBytes = 1 << 20

	reader, err := file.Open()
	if err != nil {
		return false
	}
	defer reader.Close()

	limited := &io.LimitedReader{R: reader, N: maxContentTypesBytes + 1}
	decoder := xml.NewDecoder(limited)
	hasMatchingOverride := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return limited.N > 0 && hasMatchingOverride
		}
		if err != nil {
			return false
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "Override" {
			continue
		}
		partName := ""
		contentType := ""
		for _, attribute := range start.Attr {
			switch attribute.Name.Local {
			case "PartName":
				partName = attribute.Value
			case "ContentType":
				contentType = attribute.Value
			}
		}
		if partName == mainPart && contentType == mainContentType {
			hasMatchingOverride = true
		}
	}
}

// validOOXMLMainPart reads the bounded XML part to EOF so a valid opening tag can't hide malformed trailing content.
func validOOXMLMainPart(file *zip.File, rootName string) bool {
	const maxMainPartBytes = 64 << 20

	reader, err := file.Open()
	if err != nil {
		return false
	}
	defer reader.Close()

	limited := &io.LimitedReader{R: reader, N: maxMainPartBytes + 1}
	decoder := xml.NewDecoder(limited)
	foundRoot := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return limited.N > 0 && foundRoot
		}
		if err != nil {
			return false
		}
		if start, ok := token.(xml.StartElement); ok && !foundRoot {
			if start.Name.Local != rootName {
				return false
			}
			foundRoot = true
		}
	}
}

func isOOXMLMediaType(mediaType string) bool {
	switch mediaType {
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return true
	default:
		return false
	}
}

func inspectPDF(path string) (int, error) {
	return inspectPDFWithLimit(path, maxPDFDecodedBytes)
}

// inspectPDFWithLimit contains parser panics and drains content streams so encrypted and decompression-bomb PDFs don't reach tools.
func inspectPDFWithLimit(path string, maxDecoded int64) (pageCount int, err error) {
	defer func() {
		if recover() != nil {
			pageCount = 0
			err = corruptDocumentError()
		}
	}()

	file, openErr := os.Open(path)
	if openErr != nil {
		return 0, corruptDocumentError()
	}
	defer file.Close()

	info, statErr := file.Stat()
	if statErr != nil {
		return 0, corruptDocumentError()
	}
	reader, openErr := pdf.NewReader(file, info.Size())
	if openErr != nil {
		return 0, corruptDocumentError()
	}
	if !reader.Trailer().Key("Encrypt").IsNull() {
		return 0, corruptDocumentError()
	}

	pageCount = reader.NumPage()
	if pageCount < 0 || int64(pageCount) > info.Size() {
		return 0, corruptDocumentError()
	}
	remaining := maxDecoded
	for pageNumber := 1; pageNumber <= pageCount; pageNumber++ {
		page := reader.Page(pageNumber)
		if page.V.IsNull() {
			return 0, corruptDocumentError()
		}
		if walkErr := drainPDFContents(page.V.Key("Contents"), &remaining); walkErr != nil {
			return 0, corruptDocumentError()
		}
	}
	return pageCount, nil
}

func drainPDFContents(contents pdf.Value, remaining *int64) error {
	switch contents.Kind() {
	case pdf.Null:
		return nil
	case pdf.Stream:
		return drainPDFStream(contents, remaining)
	case pdf.Array:
		for i := range contents.Len() {
			stream := contents.Index(i)
			if stream.Kind() != pdf.Stream {
				return io.ErrUnexpectedEOF
			}
			if err := drainPDFStream(stream, remaining); err != nil {
				return err
			}
		}
		return nil
	default:
		return io.ErrUnexpectedEOF
	}
}

// drainPDFStream reads one decoded content stream, bounding cumulative decoded
// bytes across the page walk via remaining. A stream that inflates past the
// budget (a decompression bomb) is rejected rather than fully materialized.
func drainPDFStream(stream pdf.Value, remaining *int64) error {
	reader := stream.Reader()
	n, readErr := io.Copy(io.Discard, io.LimitReader(reader, *remaining+1))
	closeErr := reader.Close()
	if readErr != nil {
		return readErr
	}
	if n > *remaining {
		return io.ErrUnexpectedEOF
	}
	*remaining -= n
	return closeErr
}

// inspectImage fully decodes after reading metadata, catching bad pixel data that DecodeConfig can accept.
func inspectImage(path, mediaType string) error {
	file, err := os.Open(path)
	if err != nil {
		return corruptDocumentError()
	}
	defer file.Close()

	config, format, err := image.DecodeConfig(file)
	if err != nil || imageFormatMediaType(format) != mediaType || !validImageDimensions(config.Width, config.Height) {
		return corruptDocumentError()
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return corruptDocumentError()
	}
	decoded, format, err := image.Decode(file)
	if err != nil || imageFormatMediaType(format) != mediaType {
		return corruptDocumentError()
	}
	bounds := decoded.Bounds()
	if !validImageDimensions(bounds.Dx(), bounds.Dy()) {
		return corruptDocumentError()
	}
	return nil
}

func imageFormatMediaType(format string) string {
	if format == "jpeg" {
		return "image/jpeg"
	}
	if format == "png" || format == "gif" || format == "webp" {
		return "image/" + format
	}
	return ""
}

func validImageDimensions(width, height int) bool {
	if width <= 0 || height <= 0 || width > maxImageDimension || height > maxImageDimension {
		return false
	}
	return int64(width)*int64(height) <= maxImagePixels
}

func corruptDocumentError() *DocumentError {
	return NewDocumentError(
		CodeCorruptOrEncrypted,
		"The attachment is corrupt or encrypted.",
		nil,
		nil,
	)
}

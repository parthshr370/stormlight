//go:build anthropic_smoke

package smoke

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/anthropic"
)

const (
	smokeDOCXMediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	smokeXLSXMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	smokePPTXMediaType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

func TestAnthropicSmoke_ReadsAttachments(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_OAUTH_TOKEN") == "" {
		t.Skip("set ANTHROPIC_API_KEY to run")
	}

	modelID := smokeFirstNonEmpty(os.Getenv("ANTHROPIC_DEFAULT_SONNET_MODEL"), "claude-opus-4-8")
	baseURL := smokeFirstNonEmpty(os.Getenv("HARNESS_ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_BASE_URL"), "https://api.anthropic.com")
	apiKey := smokeFirstNonEmpty(os.Getenv("ANTHROPIC_OAUTH_TOKEN"), os.Getenv("ANTHROPIC_API_KEY"))
	model := types.Model{
		ID:            modelID,
		Name:          modelID,
		API:           "anthropic-messages",
		Provider:      "anthropic",
		BaseURL:       baseURL,
		ContextWindow: 200000,
		MaxTokens:     32000,
	}

	facts := map[string]string{
		"txt":  "BLUE-HERON-42",
		"docx": "CRIMSON-FALCON-88",
		"xlsx": "AMBER-LYNX-31",
		"pptx": "SILVER-OTTER-56",
	}
	for _, name := range []string{"txt", "docx", "xlsx", "pptx"} {
		t.Run(name, func(t *testing.T) {
			fact := facts[name]
			textBytes := smokeDeliveredText(t, name, fact)
			if !strings.Contains(string(textBytes), fact) {
				t.Fatalf("delivered text = %q, want fact %q", textBytes, fact)
			}

			cacheRoot := t.TempDir()
			store := document.SessionNamespace("smoke")
			sum := sha256.Sum256(textBytes)
			key := hex.EncodeToString(sum[:])
			blobPath := filepath.Join(cacheRoot, store, "blobs", key[:2], key)
			if err := os.MkdirAll(filepath.Dir(blobPath), 0o700); err != nil {
				t.Fatalf("MkdirAll blob directory: %v", err)
			}
			if err := os.WriteFile(blobPath, textBytes, 0o600); err != nil {
				t.Fatalf("WriteFile blob: %v", err)
			}

			reader := document.NewCacheRootBlobReader(cacheRoot)
			request := types.Context{Messages: []types.Message{types.UserMessage{Content: types.BlockContent(
				types.NewText("What is the codename mentioned in the attached document? Answer with just the codename."),
				types.NewDocumentRef(store, key, "text/plain", name+".txt", int64(len(textBytes)), 0),
			)}}}
			assistant := anthropic.StreamSimple(context.Background(), model, request, &types.SimpleStreamOptions{
				StreamOptions: types.StreamOptions{APIKey: apiKey},
				BlobReader:    reader,
			})
			for range assistant.Events() {
			}
			message, err := assistant.Result(context.Background())
			if err != nil {
				t.Fatalf("Anthropic stream result: %v", err)
			}
			if message == nil {
				t.Fatal("Anthropic stream result was nil")
			}

			var answer strings.Builder
			for _, block := range message.Content {
				if block.Type == types.BlockText {
					answer.WriteString(block.Text)
				}
			}
			got := answer.String()
			t.Logf("model answer: %s", got)
			if !strings.Contains(got, fact) {
				t.Fatalf("model answer = %q, want fact %q (stop reason %q, error %q)", got, fact, message.StopReason, message.ErrorMessage)
			}
		})
	}
}

func smokeDeliveredText(t *testing.T, format, fact string) []byte {
	t.Helper()
	if format == "txt" {
		return []byte("Project codename: " + fact)
	}

	var mediaType string
	var parts []smokeOOXMLPart
	switch format {
	case "docx":
		mediaType = smokeDOCXMediaType
		parts = []smokeOOXMLPart{
			{name: "[Content_Types].xml", data: "<Types/>"},
			{name: "word/document.xml", data: `<w:document xmlns:w="urn:word"><w:body><w:p><w:r><w:t>` + fact + `</w:t></w:r></w:p></w:body></w:document>`},
		}
	case "xlsx":
		mediaType = smokeXLSXMediaType
		parts = []smokeOOXMLPart{
			{name: "[Content_Types].xml", data: "<Types/>"},
			{name: "xl/sharedStrings.xml", data: `<sst xmlns="urn:spreadsheet"><si><r><t>` + fact + `</t></r></si></sst>`},
			{name: "xl/worksheets/sheet1.xml", data: `<worksheet xmlns="urn:spreadsheet"><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c></row></sheetData></worksheet>`},
		}
	case "pptx":
		mediaType = smokePPTXMediaType
		parts = []smokeOOXMLPart{
			{name: "[Content_Types].xml", data: "<Types/>"},
			{name: "ppt/slides/slide1.xml", data: `<p:sld xmlns:p="urn:presentation" xmlns:a="urn:drawing"><a:t>` + fact + `</a:t></p:sld>`},
		}
	default:
		t.Fatalf("unsupported smoke fixture format %q", format)
	}

	path := smokeWriteOOXMLFixture(t, format, parts)
	text, err := document.ExtractOOXMLText(path, mediaType)
	if err != nil {
		t.Fatalf("ExtractOOXMLText(%s): %v", format, err)
	}
	if !strings.Contains(text, fact) {
		t.Fatalf("ExtractOOXMLText(%s) = %q, want fact %q", format, text, fact)
	}
	return []byte(text)
}

type smokeOOXMLPart struct {
	name string
	data string
}

func smokeWriteOOXMLFixture(t *testing.T, extension string, parts []smokeOOXMLPart) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture."+extension)
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

func smokeFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

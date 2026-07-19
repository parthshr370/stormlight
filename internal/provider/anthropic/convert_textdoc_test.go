package anthropic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
)

type textDocumentBlobReader struct {
	blobs map[string][]byte
}

func (r *textDocumentBlobReader) StatBlob(_ context.Context, store, key string) (int64, error) {
	data, ok := r.blobs[textDocumentBlobKey(store, key)]
	if !ok {
		return 0, errors.New("blob not found")
	}
	return int64(len(data)), nil
}

func (r *textDocumentBlobReader) OpenBlob(_ context.Context, store, key string) (io.ReadCloser, error) {
	data, ok := r.blobs[textDocumentBlobKey(store, key)]
	if !ok {
		return nil, errors.New("blob not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func textDocumentBlobKey(store, key string) string {
	return store + "\x00" + key
}

func TestConvertMessagesEncodesTextDocumentReferences(t *testing.T) {
	const store = "session-local"

	tests := []struct {
		name      string
		mediaType string
		data      string
	}{
		{
			name:      "plain text",
			mediaType: "text/plain",
			data:      "meeting notes",
		},
		{
			name:      "markdown normalizes to plain text",
			mediaType: "text/markdown",
			data:      "# Meeting notes\n\n- review proposal",
		},
		{
			name:      "csv normalizes to plain text",
			mediaType: "text/csv",
			data:      "name,score\nAda,10",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const key = "notes-key"
			reader := &textDocumentBlobReader{blobs: map[string][]byte{
				textDocumentBlobKey(store, key): []byte(test.data),
			}}
			block := types.NewDocumentRef(store, key, test.mediaType, "notes.txt", int64(len(test.data)), 0)

			got, err := convertMessages(
				context.Background(),
				[]types.Message{types.UserMessage{Content: types.BlockContent(block)}},
				baseModel(),
				false,
				nil,
				false,
				reader,
			)
			if err != nil {
				t.Fatalf("convertMessages: %v", err)
			}

			content := got[0]["content"].([]map[string]any)
			want := map[string]any{
				"type": "document",
				"source": map[string]any{
					"type":       "text",
					"media_type": "text/plain",
					"data":       test.data,
				},
			}
			if len(content) != 1 || !reflect.DeepEqual(content[0], want) {
				t.Fatalf("content = %#v, want %#v", content, want)
			}
		})
	}
}

func TestConvertMessagesRejectsInvalidTextDocumentReferences(t *testing.T) {
	const store = "session-local"

	tests := []struct {
		name      string
		mediaType string
		data      []byte
		wantCode  document.ErrorCode
	}{
		{
			name:      "non UTF-8 text",
			mediaType: "text/plain",
			data:      []byte{0xff, 0xfe, 'x'},
			wantCode:  document.CodeCorruptOrEncrypted,
		},
		{
			name:      "unsupported document media type",
			mediaType: "application/zip",
			data:      []byte("archive"),
			wantCode:  document.CodeUnsupportedForRoute,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const key = "document-key"
			reader := &textDocumentBlobReader{blobs: map[string][]byte{
				textDocumentBlobKey(store, key): test.data,
			}}
			block := types.NewDocumentRef(store, key, test.mediaType, "attachment", int64(len(test.data)), 0)

			_, err := convertMessages(
				context.Background(),
				[]types.Message{types.UserMessage{Content: types.BlockContent(block)}},
				baseModel(),
				false,
				nil,
				false,
				reader,
			)
			var documentErr *document.DocumentError
			if !errors.As(err, &documentErr) {
				t.Fatalf("error = %v, want *document.DocumentError", err)
			}
			if documentErr.Code != test.wantCode {
				t.Fatalf("DocumentError.Code = %q, want %q", documentErr.Code, test.wantCode)
			}
		})
	}
}

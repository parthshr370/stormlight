package document

import (
	"reflect"
	"testing"
)

func TestMediaRefRefDropsDigestAndPreservesHistorySafeFields(t *testing.T) {
	var digest [32]byte
	for i := range digest {
		digest[i] = byte(i + 1)
	}

	media := MediaRef{
		ID:        "attachment-7",
		Filename:  "quarterly-report.pdf",
		MediaType: "application/pdf",
		SizeBytes: 8192,
		PageCount: 12,
		SHA256:    digest,
		Blob: BlobRef{
			Store: StoreSessionLocal,
			Key:   "blob-key",
		},
	}

	got := media.Ref()
	want := DocumentRef{
		ID:        media.ID,
		Filename:  media.Filename,
		MediaType: media.MediaType,
		SizeBytes: media.SizeBytes,
		PageCount: media.PageCount,
		Blob:      media.Blob,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Ref() = %#v, want %#v", got, want)
	}
	if _, exists := reflect.TypeOf(got).FieldByName("SHA256"); exists {
		t.Fatal("DocumentRef must not persist SHA256")
	}
}

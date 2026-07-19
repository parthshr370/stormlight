package document

import (
	"errors"
	"strings"
	"testing"
)

type testCause struct {
	message string
}

func (e *testCause) Error() string {
	return e.message
}

func TestDocumentErrorPreservesSafeFieldsAndWrapsCause(t *testing.T) {
	cause := &testCause{message: "internal parser failure"}
	details := map[string]any{
		"filename":     "report.pdf",
		"actual_pages": 51,
		"max_pages":    50,
	}
	docErr := NewDocumentError(
		CodePageLimitExceeded,
		"report.pdf has 51 pages; the limit is 50",
		details,
		cause,
	)

	if docErr.Code != CodePageLimitExceeded {
		t.Fatalf("Code = %q, want %q", docErr.Code, CodePageLimitExceeded)
	}
	if docErr.Message != "report.pdf has 51 pages; the limit is 50" {
		t.Fatalf("Message = %q", docErr.Message)
	}
	if docErr.Details["actual_pages"] != 51 || docErr.Details["max_pages"] != 50 {
		t.Fatalf("Details = %#v, want page-limit context", docErr.Details)
	}
	if got := docErr.Error(); !strings.Contains(got, string(CodePageLimitExceeded)) || !strings.Contains(got, docErr.Message) {
		t.Fatalf("Error() = %q, want code and message", got)
	}
	if got := docErr.Unwrap(); got != cause {
		t.Fatalf("Unwrap() = %v, want cause %v", got, cause)
	}
	if !errors.Is(docErr, cause) {
		t.Fatal("errors.Is(document error, cause) = false, want true")
	}
	var gotCause *testCause
	if !errors.As(docErr, &gotCause) || gotCause != cause {
		t.Fatalf("errors.As(document error, *testCause) = %v, want cause %#v", gotCause, cause)
	}
	var gotDocumentError *DocumentError
	if !errors.As(docErr, &gotDocumentError) || gotDocumentError != docErr {
		t.Fatalf("errors.As(document error, *DocumentError) = %v, want %#v", gotDocumentError, docErr)
	}
}

func TestDocumentErrorAllowsNilCause(t *testing.T) {
	docErr := NewDocumentError(CodeUnsupportedMediaType, "unsupported media type", nil, nil)
	if docErr.Unwrap() != nil {
		t.Fatalf("Unwrap() = %v, want nil", docErr.Unwrap())
	}
}

func TestUnsupportedForRouteCodeValue(t *testing.T) {
	if got, want := string(CodeUnsupportedForRoute), "document_unsupported_for_route"; got != want {
		t.Fatalf("CodeUnsupportedForRoute = %q, want %q", got, want)
	}
}

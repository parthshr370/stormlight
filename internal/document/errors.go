package document

// ErrorCode identifies a terminal document failure.
type ErrorCode string

const (
	// CodePageLimitExceeded means a document has too many pages.
	CodePageLimitExceeded ErrorCode = "document_page_limit_exceeded"
	// CodeRequestSizeExceeded means the attachment request is too large.
	CodeRequestSizeExceeded ErrorCode = "document_request_size_exceeded"
	// CodeUnsupportedMediaType means the ingest pipeline cannot handle an attachment media type.
	CodeUnsupportedMediaType ErrorCode = "document_unsupported_media_type"
	// CodeUnsupportedForRoute means the media type is valid and generally supported, but the
	// selected model route cannot encode it yet. CodeUnsupportedMediaType covers MIME types the
	// ingest pipeline cannot handle.
	CodeUnsupportedForRoute ErrorCode = "document_unsupported_for_route"
	// CodeDownloadFailed means attachment download failed.
	CodeDownloadFailed ErrorCode = "document_download_failed"
	// CodeIntegrityMismatch means attachment bytes did not match the expected digest.
	CodeIntegrityMismatch ErrorCode = "document_integrity_mismatch"
	// CodeCorruptOrEncrypted means a document is corrupt or encrypted.
	CodeCorruptOrEncrypted ErrorCode = "document_corrupt_or_encrypted"
	// CodeSSRFBlocked means SSRF protection rejected an attachment request.
	CodeSSRFBlocked ErrorCode = "document_ssrf_blocked"
	// CodeOriginNotAllowed means policy rejects the attachment origin.
	CodeOriginNotAllowed ErrorCode = "document_origin_not_allowed"
	// CodeUnsupportedAttachmentSource means the ingest pipeline cannot handle the attachment source kind.
	CodeUnsupportedAttachmentSource ErrorCode = "document_unsupported_attachment_source"
	// CodeTooManyAttachments means a message contains too many attachments.
	CodeTooManyAttachments ErrorCode = "document_too_many_attachments"
)

// DocumentError is a terminal, user-facing attachment failure. Message is deterministic text safe to
// show a user and never contains bytes, base64, credentials, or private URLs. Details contains
// sanitized structured context such as filename, actual_pages, max_pages, or model. Callers must keep
// Details free of secrets, URLs, and byte content. The wrapped cause stays internal.
type DocumentError struct {
	// Code identifies the failure.
	Code ErrorCode
	// Message is deterministic text safe to show a user.
	Message string
	// Details contains sanitized structured context safe to show a user.
	Details map[string]any
	cause   error
}

// Error returns the document error code and message.
func (e *DocumentError) Error() string {
	return string(e.Code) + ": " + e.Message
}

// Unwrap returns the internal cause.
func (e *DocumentError) Unwrap() error {
	return e.cause
}

// ErrorCode returns the machine-readable failure code as a string. It lets a
// DocumentError satisfy the provider-neutral structured-error contract.
func (e *DocumentError) ErrorCode() string {
	return string(e.Code)
}

// ErrorDetails returns the sanitized structured context for the failure.
func (e *DocumentError) ErrorDetails() map[string]any {
	return e.Details
}

// NewDocumentError builds a DocumentError. cause may be nil. Callers must keep details free of
// secrets, URLs, and byte content.
func NewDocumentError(code ErrorCode, message string, details map[string]any, cause error) *DocumentError {
	return &DocumentError{
		Code:    code,
		Message: message,
		Details: details,
		cause:   cause,
	}
}

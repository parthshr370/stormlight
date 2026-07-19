package openai

import "errors"

// streamTruncatedError marks an error as a truncated-stream failure so callers can
// distinguish it from a normal error via errors.As.
type streamTruncatedError struct{ err error }

func (e *streamTruncatedError) Error() string { return e.err.Error() }
func (e *streamTruncatedError) Unwrap() error { return e.err }

// StreamTruncated wraps err as a truncated-stream error, returning nil for a nil err.
func StreamTruncated(err error) error {
	if err == nil {
		return nil
	}
	return &streamTruncatedError{err: err}
}

// IsStreamTruncated reports whether err (or anything it wraps) is a truncated-stream error.
func IsStreamTruncated(err error) bool {
	var target *streamTruncatedError
	return errors.As(err, &target)
}

package google

import "errors"

// streamTruncatedError marks an error as a mid-stream truncation, wrapping the
// underlying cause so it stays inspectable via errors.As/Unwrap.
type streamTruncatedError struct{ err error }

func (e *streamTruncatedError) Error() string { return e.err.Error() }
func (e *streamTruncatedError) Unwrap() error { return e.err }

// StreamTruncated wraps err as a truncation error (returning nil for nil err),
// so callers can distinguish a cut-short stream from a normal failure.
func StreamTruncated(err error) error {
	if err == nil {
		return nil
	}
	return &streamTruncatedError{err: err}
}

// IsStreamTruncated reports whether err, or anything it wraps, is a stream
// truncation error produced by StreamTruncated.
func IsStreamTruncated(err error) bool {
	var target *streamTruncatedError
	return errors.As(err, &target)
}

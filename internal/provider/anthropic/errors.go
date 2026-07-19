package anthropic

import "errors"

// streamTruncatedError is a marker error that wraps the real cause with the
// StreamTruncated sentinel. The loop inspects errors with [IsStreamTruncated]
// to distinguish "stream died silently" (bounded retry, recompact) from real
// provider errors (terminal).
type streamTruncatedError struct{ err error }

func (e *streamTruncatedError) Error() string { return e.err.Error() }
func (e *streamTruncatedError) Unwrap() error { return e.err }

// StreamTruncated wraps err as a stream-truncated sentinel. A nil err is a
// no-op (convenience for callers that don't want to check for nil first).
func StreamTruncated(err error) error {
	if err == nil {
		return nil
	}
	return &streamTruncatedError{err: err}
}

// IsStreamTruncated reports whether err (or any wrapped error) is a
// StreamTruncated sentinel.
func IsStreamTruncated(err error) bool {
	var target *streamTruncatedError
	return errors.As(err, &target)
}

// Package util holds small provider helpers for sanitizing Unicode text and
// flattening HTTP header maps. It also provides utilities shared across engine
// packages.
package util

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

// SanitizeSurrogates removes unpaired UTF-16 surrogate code points. Go strings
// are UTF-8, so valid surrogate code points cannot appear as decoded runes; the
// byte guard below drops invalid UTF-8 encodings of surrogate code points too.
func SanitizeSurrogates(text string) string {
	var b strings.Builder
	b.Grow(len(text))

	for i := 0; i < len(text); {
		if encodedSurrogateAt(text, i) {
			i += 3
			continue
		}

		r, size := utf8.DecodeRuneInString(text[i:])
		if r >= 0xd800 && r <= 0xdfff {
			i += size
			continue
		}
		if r == utf8.RuneError && size == 1 {
			// Preserve unrelated invalid UTF-8 bytes verbatim; this only removes
			// unpaired surrogate code units.
			b.WriteByte(text[i])
			i++
			continue
		}

		b.WriteString(text[i : i+size])
		i += size
	}

	return b.String()
}

// encodedSurrogateAt spots a forbidden three-byte UTF-8 sequence without dropping unrelated invalid bytes.
func encodedSurrogateAt(text string, i int) bool {
	return i+2 < len(text) &&
		text[i] == 0xed &&
		text[i+1] >= 0xa0 && text[i+1] <= 0xbf &&
		text[i+2] >= 0x80 && text[i+2] <= 0xbf
}

// HeadersToRecord flattens response headers to a map.
func HeadersToRecord(h http.Header) map[string]string {
	result := make(map[string]string, len(h))
	for key, values := range h {
		// deviation: Go http.Header stores multi-values; fetch Headers.entries()
		// exposes one comma-joined value and lower-case names.
		result[strings.ToLower(key)] = strings.Join(values, ", ")
	}
	return result
}

// ProviderHeadersToRecord drops null-valued provider headers.
func ProviderHeadersToRecord(headers map[string]*string) map[string]string {
	if len(headers) == 0 {
		return nil
	}

	result := make(map[string]string, len(headers))
	for key, value := range headers {
		if value != nil {
			result[key] = *value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

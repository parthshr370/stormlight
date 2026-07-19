// Package jsonrepair repairs malformed JSON from model output so it can be
// parsed, escaping stray control characters and invalid backslash escapes.
// Package jsonrepair provides a JSON repair function (byte state-machine for
// missing braces/quotes) and a streaming JSON parser that never errors —
// invalid/incomplete input returns {} so mid-stream tool-arg deltas don't
// crash the fold loop. Final tool arguments use [ParseJsonWithRepair] for a
// best-effort parse of the accumulated partial JSON.
package jsonrepair

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RepairJson escapes raw control chars inside string literals and doubles
// backslashes before invalid escape chars.
func RepairJson(input string) string {
	var repaired strings.Builder
	repaired.Grow(len(input))
	inString := false

	// deviation: iterate bytes. Only ASCII quotes, backslashes, and controls are
	// special-cased; UTF-8 continuation bytes pass through verbatim.
	for i := 0; i < len(input); i++ {
		char := input[i]

		if !inString {
			repaired.WriteByte(char)
			if char == '"' {
				inString = true
			}
			continue
		}

		if char == '"' {
			repaired.WriteByte(char)
			inString = false
			continue
		}

		if char == '\\' {
			if i+1 >= len(input) {
				repaired.WriteString("\\\\")
				continue
			}

			nextChar := input[i+1]
			if nextChar == 'u' {
				unicodeDigits := input[i+2 : min(i+6, len(input))]
				if len(unicodeDigits) == 4 && isHex4(unicodeDigits) {
					repaired.WriteString("\\u")
					repaired.WriteString(unicodeDigits)
					i += 5
					continue
				}
			}

			if isValidJSONEscape(nextChar) {
				repaired.WriteByte('\\')
				repaired.WriteByte(nextChar)
				i++
				continue
			}

			repaired.WriteString("\\\\")
			continue
		}

		if isControlCharacter(char) {
			repaired.WriteString(escapeControlCharacter(char))
		} else {
			repaired.WriteByte(char)
		}
	}

	return repaired.String()
}

func isControlCharacter(char byte) bool { return char <= 0x1f }

func escapeControlCharacter(char byte) string {
	switch char {
	case '\b':
		return "\\b"
	case '\f':
		return "\\f"
	case '\n':
		return "\\n"
	case '\r':
		return "\\r"
	case '\t':
		return "\\t"
	default:
		return fmt.Sprintf("\\u%04x", char)
	}
}

func isValidJSONEscape(char byte) bool {
	switch char {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't', 'u':
		return true
	default:
		return false
	}
}

func isHex4(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ParseJsonWithRepair unmarshals input into out, retrying once with RepairJson.
func ParseJsonWithRepair(input string, out any) error {
	if err := json.Unmarshal([]byte(input), out); err != nil {
		repaired := RepairJson(input)
		if repaired != input {
			return json.Unmarshal([]byte(repaired), out)
		}
		return err
	}
	return nil
}

// ParseStreamingJSON parses possibly incomplete streaming JSON without failing.
func ParseStreamingJSON(partial string) map[string]any {
	if strings.TrimSpace(partial) == "" {
		return map[string]any{}
	}

	var result map[string]any
	if err := ParseJsonWithRepair(partial, &result); err == nil && result != nil {
		return result
	}
	// A malformed mid-stream delta has no trustworthy tool arguments. The Go port
	// intentionally has no external dependency and only trusts final tool args.
	return map[string]any{}
}

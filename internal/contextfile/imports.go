// Package contextfile expands bounded @file references in trusted context files.
package contextfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxImportDepth is the largest number of import edges followed from a root.
const MaxImportDepth = 5

// ExpandOptions configures import expansion.
type ExpandOptions struct {
	HomeDir  string
	MaxDepth int
}

// ErrorCode classifies context import errors.
type ErrorCode string

const (
	InvalidOptions ErrorCode = "invalid_options"
	InvalidSource  ErrorCode = "invalid_source"
	ReadSource     ErrorCode = "read_source"
)

// Error is a typed context-import failure.
type Error struct {
	Code ErrorCode
	Path string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("context import %s: %s", e.Code, e.Path) }
func (e *Error) Unwrap() error { return e.Err }
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e.Code == other.Code
}

// Expand expands @file imports in content. Optional imports that cannot be read
// stay literal; only invalid options, source failures, and cancellation fail.
func Expand(ctx context.Context, filePath string, content []byte, options ExpandOptions) (string, error) {
	if options.MaxDepth < 0 || options.MaxDepth > MaxImportDepth {
		return "", &Error{Code: InvalidOptions, Path: filePath, Err: errors.New("max depth must be between 0 and 5")}
	}
	maxDepth := options.MaxDepth
	if maxDepth == 0 {
		maxDepth = MaxImportDepth
	}
	absolute, err := filepath.Abs(filePath)
	if err != nil {
		return "", &Error{Code: InvalidSource, Path: filePath, Err: err}
	}
	canonical := canonicalPath(absolute)
	visited := map[string]struct{}{canonical: {}}
	return expand(ctx, absolute, string(content), options.HomeDir, maxDepth, 0, visited)
}

// expand walks only prose: fenced code stays literal so examples don't accidentally import local files.
func expand(ctx context.Context, filePath, content, home string, maxDepth, depth int, visited map[string]struct{}) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var out strings.Builder
	inFence := false
	fenceChar := byte(0)
	fenceLen := 0
	for len(content) > 0 {
		line := content
		newline := ""
		if index := strings.IndexByte(content, '\n'); index >= 0 {
			line, content, newline = content[:index], content[index+1:], "\n"
		} else {
			content = ""
		}
		trimmed := strings.TrimLeft(line, " \t")
		if marker, count := fence(trimmed); count > 0 {
			if !inFence {
				inFence, fenceChar, fenceLen = true, marker, count
			} else if marker == fenceChar && count >= fenceLen {
				inFence = false
			}
			out.WriteString(line)
			out.WriteString(newline)
			continue
		}
		if inFence {
			out.WriteString(line)
			out.WriteString(newline)
			continue
		}
		expanded, err := expandLine(ctx, filePath, line, home, maxDepth, depth, visited)
		if err != nil {
			return "", err
		}
		out.WriteString(expanded)
		out.WriteString(newline)
	}
	return out.String(), nil
}

// fence recognizes the marker and minimum length needed to protect a Markdown code block.
func fence(line string) (byte, int) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	marker := line[0]
	count := 0
	for count < len(line) && line[count] == marker {
		count++
	}
	if count < 3 {
		return 0, 0
	}
	return marker, count
}

// expandLine ignores inline code and keeps trailing prose punctuation outside a resolved import token.
func expandLine(ctx context.Context, source, line, home string, maxDepth, depth int, visited map[string]struct{}) (string, error) {
	var out strings.Builder
	inlineRun := 0
	for i := 0; i < len(line); {
		if line[i] == '`' {
			run := 1
			for i+run < len(line) && line[i+run] == '`' {
				run++
			}
			if inlineRun == 0 {
				inlineRun = run
			} else if inlineRun == run {
				inlineRun = 0
			}
			out.WriteString(line[i : i+run])
			i += run
			continue
		}
		if inlineRun != 0 || line[i] != '@' || (i > 0 && line[i-1] != ' ' && line[i-1] != '\t') || i+1 >= len(line) || !importStart(line[i+1]) {
			out.WriteByte(line[i])
			i++
			continue
		}
		end := i + 1
		for end < len(line) && !tokenWhitespace(line[end]) {
			end++
		}
		tokenEnd := end
		for tokenEnd > i+1 && strings.ContainsRune(".,;:!?)]}\"'", rune(line[tokenEnd-1])) {
			tokenEnd--
		}
		candidate := line[i+1 : tokenEnd]
		literal := line[i:end]
		replacement, ok, err := importFile(ctx, source, candidate, home, maxDepth, depth, visited)
		if err != nil {
			return "", err
		}
		if ok {
			out.WriteString(replacement)
			out.WriteString(line[tokenEnd:end])
		} else {
			out.WriteString(literal)
		}
		i = end
	}
	return out.String(), nil
}

func importStart(b byte) bool {
	return b == '.' || b == '/' || b == '~' || b == '_' || b == '-' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func tokenWhitespace(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', '\v', '\f':
		return true
	default:
		return false
	}
}

// importFile leaves missing, cyclic, deep, and non-regular references literal so optional imports don't become errors.
func importFile(ctx context.Context, source, target, home string, maxDepth, depth int, visited map[string]struct{}) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	resolved := target
	if resolved == "~" {
		if home == "" {
			return "", false, nil
		}
		resolved = home
	} else if strings.HasPrefix(resolved, "~/") {
		if home == "" {
			return "", false, nil
		}
		resolved = filepath.Join(home, resolved[2:])
	} else if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(source), resolved)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", false, nil
	}
	key := canonicalPath(absolute)
	if _, exists := visited[key]; exists {
		return "", false, nil
	}
	if depth >= maxDepth {
		return "", false, nil
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return "", false, nil
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return "", false, nil
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	visited[key] = struct{}{}
	expanded, err := expand(ctx, absolute, string(data), home, maxDepth, depth+1, visited)
	if err != nil {
		return "", false, err
	}
	return expanded, true, nil
}

// canonicalPath gives the recursion guard one identity for a file even when callers reach it through a symlink.
func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

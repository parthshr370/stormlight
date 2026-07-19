// Package pathutil provides path resolution and normalization helpers:
// ResolveToCwd (expand ~/$HOME and join to cwd), canonicalizePath
// (EvalSymlinks), and ResolveReadPath (macOS unicode variants — narrow NBSP,
// NFD normalization, curly quotes — gated on darwin).
package pathutil

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const narrowNoBreakSpace = "\u202f"

var (
	unicodeSpaces = regexp.MustCompile(`[\x{00A0}\x{2000}-\x{200A}\x{202F}\x{205F}\x{3000}]`)
	amPMPath      = regexp.MustCompile(`(?i) (AM|PM)\.`)
)

// PathInputOptions controls how NormalizePath and ResolvePath process input:
// trimming, tilde expansion, @-prefix stripping, and unicode-space normalization.
type PathInputOptions struct {
	Trim                   bool
	ExpandTilde            *bool
	HomeDir                string
	StripAtPrefix          bool
	NormalizeUnicodeSpaces bool
}

// CanonicalizePath resolves symlinks in path via filepath.EvalSymlinks
// and returns the original path on error.
func CanonicalizePath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// IsLocalPath reports whether value is a local filesystem path,
// not a remote URI or protocol (npm:, git:, github:, http(s):, ssh:).
func IsLocalPath(value string) bool {
	trimmed := strings.TrimSpace(value)
	for _, prefix := range []string{"npm:", "git:", "github:", "http:", "https:", "ssh:"} {
		if strings.HasPrefix(trimmed, prefix) {
			return false
		}
	}
	return true
}

// NormalizePath applies trim, tilde expansion, @-prefix stripping,
// file:// parsing, and unicode-space normalization according to options.
func NormalizePath(input string, options PathInputOptions) string {
	normalized := input
	if options.Trim {
		normalized = strings.TrimSpace(normalized)
	}
	if options.NormalizeUnicodeSpaces {
		normalized = unicodeSpaces.ReplaceAllString(normalized, " ")
	}
	if options.StripAtPrefix && strings.HasPrefix(normalized, "@") {
		normalized = normalized[1:]
	}

	if shouldExpandTilde(options.ExpandTilde) {
		home := options.HomeDir
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		if home != "" {
			if normalized == "~" {
				return home
			}
			if strings.HasPrefix(normalized, "~/") || (runtime.GOOS == "windows" && strings.HasPrefix(normalized, `~\`)) {
				return filepath.Join(home, normalized[2:])
			}
		}
	}

	if strings.HasPrefix(normalized, "file://") {
		if parsed, err := url.Parse(normalized); err == nil {
			path := parsed.Path
			if runtime.GOOS == "windows" && parsed.Host != "" {
				path = `\\` + parsed.Host + path
			}
			if unescaped, err := url.PathUnescape(path); err == nil {
				path = unescaped
			}
			return filepath.FromSlash(path)
		}
	}

	return normalized
}

// ResolvePath normalizes input according to options and joins it to baseDir
// (defaulting to the working directory). If input is already absolute,
// it's cleaned and returned directly.
func ResolvePath(input string, baseDir string, options PathInputOptions) string {
	if baseDir == "" {
		baseDir, _ = os.Getwd()
	}
	normalized := NormalizePath(input, options)
	normalizedBaseDir := NormalizePath(baseDir, PathInputOptions{})
	if filepath.IsAbs(normalized) {
		return cleanAbs(normalized)
	}
	return cleanAbs(filepath.Join(normalizedBaseDir, normalized))
}

// GetCwdRelativePath returns filePath relative to cwd and reports whether
// it's under cwd. Returns empty string and false for paths outside cwd.
func GetCwdRelativePath(filePath string, cwd string) (string, bool) {
	resolvedCwd := ResolvePath(cwd, "", PathInputOptions{})
	resolvedPath := ResolvePath(filePath, resolvedCwd, PathInputOptions{})
	rel, err := filepath.Rel(resolvedCwd, resolvedPath)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return ".", true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// FormatPathRelativeToCwdOrAbsolute returns a forward-slash path: relative
// to cwd when filePath is under cwd, absolute otherwise.
func FormatPathRelativeToCwdOrAbsolute(filePath string, cwd string) string {
	abs := ResolvePath(filePath, cwd, PathInputOptions{})
	if rel, ok := GetCwdRelativePath(abs, cwd); ok {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

// ExpandPath is a shorthand for NormalizePath with unicode-space
// normalization and @-prefix stripping.
func ExpandPath(filePath string) string {
	return NormalizePath(filePath, PathInputOptions{NormalizeUnicodeSpaces: true, StripAtPrefix: true})
}

// ResolveToCwd is a shorthand for ResolvePath with unicode-space
// normalization and @-prefix stripping.
func ResolveToCwd(filePath string, cwd string) string {
	return ResolvePath(filePath, cwd, PathInputOptions{NormalizeUnicodeSpaces: true, StripAtPrefix: true})
}

// FileExists reports whether filePath exists on disk.
func FileExists(filePath string) bool {
	_, err := os.Stat(filePath)
	return err == nil
}

// PathExists reports whether filePath exists on disk.
func PathExists(filePath string) bool {
	return FileExists(filePath)
}

// ResolveReadPath resolves filePath to cwd and, when the result doesn't
// exist, tries macOS unicode variants (NFC and NFD forms, narrow NBSP, curly quotes)
// before falling back to the resolved path.
func ResolveReadPath(filePath string, cwd string) string {
	resolved := ResolveToCwd(filePath, cwd)
	if FileExists(resolved) {
		return resolved
	}

	amPMVariant := tryMacOSScreenshotPath(resolved)
	if amPMVariant != resolved && FileExists(amPMVariant) {
		return amPMVariant
	}

	nfdVariant := tryNFDVariant(resolved)
	if nfdVariant != resolved && FileExists(nfdVariant) {
		return nfdVariant
	}

	curlyVariant := tryCurlyQuoteVariant(resolved)
	if curlyVariant != resolved && FileExists(curlyVariant) {
		return curlyVariant
	}

	nfdCurlyVariant := tryCurlyQuoteVariant(nfdVariant)
	if nfdCurlyVariant != resolved && FileExists(nfdCurlyVariant) {
		return nfdCurlyVariant
	}

	return resolved
}

// shouldExpandTilde preserves the zero-value default: an unset option still expands home paths.
func shouldExpandTilde(v *bool) bool {
	return v == nil || *v
}

func cleanAbs(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// tryMacOSScreenshotPath accounts for Finder screenshot names that use a narrow no-break space before AM or PM.
func tryMacOSScreenshotPath(filePath string) string {
	return amPMPath.ReplaceAllString(filePath, narrowNoBreakSpace+"$1.")
}

func tryNFDVariant(filePath string) string {
	return norm.NFD.String(filePath)
}

func tryCurlyQuoteVariant(filePath string) string {
	return strings.ReplaceAll(filePath, "'", "\u2019")
}

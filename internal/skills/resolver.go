package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.harness.dev/harness/internal/resource"
)

// ResolveErrorCode classifies failures while resolving a skill URI.
type ResolveErrorCode string

const (
	ResolveInvalidURI    ResolveErrorCode = "invalid_uri"
	ResolveUnknownSkill  ResolveErrorCode = "unknown_skill"
	ResolveInvalidPath   ResolveErrorCode = "invalid_path"
	ResolvePathEscape    ResolveErrorCode = "path_escape"
	ResolveNotFound      ResolveErrorCode = "not_found"
	ResolveInvalidTarget ResolveErrorCode = "invalid_target"
	ResolveReadFailed    ResolveErrorCode = "read_failed"
)

// ResolveError is a typed skill URI resolution failure. Path is always logical
// to the skill and never exposes the skill's disk location.
type ResolveError struct {
	Code  ResolveErrorCode
	URI   string
	Skill string
	Path  string
	Err   error
}

func (e *ResolveError) Error() string {
	if e.Skill != "" {
		return fmt.Sprintf("resolve %s for skill %q: %s", e.URI, e.Skill, e.Code)
	}
	return fmt.Sprintf("resolve %s: %s", e.URI, e.Code)
}

func (e *ResolveError) Unwrap() error { return e.Err }

// Is matches a resolution failure by code.
func (e *ResolveError) Is(target error) bool {
	other, ok := target.(*ResolveError)
	return ok && e.Code == other.Code
}

// Resolver is an immutable index of canonical skill metadata.
type Resolver struct {
	items map[string]Skill
}

// NewResolver validates and indexes a canonical skill slice.
func NewResolver(items []Skill) (*Resolver, error) {
	index := make(map[string]Skill, len(items))
	for _, skill := range items {
		if skill.Name == "" || len(ValidateName(skill.Name)) != 0 {
			return nil, &ResolveError{Code: ResolveInvalidPath, Skill: skill.Name, Err: errors.New("invalid skill name")}
		}
		if !filepath.IsAbs(skill.FilePath) || !filepath.IsAbs(skill.BaseDir) {
			return nil, &ResolveError{Code: ResolveInvalidPath, Skill: skill.Name, Err: errors.New("skill paths must be absolute")}
		}
		rel, err := filepath.Rel(skill.BaseDir, skill.FilePath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, &ResolveError{Code: ResolveInvalidPath, Skill: skill.Name, Err: errors.New("skill file is outside its base directory")}
		}
		if _, exists := index[skill.Name]; exists {
			return nil, &ResolveError{Code: ResolveInvalidPath, Skill: skill.Name, Err: errors.New("duplicate skill name")}
		}
		index[skill.Name] = skill
	}
	return &Resolver{items: index}, nil
}

// Resolve reads a skill:// URI through an os.Root rooted at its indexed base.
func (r *Resolver) Resolve(ctx context.Context, rawURI string) (resource.Content, error) {
	if err := ctx.Err(); err != nil {
		return resource.Content{}, err
	}
	u, err := url.Parse(rawURI)
	if err != nil || u.Scheme != "skill" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(rawURI, "#") || u.Host == "" {
		return resource.Content{}, &ResolveError{Code: ResolveInvalidURI, URI: rawURI, Err: err}
	}
	skill, ok := r.items[u.Host]
	if !ok {
		return resource.Content{}, &ResolveError{Code: ResolveUnknownSkill, URI: rawURI, Skill: u.Host, Err: fs.ErrNotExist}
	}

	var target string
	if u.EscapedPath() == "" || u.EscapedPath() == "/" {
		target, err = filepath.Rel(skill.BaseDir, skill.FilePath)
		if err != nil {
			return resource.Content{}, &ResolveError{Code: ResolveInvalidPath, URI: rawURI, Skill: skill.Name, Err: err}
		}
	} else {
		escaped := strings.TrimPrefix(u.EscapedPath(), "/")
		target, err = url.PathUnescape(escaped)
		if err != nil {
			return resource.Content{}, &ResolveError{Code: ResolveInvalidURI, URI: rawURI, Skill: skill.Name, Err: err}
		}
	}
	logical := filepath.ToSlash(target)
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.ContainsRune(target, 0) || strings.Contains(target, `\`) {
		return resource.Content{}, &ResolveError{Code: ResolveInvalidPath, URI: rawURI, Skill: skill.Name, Path: logical, Err: errors.New("invalid asset path")}
	}
	for _, part := range strings.FieldsFunc(target, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if part == ".." {
			return resource.Content{}, &ResolveError{Code: ResolvePathEscape, URI: rawURI, Skill: skill.Name, Path: logical, Err: errors.New("path traversal")}
		}
	}
	target = filepath.Clean(target)
	root, err := os.OpenRoot(skill.BaseDir)
	if err != nil {
		return resource.Content{}, &ResolveError{Code: ResolveReadFailed, URI: rawURI, Skill: skill.Name, Path: logical, Err: err}
	}
	defer root.Close()
	info, err := root.Stat(target)
	if err != nil {
		code := ResolveReadFailed
		if errors.Is(err, fs.ErrNotExist) {
			code = ResolveNotFound
		} else if targetEscapesBase(skill.BaseDir, target) {
			code = ResolvePathEscape
		}
		return resource.Content{}, &ResolveError{Code: code, URI: rawURI, Skill: skill.Name, Path: logical, Err: err}
	}
	if !info.Mode().IsRegular() {
		return resource.Content{}, &ResolveError{Code: ResolveInvalidTarget, URI: rawURI, Skill: skill.Name, Path: logical, Err: errors.New("target is not a regular file")}
	}
	file, err := root.Open(target)
	if err != nil {
		code := ResolveReadFailed
		if errors.Is(err, fs.ErrNotExist) {
			code = ResolveNotFound
		} else if targetEscapesBase(skill.BaseDir, target) {
			code = ResolvePathEscape
		}
		return resource.Content{}, &ResolveError{Code: code, URI: rawURI, Skill: skill.Name, Path: logical, Err: err}
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return resource.Content{}, &ResolveError{Code: ResolveReadFailed, URI: rawURI, Skill: skill.Name, Path: logical, Err: err}
	}
	if !info.Mode().IsRegular() {
		return resource.Content{}, &ResolveError{Code: ResolveInvalidTarget, URI: rawURI, Skill: skill.Name, Path: logical, Err: errors.New("target is not a regular file")}
	}
	if err := ctx.Err(); err != nil {
		return resource.Content{}, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return resource.Content{}, &ResolveError{Code: ResolveReadFailed, URI: rawURI, Skill: skill.Name, Path: logical, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return resource.Content{}, err
	}
	mediaType := resource.TextMediaType
	if strings.EqualFold(filepath.Ext(target), ".md") {
		mediaType = resource.MarkdownMediaType
	}
	uriPath := filepath.ToSlash(target)
	if uriPath == "." {
		uriPath = ""
	}
	if uriPath == "" {
		return resource.Content{URI: "skill://" + skill.Name, MediaType: mediaType, Data: data}, nil
	}
	return resource.Content{URI: "skill://" + skill.Name + (&url.URL{Path: "/" + uriPath}).EscapedPath(), MediaType: mediaType, Data: data}, nil
}

// targetEscapesBase identifies symlinks that [os.Root] rejects for leaving a skill's base directory.

func targetEscapesBase(base, target string) bool {
	path := filepath.Join(base, target)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return outsideBase(base, resolved)
	}
	info, lstatErr := os.Lstat(path)
	if lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	link, linkErr := os.Readlink(path)
	if linkErr != nil {
		return false
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(path), link)
	}
	return outsideBase(base, filepath.Clean(link))
}

// outsideBase uses path-aware containment so sibling directories sharing a prefix don't count as children.
func outsideBase(base, path string) bool {
	relative, err := filepath.Rel(base, path)
	return err == nil && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative))
}

// SkillNames returns sorted indexed names for diagnostics without exposing paths.
func (r *Resolver) SkillNames() []string {
	names := make([]string, 0, len(r.items))
	for name := range r.items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

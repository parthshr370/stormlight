// Package skills discovers and validates agent skills from directory trees.
// Each skill is a SKILL.md file with YAML frontmatter; skills are deduplicated
// by [filepath.EvalSymlinks] and checked for name/description constraints.
// Gitignore rules are honored so .gitignored directories are skipped.
package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/pathutil"
	"gopkg.in/yaml.v3"
)

const (
	// MaxNameLength is the maximum byte length for a skill name.
	MaxNameLength = 64
	// MaxDescriptionLength is the maximum byte length for a skill description.
	MaxDescriptionLength = 1024
)

var validName = regexp.MustCompile(`^[a-z0-9-]+$`)

// SourceInfo records where a skill was loaded from.
type SourceInfo struct {
	Source  string
	Scope   string
	BaseDir string
}

// Skill is a loaded agent skill with its metadata and source location.
type Skill struct {
	Name                   string
	Description            string
	WhenToUse              string
	AllowedTools           []string
	Model                  string
	Fork                   bool
	Paths                  []string
	FilePath               string
	BaseDir                string
	SourceInfo             SourceInfo
	DisableModelInvocation bool
}

// Diagnostic records a problem found during skill loading.
type Diagnostic struct {
	Type      string
	Message   string
	Path      string
	Collision *Collision
}

// Collision records a conflict between two skills with the same name.
type Collision struct {
	ResourceType string
	Name         string
	WinnerPath   string
	LoserPath    string
}

// LoadResult holds the skills and diagnostics from a load operation.
type LoadResult struct {
	Skills      []Skill
	Diagnostics []Diagnostic
}

// LoadFromDirOptions configures loading skills from a single directory tree.
type LoadFromDirOptions struct {
	Dir    string
	Source string
}

// LoadOptions configures how skills are discovered from the agent dir, cwd, and explicit paths.
type LoadOptions struct {
	Cwd             string
	AgentDir        string
	SkillPaths      []string
	IncludeDefaults bool
}

// frontmatter keeps document-only compatibility fields separate from the public skill metadata.
type frontmatter struct {
	Name                   string   `yaml:"name"`
	Description            string   `yaml:"description"`
	WhenToUse              string   `yaml:"when_to_use"`
	AllowedTools           []string `yaml:"allowed-tools"`
	Model                  string   `yaml:"model"`
	Context                string   `yaml:"context"`
	Paths                  []string `yaml:"paths"`
	DisableModelInvocation bool     `yaml:"disable-model-invocation"`
	Hide                   bool     `yaml:"hide"`
}

// ValidateName reports validation errors for a skill name (length, charset, hyphen rules).
func ValidateName(name string) []string {
	errs := []string{}
	if len(name) > MaxNameLength {
		errs = append(errs, fmt.Sprintf("name exceeds %d characters (%d)", MaxNameLength, len(name)))
	}
	if !validName.MatchString(name) {
		errs = append(errs, "name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errs = append(errs, "name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		errs = append(errs, "name must not contain consecutive hyphens")
	}
	return errs
}

// ValidateDescription reports validation errors for a skill description (non-empty, max length).
func ValidateDescription(description string) []string {
	errs := []string{}
	if strings.TrimSpace(description) == "" {
		errs = append(errs, "description is required")
	} else if len(description) > MaxDescriptionLength {
		errs = append(errs, fmt.Sprintf("description exceeds %d characters (%d)", MaxDescriptionLength, len(description)))
	}
	return errs
}

// LoadSkillsFromDir discovers skills in one directory tree with the supplied source label.
func LoadSkillsFromDir(ctx context.Context, options LoadFromDirOptions) (LoadResult, error) {
	return loadSkillsFromDirInternal(ctx, options.Dir, options.Source, true, nil, options.Dir)
}

// LoadSkills discovers skills from the configured agent, project, and explicit paths.
func LoadSkills(ctx context.Context, options LoadOptions) (LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return LoadResult{}, err
	}
	resolvedCwd := pathutil.ResolvePath(options.Cwd, "", pathutil.PathInputOptions{})
	resolvedAgentDir := pathutil.ResolvePath(options.AgentDir, "", pathutil.PathInputOptions{})
	if resolvedCwd == "" || resolvedAgentDir == "" {
		return LoadResult{}, fmt.Errorf("skill loader requires cwd and agent dir")
	}

	skillMap := map[string]Skill{}
	realPaths := map[string]bool{}
	diagnostics := []Diagnostic{}
	collisions := []Diagnostic{}
	addSkills := func(result LoadResult) {
		diagnostics = append(diagnostics, result.Diagnostics...)
		for _, skill := range result.Skills {
			realPath := pathutil.CanonicalizePath(skill.FilePath)
			if realPaths[realPath] {
				continue
			}
			if existing, ok := skillMap[skill.Name]; ok {
				collisions = append(collisions, Diagnostic{Type: "collision", Message: fmt.Sprintf("name %q collision", skill.Name), Path: skill.FilePath, Collision: &Collision{ResourceType: "skill", Name: skill.Name, WinnerPath: existing.FilePath, LoserPath: skill.FilePath}})
				continue
			}
			skillMap[skill.Name] = skill
			realPaths[realPath] = true
		}
	}
	load := func(dir, source string) error {
		result, err := loadSkillsFromDirInternal(ctx, dir, source, true, nil, dir)
		if err != nil {
			return err
		}
		addSkills(result)
		return nil
	}
	userSkillsDir := filepath.Join(resolvedAgentDir, "skills")
	projectSkillsDir := filepath.Join(resolvedCwd, config.ProjectConfigDirName, "skills")
	if options.IncludeDefaults {
		if err := load(userSkillsDir, "user"); err != nil {
			return LoadResult{}, err
		}
		if err := load(projectSkillsDir, "project"); err != nil {
			return LoadResult{}, err
		}
	}
	for _, rawPath := range options.SkillPaths {
		if err := ctx.Err(); err != nil {
			return LoadResult{}, err
		}
		resolvedPath := pathutil.ResolvePath(rawPath, resolvedCwd, pathutil.PathInputOptions{Trim: true})
		info, err := os.Stat(resolvedPath)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{Type: "warning", Message: "skill path does not exist", Path: resolvedPath})
			continue
		}
		source := "path"
		if !options.IncludeDefaults {
			if isUnderPath(resolvedPath, userSkillsDir) {
				source = "user"
			} else if isUnderPath(resolvedPath, projectSkillsDir) {
				source = "project"
			}
		}
		if info.IsDir() {
			if err := load(resolvedPath, source); err != nil {
				return LoadResult{}, err
			}
		} else if strings.HasSuffix(resolvedPath, ".md") {
			loaded := loadSkillFromFile(ctx, resolvedPath, source)
			if loaded.Err != nil {
				return LoadResult{}, loaded.Err
			}
			if loaded.Skill != nil {
				addSkills(LoadResult{Skills: []Skill{*loaded.Skill}, Diagnostics: loaded.Diagnostics})
			} else {
				diagnostics = append(diagnostics, loaded.Diagnostics...)
			}
		} else {
			diagnostics = append(diagnostics, Diagnostic{Type: "warning", Message: "skill path is not a markdown file", Path: resolvedPath})
		}
	}
	names := make([]string, 0, len(skillMap))
	for name := range skillMap {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Skill, 0, len(names))
	for _, name := range names {
		out = append(out, skillMap[name])
	}
	diagnostics = append(diagnostics, collisions...)
	return LoadResult{Skills: out, Diagnostics: diagnostics}, nil
}

// skillFileResult keeps recoverable validation diagnostics separate from failures that stop discovery.
type skillFileResult struct {
	Skill       *Skill
	Diagnostics []Diagnostic
	Err         error
}

// loadSkillsFromDirInternal carries ignore rules from parent directories so nested patterns keep their original scope.
func loadSkillsFromDirInternal(ctx context.Context, dir string, source string, includeRootFiles bool, inherited ignoreRules, rootDir string) (LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return LoadResult{}, err
	}
	result := LoadResult{}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, err
	}
	if rootDir == "" {
		rootDir = dir
	}
	rules := append(ignoreRules{}, inherited...)
	rules = append(rules, readIgnoreRules(dir, rootDir)...)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return LoadResult{}, err
		}
		if entry.Name() != "SKILL.md" {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		info, err := os.Stat(fullPath)
		if err != nil || info.IsDir() || rules.ignores(relativeTo(rootDir, fullPath)) {
			continue
		}
		loaded := loadSkillFromFile(ctx, fullPath, source)
		if loaded.Err != nil {
			return LoadResult{}, loaded.Err
		}
		if loaded.Skill != nil {
			result.Skills = append(result.Skills, *loaded.Skill)
		}
		result.Diagnostics = append(result.Diagnostics, loaded.Diagnostics...)
		return result, nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return LoadResult{}, err
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		fullPath := filepath.Join(dir, name)
		info, err := os.Stat(fullPath)
		if err != nil {
			continue
		}
		rel := relativeTo(rootDir, fullPath)
		ignorePath := rel
		if info.IsDir() {
			ignorePath += "/"
		}
		if rules.ignores(ignorePath) {
			continue
		}
		if info.IsDir() {
			sub, err := loadSkillsFromDirInternal(ctx, fullPath, source, false, rules, rootDir)
			if err != nil {
				return LoadResult{}, err
			}
			result.Skills = append(result.Skills, sub.Skills...)
			result.Diagnostics = append(result.Diagnostics, sub.Diagnostics...)
			continue
		}
		if !includeRootFiles || !strings.HasSuffix(name, ".md") {
			continue
		}
		loaded := loadSkillFromFile(ctx, fullPath, source)
		if loaded.Err != nil {
			return LoadResult{}, loaded.Err
		}
		if loaded.Skill != nil {
			result.Skills = append(result.Skills, *loaded.Skill)
		}
		result.Diagnostics = append(result.Diagnostics, loaded.Diagnostics...)
	}
	return result, nil
}

// loadSkillFromFile canonicalizes resolver paths only after metadata passes validation.
func loadSkillFromFile(ctx context.Context, filePath string, source string) skillFileResult {
	if err := ctx.Err(); err != nil {
		return skillFileResult{Err: err}
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return skillFileResult{Diagnostics: []Diagnostic{{Type: "warning", Message: err.Error(), Path: filePath}}}
	}
	if err := ctx.Err(); err != nil {
		return skillFileResult{Err: err}
	}
	fm, err := parseFrontmatter(string(raw))
	if err != nil {
		return skillFileResult{Diagnostics: []Diagnostic{{Type: "warning", Message: err.Error(), Path: filePath}}}
	}
	skillDir := filepath.Dir(filePath)
	name := fm.Name
	if name == "" {
		name = filepath.Base(skillDir)
	}
	diagnostics := make([]Diagnostic, 0)
	for _, msg := range ValidateDescription(fm.Description) {
		diagnostics = append(diagnostics, Diagnostic{Type: "warning", Message: msg, Path: filePath})
	}
	for _, msg := range ValidateName(name) {
		diagnostics = append(diagnostics, Diagnostic{Type: "warning", Message: msg, Path: filePath})
	}
	if strings.TrimSpace(fm.Description) == "" || len(ValidateName(name)) != 0 {
		return skillFileResult{Diagnostics: diagnostics}
	}
	canonicalFile := pathutil.CanonicalizePath(filePath)
	canonicalBase := pathutil.CanonicalizePath(skillDir)
	skill := Skill{
		Name: name, Description: fm.Description, WhenToUse: fm.WhenToUse,
		AllowedTools: append([]string(nil), fm.AllowedTools...), Model: fm.Model,
		Fork: fm.Context == "fork", Paths: append([]string(nil), fm.Paths...),
		FilePath: canonicalFile, BaseDir: canonicalBase,
		SourceInfo:             createSkillSourceInfo(canonicalFile, canonicalBase, source),
		DisableModelInvocation: fm.DisableModelInvocation || fm.Hide,
	}
	return skillFileResult{Skill: &skill, Diagnostics: diagnostics}
}

// Find returns the skill named name.
func Find(items []Skill, name string) (Skill, bool) {
	for _, item := range items {
		if item.Name == name {
			return item, true
		}
	}
	return Skill{}, false
}

// LoadBody reads a skill body on demand and strips frontmatter when present.
func LoadBody(ctx context.Context, skill Skill) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(skill.FilePath)
	if err != nil {
		return "", fmt.Errorf("read skill body: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return strings.TrimSpace(stripFrontmatter(string(data))), nil
}

// stripFrontmatter leaves malformed delimiters in place so callers don't silently lose skill content.
func stripFrontmatter(content string) string {
	normalized := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return content
	}
	if end := strings.Index(normalized[4:], "\n---"); end >= 0 {
		return normalized[4+end+4:]
	}
	return content
}

// parseFrontmatter only lets YAML errors escape after it finds a closing delimiter.
func parseFrontmatter(content string) (frontmatter, error) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	if !strings.HasPrefix(normalized, "---") {
		return frontmatter{}, nil
	}
	end := strings.Index(normalized[3:], "\n---")
	if end == -1 {
		return frontmatter{}, nil
	}
	yamlString := normalized[4 : 3+end]
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(yamlString), &fm); err != nil {
		return frontmatter{}, err
	}
	return fm, nil
}

func createSkillSourceInfo(_ string, baseDir string, source string) SourceInfo {
	switch source {
	case "user":
		return SourceInfo{Source: "local", Scope: "user", BaseDir: baseDir}
	case "project":
		return SourceInfo{Source: "local", Scope: "project", BaseDir: baseDir}
	case "path":
		return SourceInfo{Source: "local", BaseDir: baseDir}
	default:
		return SourceInfo{Source: source, BaseDir: baseDir}
	}
}

// ignoreRules keeps match order because later patterns override earlier ones.
type ignoreRules []string

// readIgnoreRules rebases a directory's patterns to the discovery root before passing them to children.
func readIgnoreRules(dir string, rootDir string) ignoreRules {
	var rules ignoreRules
	for _, filename := range []string{".gitignore", ".ignore", ".fdignore"} {
		content, err := os.ReadFile(filepath.Join(dir, filename))
		if err != nil {
			continue
		}
		relDir := relativeTo(rootDir, dir)
		prefix := ""
		if relDir != "." && relDir != "" {
			prefix = relDir + "/"
		}
		for _, line := range strings.Split(string(content), "\n") {
			if pattern := prefixIgnorePattern(line, prefix); pattern != "" {
				rules = append(rules, pattern)
			}
		}
	}
	return rules
}

// prefixIgnorePattern preserves negation while scoping a nested ignore rule to its declaring directory.
func prefixIgnorePattern(line string, prefix string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || (strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, `\#`)) {
		return ""
	}
	pattern := line
	negated := false
	if strings.HasPrefix(pattern, "!") {
		negated = true
		pattern = pattern[1:]
	} else if strings.HasPrefix(pattern, `\!`) {
		pattern = pattern[1:]
	}
	if strings.HasPrefix(pattern, "/") {
		pattern = pattern[1:]
	}
	prefixed := pattern
	if prefix != "" {
		prefixed = prefix + pattern
	}
	if negated {
		return "!" + prefixed
	}
	return prefixed
}

// ignores reports whether ordered gitignore-style rules leave p excluded; later negations can re-include it.
func (r ignoreRules) ignores(p string) bool {
	p = filepath.ToSlash(p)
	ignored := false
	for _, rule := range r {
		negated := strings.HasPrefix(rule, "!")
		pattern := strings.TrimPrefix(rule, "!")
		pattern = filepath.ToSlash(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		matched := false
		if strings.HasSuffix(pattern, "/") {
			matched = strings.HasPrefix(p, pattern) || strings.TrimSuffix(p, "/") == strings.TrimSuffix(pattern, "/")
		} else if ok, _ := filepath.Match(pattern, p); ok {
			matched = true
		} else if ok, _ := filepath.Match(pattern, filepath.Base(p)); ok && !strings.Contains(pattern, "/") {
			matched = true
		} else if strings.HasPrefix(p, strings.TrimSuffix(pattern, "/")+"/") {
			matched = true
		}
		if matched {
			ignored = !negated
		}
	}
	return ignored
}

// isUnderPath uses a separator-aware boundary so siblings sharing a prefix don't inherit a source.
func isUnderPath(target string, root string) bool {
	normalizedRoot := pathutil.ResolvePath(root, "", pathutil.PathInputOptions{})
	resolvedTarget := pathutil.ResolvePath(target, "", pathutil.PathInputOptions{})
	if resolvedTarget == normalizedRoot {
		return true
	}
	prefix := normalizedRoot
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(resolvedTarget, prefix)
}

// relativeTo gives ignore matching slash-separated paths and keeps a usable value when paths can't relate.
func relativeTo(root string, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

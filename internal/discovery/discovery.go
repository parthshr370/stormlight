// Package discovery collects the fixed V1 sources of session context, rules, and skills.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/contextfile"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/skills"
)

// Options configures startup discovery.
type Options struct {
	Cwd                  string
	RepoRoot             string
	AgentDir             string
	HomeDir              string
	SkillPaths           []string
	IncludeDefaultSkills bool
}

// DiagnosticCode classifies non-fatal discovery diagnostics.
type DiagnosticCode string

const (
	DiagnosticSkill     DiagnosticCode = "skill"
	DiagnosticCollision DiagnosticCode = "collision"
	DiagnosticImport    DiagnosticCode = "import"
)

// Diagnostic is a non-fatal source warning.
type Diagnostic struct {
	Code      DiagnosticCode
	Path      string
	Message   string
	Collision *skills.Collision
}

// SessionInputs are the concrete session values produced at startup.
type SessionInputs struct {
	RepoRoot     string
	ContextFiles []prompt.ContextFile
	Skills       []skills.Skill
	GenericRules []string
	Diagnostics  []Diagnostic
}

// ErrorCode classifies fatal discovery failures.
type ErrorCode string

const (
	InvalidOptions  ErrorCode = "invalid_options"
	ResolveCwd      ErrorCode = "resolve_cwd"
	ResolveAgentDir ErrorCode = "resolve_agent_dir"
	ResolveRepoRoot ErrorCode = "resolve_repo_root"
	ReadContext     ErrorCode = "read_context"
	ReadRule        ErrorCode = "read_rule"
	ExpandContext   ErrorCode = "expand_context"
	ExpandRule      ErrorCode = "expand_rule"
	LoadSkills      ErrorCode = "load_skills"
)

// Error is a typed discovery failure.
type Error struct {
	Code ErrorCode
	Op   string
	Path string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("discovery %s %s: %v", e.Code, e.Path, e.Err) }
func (e *Error) Unwrap() error { return e.Err }
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e.Code == other.Code
}

// candidate keeps a source path with its bytes so expansion errors can name the file that supplied them.
type candidate struct {
	path    string
	content []byte
}

// Discover collects context files, sticky rules, and canonical skills once at startup.
func Discover(ctx context.Context, options Options) (SessionInputs, error) {
	if strings.TrimSpace(options.Cwd) == "" || strings.TrimSpace(options.AgentDir) == "" {
		return SessionInputs{}, &Error{Code: InvalidOptions, Op: "options", Err: errors.New("cwd and agent dir are required")}
	}
	cwd, err := canonicalDir(options.Cwd)
	if err != nil {
		return SessionInputs{}, &Error{Code: ResolveCwd, Op: "canonicalize", Path: options.Cwd, Err: err}
	}
	agentDir, err := canonicalDir(options.AgentDir)
	if err != nil {
		return SessionInputs{}, &Error{Code: ResolveAgentDir, Op: "canonicalize", Path: options.AgentDir, Err: err}
	}
	home := options.HomeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" {
		home, _ = canonicalDir(home)
	}
	repoRoot, ancestors, err := resolveRoot(cwd, options.RepoRoot, home)
	if err != nil {
		code := ResolveRepoRoot
		if errors.Is(err, errOutsideRoot) {
			code = InvalidOptions
		}
		return SessionInputs{}, &Error{Code: code, Op: "resolve root", Path: options.RepoRoot, Err: err}
	}
	inputs := SessionInputs{RepoRoot: repoRoot}

	userContext, err := readCandidate(filepath.Join(agentDir, "AGENTS.md"), ReadContext)
	if err != nil {
		return SessionInputs{}, err
	}
	userRule, err := readCandidate(filepath.Join(agentDir, "RULES.md"), ReadRule)
	if err != nil {
		return SessionInputs{}, err
	}
	var projectContexts []candidate
	var nativeRule *candidate
	nativeContextSelected := false
	for _, ancestor := range ancestors {
		if err := ctx.Err(); err != nil {
			return SessionInputs{}, err
		}
		nativeContext, err := readCandidate(filepath.Join(ancestor, config.ProjectConfigDirName, "AGENTS.md"), ReadContext)
		if err != nil {
			return SessionInputs{}, err
		}
		var standalone *candidate
		if !strings.HasPrefix(filepath.Base(ancestor), ".") {
			standalone, err = readCandidate(filepath.Join(ancestor, "AGENTS.md"), ReadContext)
		}
		if err != nil {
			return SessionInputs{}, err
		}
		selectsNative := nativeContext != nil && !nativeContextSelected
		if selectsNative {
			projectContexts = append(projectContexts, *nativeContext)
			nativeContextSelected = true
		} else if standalone != nil {
			projectContexts = append(projectContexts, *standalone)
		}
		if nativeRule == nil {
			rule, err := readCandidate(filepath.Join(ancestor, config.ProjectConfigDirName, "RULES.md"), ReadRule)
			if err != nil {
				return SessionInputs{}, err
			}
			if rule != nil {
				nativeRule = rule
			}
		}
	}
	if userContext != nil {
		item, err := expandContext(ctx, *userContext, home)
		if err != nil {
			return SessionInputs{}, err
		}
		inputs.ContextFiles = append(inputs.ContextFiles, item)
	}
	// ancestors are cwd-first. Reversing yields the required root-first order.
	seenPaths := map[string]struct{}{}
	project := make([]prompt.ContextFile, 0, len(projectContexts))
	for index := len(projectContexts) - 1; index >= 0; index-- {
		item, err := expandContext(ctx, projectContexts[index], home)
		if err != nil {
			return SessionInputs{}, err
		}
		real := canonicalPath(projectContexts[index].path)
		if _, exists := seenPaths[real]; exists {
			continue
		}
		seenPaths[real] = struct{}{}
		project = append(project, item)
	}
	lastContent := make(map[string]int, len(project))
	for index, item := range project {
		lastContent[item.Content] = index
	}
	collapsed := make([]prompt.ContextFile, 0, len(project))
	for index, item := range project {
		if lastContent[item.Content] == index {
			collapsed = append(collapsed, item)
		}
	}
	inputs.ContextFiles = append(inputs.ContextFiles, collapsed...)
	for _, rule := range []*candidate{userRule, nativeRule} {
		if rule == nil {
			continue
		}
		body, err := expandRule(ctx, *rule, home)
		if err != nil {
			return SessionInputs{}, err
		}
		if strings.TrimSpace(body) != "" {
			inputs.GenericRules = append(inputs.GenericRules, body)
		}
	}
	loaded, err := skills.LoadSkills(ctx, skills.LoadOptions{Cwd: cwd, AgentDir: agentDir, SkillPaths: append([]string(nil), options.SkillPaths...), IncludeDefaults: options.IncludeDefaultSkills})
	if err != nil {
		return SessionInputs{}, &Error{Code: LoadSkills, Op: "load skills", Err: err}
	}
	inputs.Skills = loaded.Skills
	for _, diagnostic := range loaded.Diagnostics {
		code := DiagnosticSkill
		if diagnostic.Collision != nil || diagnostic.Type == "collision" {
			code = DiagnosticCollision
		}
		inputs.Diagnostics = append(inputs.Diagnostics, Diagnostic{Code: code, Path: diagnostic.Path, Message: diagnostic.Message, Collision: diagnostic.Collision})
	}
	return inputs, nil
}

// readCandidate treats missing and blank optional sources as absent, keeping discovery fallback selection simple.
func readCandidate(path string, code ErrorCode) (*candidate, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &Error{Code: code, Op: "read", Path: path, Err: err}
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	return &candidate{path: path, content: data}, nil
}

func expandContext(ctx context.Context, item candidate, home string) (prompt.ContextFile, error) {
	content, err := contextfile.Expand(ctx, item.path, item.content, contextfile.ExpandOptions{HomeDir: home})
	if err != nil {
		return prompt.ContextFile{}, &Error{Code: ExpandContext, Op: "expand", Path: item.path, Err: err}
	}
	return prompt.ContextFile{Path: filepath.ToSlash(item.path), Content: content}, nil
}

func expandRule(ctx context.Context, item candidate, home string) (string, error) {
	content, err := contextfile.Expand(ctx, item.path, item.content, contextfile.ExpandOptions{HomeDir: home})
	if err != nil {
		return "", &Error{Code: ExpandRule, Op: "expand", Path: item.path, Err: err}
	}
	return content, nil
}

var errOutsideRoot = errors.New("cwd is outside repo root")

// resolveRoot picks the boundary for ancestor discovery: an explicit root wins, then Git, home, or the filesystem root.
func resolveRoot(cwd, suppliedRoot, home string) (string, []string, error) {
	boundary := ""
	if suppliedRoot != "" {
		root, err := canonicalDir(suppliedRoot)
		if err != nil {
			return "", nil, err
		}
		if !within(root, cwd) {
			return "", nil, errOutsideRoot
		}
		boundary = root
	} else {
		for dir := cwd; ; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				boundary = dir
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
		if boundary == "" && home != "" && within(home, cwd) {
			boundary = home
		}
		if boundary == "" {
			for boundary = cwd; filepath.Dir(boundary) != boundary; boundary = filepath.Dir(boundary) {
			}
		}
	}
	ancestors := make([]string, 0)
	for dir := cwd; ; dir = filepath.Dir(dir) {
		ancestors = append(ancestors, dir)
		if dir == boundary {
			break
		}
	}
	return boundary, ancestors, nil
}

// canonicalDir resolves symlinks when possible but preserves a clean absolute path for an absent directory.
func canonicalDir(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

// canonicalPath gives duplicate checks a stable identity while tolerating paths that can't be resolved.
func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverOrdersContextsAndRules(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "pkg")
	agentDir := filepath.Join(root, "agent")
	for _, path := range []string{
		filepath.Join(root, ".git", "keep"),
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(cwd, "AGENTS.md"),
		filepath.Join(cwd, ".harness", "AGENTS.md"),
		filepath.Join(agentDir, "AGENTS.md"),
		filepath.Join(agentDir, "RULES.md"),
		filepath.Join(root, ".harness", "RULES.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "AGENTS.md"), "root")
	write(t, filepath.Join(cwd, "AGENTS.md"), "standalone")
	write(t, filepath.Join(cwd, ".harness", "AGENTS.md"), "native")
	write(t, filepath.Join(agentDir, "AGENTS.md"), "user")
	write(t, filepath.Join(agentDir, "RULES.md"), "user rule")
	write(t, filepath.Join(root, ".harness", "RULES.md"), "project rule")
	inputs, err := Discover(context.Background(), Options{Cwd: cwd, AgentDir: agentDir, IncludeDefaultSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.ContextFiles) != 3 {
		t.Fatalf("contexts = %#v", inputs.ContextFiles)
	}
	for index, want := range []string{"user", "root", "native"} {
		if inputs.ContextFiles[index].Content != want {
			t.Fatalf("context %d = %#v, want %q", index, inputs.ContextFiles[index], want)
		}
	}
	if len(inputs.GenericRules) != 2 || inputs.GenericRules[0] != "user rule" || inputs.GenericRules[1] != "project rule" {
		t.Fatalf("rules = %#v", inputs.GenericRules)
	}
}

func TestDiscoverSkipsDotDirectoryStandaloneCandidatesBeforeRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create func(t *testing.T, path string)
	}{
		{
			name: "readable",
			create: func(t *testing.T, path string) {
				write(t, path, "must be skipped")
			},
		},
		{
			name: "directory",
			create: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			agentDir := filepath.Join(root, "agent")
			cwd := filepath.Join(root, ".private", "pkg")
			for _, path := range []string{agentDir, cwd, filepath.Join(root, ".git")} {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(t, filepath.Join(root, "AGENTS.md"), "root instructions")
			tc.create(t, filepath.Join(root, ".private", "AGENTS.md"))

			inputs, err := Discover(context.Background(), Options{Cwd: cwd, AgentDir: agentDir})
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}
			if len(inputs.ContextFiles) != 1 || inputs.ContextFiles[0].Content != "root instructions" {
				t.Fatalf("contexts = %#v", inputs.ContextFiles)
			}
		})
	}
}

func TestDiscoverRulesRootBoundariesAndTypedFailures(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "nested", "cwd")
	agentDir := filepath.Join(root, "agent")
	for _, path := range []string{cwd, agentDir, filepath.Join(root, ".harness"), filepath.Join(cwd, ".harness")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, ".harness", "include.md"), "expanded rule")
	write(t, filepath.Join(root, ".harness", "RULES.md"), "@./include.md")
	write(t, filepath.Join(root, "RULES.md"), "standalone rule must be ignored")
	write(t, filepath.Join(cwd, ".harness", "RULES.md"), " \n")
	inputs, err := Discover(context.Background(), Options{Cwd: cwd, AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if inputs.RepoRoot != canonicalRoot || len(inputs.GenericRules) != 1 || inputs.GenericRules[0] != "expanded rule" {
		t.Fatalf("discovery inputs = %#v", inputs)
	}

	_, err = Discover(context.Background(), Options{Cwd: cwd, AgentDir: agentDir, RepoRoot: t.TempDir()})
	var discoveryErr *Error
	if !errors.As(err, &discoveryErr) || discoveryErr.Code != InvalidOptions {
		t.Fatalf("outside RepoRoot error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Discover(ctx, Options{Cwd: cwd, AgentDir: agentDir})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery error = %v", err)
	}

	if err := os.Mkdir(filepath.Join(agentDir, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Discover(context.Background(), Options{Cwd: cwd, AgentDir: agentDir})
	if !errors.As(err, &discoveryErr) || discoveryErr.Code != ReadContext {
		t.Fatalf("unreadable top-level context error = %v", err)
	}
}

func TestResolveRootFallsBackToHomeAndVolume(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "project", "nested")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	root, ancestors, err := resolveRoot(cwd, "", home)
	if err != nil || root != home || len(ancestors) != 3 {
		t.Fatalf("home fallback = %q, %#v, %v", root, ancestors, err)
	}
	outside := t.TempDir()
	root, ancestors, err = resolveRoot(outside, "", home)
	if err != nil || root != filepath.VolumeName(outside)+string(filepath.Separator) || len(ancestors) == 0 {
		t.Fatalf("volume fallback = %q, %#v, %v", root, ancestors, err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverCollapsesIdenticalProjectContextToNearest(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "nested")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "AGENTS.md"), filepath.Join(root, "middle.md"), filepath.Join(cwd, "AGENTS.md")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "AGENTS.md"), "same")
	write(t, filepath.Join(cwd, "AGENTS.md"), "same")
	inputs, err := Discover(context.Background(), Options{Cwd: cwd, AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	wantPath, err := filepath.EvalSymlinks(filepath.Join(cwd, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.ContextFiles) != 1 || inputs.ContextFiles[0].Path != filepath.ToSlash(wantPath) {
		t.Fatalf("collapsed contexts = %#v", inputs.ContextFiles)
	}
}

func TestDiscoverSelectedNativeCoexistsWithFartherStandalone(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "nested")
	agentDir := filepath.Join(root, "agent")
	for _, path := range []string{
		filepath.Join(root, ".git", "keep"),
		filepath.Join(root, ".harness", "AGENTS.md"),
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(cwd, ".harness", "AGENTS.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, ".harness", "AGENTS.md"), "far native")
	write(t, filepath.Join(root, "AGENTS.md"), "far standalone")
	write(t, filepath.Join(cwd, ".harness", "AGENTS.md"), "near native")
	inputs, err := Discover(context.Background(), Options{Cwd: cwd, AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.ContextFiles) != 2 || inputs.ContextFiles[0].Content != "far standalone" || inputs.ContextFiles[1].Content != "near native" {
		t.Fatalf("contexts = %#v", inputs.ContextFiles)
	}
}

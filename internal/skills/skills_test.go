package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateNameAndDescription(t *testing.T) {
	if errs := ValidateName("valid-skill-1"); len(errs) != 0 {
		t.Fatalf("valid name errors = %v", errs)
	}
	errs := ValidateName("-Bad--Name-")
	for _, want := range []string{"invalid characters", "must not start or end", "consecutive hyphens"} {
		if !containsDiagnostic(errs, want) {
			t.Fatalf("ValidateName errors %v missing %q", errs, want)
		}
	}
	if errs := ValidateDescription(""); len(errs) != 1 || errs[0] != "description is required" {
		t.Fatalf("ValidateDescription = %v", errs)
	}
}

func TestLoadSkillsFromDir(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "root.md"), "---\nname: root-skill\ndescription: Root <skill> & test\n---\nbody")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(dir, "nested", "SKILL.md"), "---\ndescription: Nested skill\ndisable-model-invocation: true\n---\nbody")
	writeSkill(t, filepath.Join(dir, "bad.md"), "---\nname: Bad\n---\nbody")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(dir, "ignored.md"), "---\nname: ignored\ndescription: ignored\n---\nbody")

	result, err := LoadSkillsFromDir(context.Background(), LoadFromDirOptions{Dir: dir, Source: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skills) != 2 {
		t.Fatalf("skills = %+v diagnostics=%+v", result.Skills, result.Diagnostics)
	}
	if result.Skills[0].Name != "nested" || !result.Skills[0].DisableModelInvocation || result.Skills[0].SourceInfo.Scope != "project" {
		t.Fatalf("nested skill = %+v", result.Skills[0])
	}
	if result.Skills[1].Name != "root-skill" {
		t.Fatalf("root skill = %+v", result.Skills[1])
	}
	if !hasDiagnostic(result.Diagnostics, "description is required") || !hasDiagnostic(result.Diagnostics, "invalid characters") {
		t.Fatalf("diagnostics = %+v", result.Diagnostics)
	}
}

func TestLoadSkillsDedupeAndCollision(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	userDir := filepath.Join(agentDir, "skills", "dup")
	projectDir := filepath.Join(dir, ".harness", "skills", "dup2")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(userDir, "SKILL.md"), "---\nname: same\ndescription: user wins\n---\n")
	writeSkill(t, filepath.Join(projectDir, "SKILL.md"), "---\nname: same\ndescription: project loses\n---\n")

	result, err := LoadSkills(context.Background(), LoadOptions{Cwd: dir, AgentDir: agentDir, IncludeDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skills) != 1 || result.Skills[0].Description != "user wins" {
		t.Fatalf("skills = %+v", result.Skills)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Type != "collision" || result.Diagnostics[0].Collision.Name != "same" {
		t.Fatalf("diagnostics = %+v", result.Diagnostics)
	}
}

func TestLoadBodyAndScannerEdgeCases(t *testing.T) {
	dir := t.TempDir()
	withFrontmatter := filepath.Join(dir, "with-frontmatter.md")
	withoutFrontmatter := filepath.Join(dir, "plain.md")
	writeSkill(t, withFrontmatter, "---\ndescription: example\n---\n body \n")
	writeSkill(t, withoutFrontmatter, " plain body \n")
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"frontmatter", withFrontmatter, "body"},
		{"plain", withoutFrontmatter, "plain body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadBody(context.Background(), Skill{FilePath: tc.path})
			if err != nil || got != tc.want {
				t.Fatalf("LoadBody() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := LoadBody(context.Background(), Skill{FilePath: filepath.Join(dir, "missing.md")}); err == nil {
		t.Fatal("LoadBody missing file succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadBody(ctx, Skill{FilePath: withFrontmatter}); err != context.Canceled {
		t.Fatalf("cancelled LoadBody error = %v", err)
	}

	scanDir := filepath.Join(dir, "scan")
	if err := os.MkdirAll(filepath.Join(scanDir, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scanDir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scanDir, "visible"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(scanDir, ".hidden", "SKILL.md"), "---\ndescription: hidden\n---\n")
	writeSkill(t, filepath.Join(scanDir, "node_modules", "SKILL.md"), "---\ndescription: dependency\n---\n")
	writeSkill(t, filepath.Join(scanDir, "visible", "SKILL.md"), "---\ndescription: visible\nhide: true\n---\n")
	result, err := LoadSkillsFromDir(context.Background(), LoadFromDirOptions{Dir: scanDir, Source: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skills) != 1 || result.Skills[0].Name != "visible" || !result.Skills[0].DisableModelInvocation {
		t.Fatalf("scanner result = %#v", result.Skills)
	}
}

func writeSkill(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasDiagnostic(diags []Diagnostic, text string) bool {
	for _, diag := range diags {
		if strings.Contains(diag.Message, text) {
			return true
		}
	}
	return false
}

func containsDiagnostic(diags []string, text string) bool {
	for _, diag := range diags {
		if strings.Contains(diag, text) {
			return true
		}
	}
	return false
}

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetAgentDirUsesEnvWithTildeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvAgentDir, "~/custom-agent")

	got := GetAgentDir()
	want := filepath.Join(home, "custom-agent")
	if got != want {
		t.Fatalf("GetAgentDir() = %q, want %q", got, want)
	}
}

func TestGetAgentDirDefaultAndFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.Unsetenv(EnvAgentDir)

	wantAgentDir := filepath.Join(home, ".config", "harness", "agent")
	if got := GetAgentDir(); got != wantAgentDir {
		t.Fatalf("GetAgentDir() = %q, want %q", got, wantAgentDir)
	}
	if got := GetModelsPath(); got != filepath.Join(wantAgentDir, "models.json") {
		t.Fatalf("GetModelsPath() = %q", got)
	}
	if got := GetAuthPath(); got != filepath.Join(wantAgentDir, "auth.json") {
		t.Fatalf("GetAuthPath() = %q", got)
	}
	if got := GetSettingsPath(); got != filepath.Join(home, ".config", "harness", "settings.json") {
		t.Fatalf("GetSettingsPath() = %q", got)
	}
}

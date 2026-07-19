package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFauxScript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script.json")
	if err := os.WriteFile(path, []byte(`[{"text":"hello"},{"toolCalls":[{"name":"read","arguments":{"path":"x"}}]}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	provider, model, err := loadFauxScript(path)
	if err != nil {
		t.Fatal(err)
	}
	if model.Provider != "faux" || provider.PendingCount() != 2 {
		t.Fatalf("model/provider = %+v pending=%d", model, provider.PendingCount())
	}
}

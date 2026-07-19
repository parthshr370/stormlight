package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRetrySettingsPrecedenceAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"retry_enabled":false,"retry_max_attempts":4,"retry_base_delay_ms":25,"retry_backoff_cap_ms":100,"retry_max_delay_ms":0,"retry_jitter_min":0.5,"retry_jitter_max":0.75}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HARNESS_RETRY_ENABLED": "true", "HARNESS_RETRY_MAX_ATTEMPTS": "5"}
	resolved, err := (Config{SettingsPath: path, LookupEnv: func(key string) string { return env[key] }}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	settings := resolved.Retry()
	if !settings.Enabled || settings.MaxAttempts != 5 || settings.BaseDelayMS != 25 || settings.BackoffCapMS != 100 || settings.MaxDelayMS != 0 || settings.JitterMin != .5 || settings.JitterMax != .75 {
		t.Fatalf("retry settings = %+v", settings)
	}
	env["HARNESS_RETRY_JITTER_MAX"] = "0.1"
	if _, err := (Config{SettingsPath: path, LookupEnv: func(key string) string { return env[key] }}).Resolve(); err == nil {
		t.Fatal("invalid jitter range resolved")
	}
}

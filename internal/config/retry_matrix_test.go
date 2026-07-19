package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func retryResolved(t *testing.T, settings string, env map[string]string) (*ResolvedConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if settings != "" {
		if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return (Config{SettingsPath: path, LookupEnv: func(key string) string { return env[key] }}).Resolve()
}

func requireRetryConfigError(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatal("invalid retry config resolved")
	}
	var configErr *Error
	if !errors.As(err, &configErr) || configErr.Field != field {
		t.Fatalf("error=%v, want config field %q", err, field)
	}
}

func TestRetryDefaultsAndNonPositiveMaxDelay(t *testing.T) {
	resolved, err := retryResolved(t, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Retry(); !got.Enabled || got.MaxAttempts != 11 || got.BaseDelayMS != 500 || got.BackoffCapMS != 8000 || got.MaxDelayMS != 300000 || got.JitterMin != .75 || got.JitterMax != 1 {
		t.Fatalf("default retry settings = %+v", got)
	}
	for _, value := range []int{0, -1} {
		resolved, err := retryResolved(t, fmt.Sprintf(`{"retry_max_delay_ms":%d}`, value), nil)
		if err != nil || resolved.Retry().MaxDelayMS != value {
			t.Fatalf("max delay %d resolved=%+v err=%v", value, resolved, err)
		}
	}
}

func TestRetrySettingsAndEnvironmentValidationMatrix(t *testing.T) {
	invalidSettings := []struct {
		name, json, field string
	}{
		{"attempts", `{"retry_max_attempts":0}`, "retry_max_attempts"},
		{"base", `{"retry_base_delay_ms":0}`, "retry_base_delay_ms"},
		{"cap", `{"retry_base_delay_ms":10,"retry_backoff_cap_ms":9}`, "retry_backoff_cap_ms"},
		{"jitter-min", `{"retry_jitter_min":-0.1}`, "retry_jitter_min"},
		{"jitter-max", `{"retry_jitter_max":1.1}`, "retry_jitter_max"},
		{"jitter-reversed", `{"retry_jitter_min":0.8,"retry_jitter_max":0.7}`, "retry_jitter_max"},
	}
	for _, tc := range invalidSettings {
		t.Run("settings-"+tc.name, func(t *testing.T) {
			_, err := retryResolved(t, tc.json, nil)
			requireRetryConfigError(t, err, tc.field)
		})
	}
	tooLarge := maxRetryDurationMS + 1
	for _, tc := range []struct {
		name, env, field string
	}{
		{"env-attempts", "HARNESS_RETRY_MAX_ATTEMPTS", "retry_max_attempts"},
		{"env-base", "HARNESS_RETRY_BASE_DELAY_MS", "retry_base_delay_ms"},
		{"env-cap", "HARNESS_RETRY_BACKOFF_CAP_MS", "retry_backoff_cap_ms"},
		{"env-max", "HARNESS_RETRY_MAX_DELAY_MS", "retry_max_delay_ms"},
	} {
		t.Run(tc.name+"-overflow", func(t *testing.T) {
			value := fmt.Sprintf("%d", tooLarge)
			if tc.env == "HARNESS_RETRY_MAX_ATTEMPTS" {
				value = "0"
			}
			_, err := retryResolved(t, "", map[string]string{tc.env: value})
			requireRetryConfigError(t, err, tc.field)
		})
	}
	for _, tc := range []struct {
		name, key, value, field string
	}{
		{"env-jitter-nan", "HARNESS_RETRY_JITTER_MIN", "NaN", "retry_jitter_min"},
		{"env-jitter-inf", "HARNESS_RETRY_JITTER_MAX", "Inf", "retry_jitter_max"},
		{"env-jitter-reversed", "HARNESS_RETRY_JITTER_MAX", "0.5", "retry_jitter_max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{tc.key: tc.value}
			if tc.name == "env-jitter-reversed" {
				env["HARNESS_RETRY_JITTER_MIN"] = "0.75"
			}
			_, err := retryResolved(t, "", env)
			requireRetryConfigError(t, err, tc.field)
		})
	}
	for _, tc := range []struct {
		name, json, field string
	}{
		{"settings-base-overflow", fmt.Sprintf(`{"retry_base_delay_ms":%d}`, tooLarge), "retry_base_delay_ms"},
		{"settings-cap-overflow", fmt.Sprintf(`{"retry_backoff_cap_ms":%d}`, tooLarge), "retry_backoff_cap_ms"},
		{"settings-max-overflow", fmt.Sprintf(`{"retry_max_delay_ms":%d}`, tooLarge), "retry_max_delay_ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := retryResolved(t, tc.json, nil)
			requireRetryConfigError(t, err, tc.field)
		})
	}
}

func TestRetryDisabledSettingsRemainResolved(t *testing.T) {
	resolved, err := retryResolved(t, `{"retry_enabled":false,"retry_max_attempts":4,"retry_base_delay_ms":25,"retry_backoff_cap_ms":100,"retry_max_delay_ms":0,"retry_jitter_min":0.5,"retry_jitter_max":0.75}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Retry(); got.Enabled || got.MaxAttempts != 4 || got.BaseDelayMS != 25 || got.BackoffCapMS != 100 || got.MaxDelayMS != 0 || got.JitterMin != .5 || got.JitterMax != .75 {
		t.Fatalf("disabled retry settings = %+v", got)
	}
}

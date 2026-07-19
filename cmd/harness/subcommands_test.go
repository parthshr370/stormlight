package main

import (
	"bytes"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/build"
)

func TestDoctorRedactsCredentialsAndWarnsWithoutThem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "visible-secret")
	var withCredential bytes.Buffer
	if code := runDoctor(nil, &withCredential, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor exit = %d", code)
	}
	if output := withCredential.String(); strings.Contains(output, "visible-secret") || !strings.Contains(output, "REDACTED") || !strings.Contains(output, "PASS default provider credential") {
		t.Fatalf("doctor output did not redact/presence-check credential: %s", output)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	var withoutCredential bytes.Buffer
	if code := runDoctor([]string{"-model", "openai:gpt-x@https://ex/"}, &withoutCredential, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor exit = %d", code)
	}
	output := withoutCredential.String()
	if !strings.Contains(output, "provider=openai") || !strings.Contains(output, "WARN default provider credential: openai absent") {
		t.Fatalf("doctor output = %s", output)
	}
}

func TestDoctorFailsOnHardConfigurationError(t *testing.T) {
	var stderr bytes.Buffer
	if code := runDoctor([]string{"-model", "unknown:model"}, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("doctor succeeded for unsupported provider")
	}
}

func TestVersionOutput(t *testing.T) {
	var output bytes.Buffer
	if code := runVersion(&output); code != 0 {
		t.Fatalf("version exit = %d", code)
	}
	metadata := build.Current()
	want := build.Name + " version=" + metadata.Version + " commit=" + metadata.Commit + " date=" + metadata.Date + "\n"
	if output.String() != want {
		t.Fatalf("version output = %q, want %q", output.String(), want)
	}
}

func TestVersionFlagDispatchIgnoresInvalidConfig(t *testing.T) {
	t.Setenv("HARNESS_MAX_TOKENS", "not-an-int")
	var output, stderr bytes.Buffer
	if code := run([]string{"-version"}, &output, &stderr); code != 0 {
		t.Fatalf("-version exit = %d, stderr = %q", code, stderr.String())
	}
	if got := output.String(); !strings.Contains(got, build.Name+" version=") || !strings.Contains(got, " commit=") || !strings.Contains(got, " date=") {
		t.Fatalf("-version output = %q", got)
	}
}

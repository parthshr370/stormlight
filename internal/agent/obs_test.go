package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestRedactTruncateMasksSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		leak string // must NOT appear in the output
	}{
		{"env api key", "EXAMPLE_API_KEY=lz-supersecret12345", "lz-supersecret12345"},
		{"anthropic key", "ANTHROPIC_API_KEY=sk-ant-abcdefgh12345", "sk-ant-abcdefgh12345"},
		{"json key", `{"api_key":"jsonsecretvalue123"}`, "jsonsecretvalue123"},
		{"json prefixed key", `{"EXAMPLE_API_KEY":"jsonsecretvalue123"}`, "jsonsecretvalue123"},
		{"kv token", "token: bearerlessSecret999", "bearerlessSecret999"},
		{"authorization bearer", "Authorization: Bearer abcDEF123456789tokenvalue", "abcDEF123456789tokenvalue"},
		{"sk key bare", "here is sk-abcdefgh12345 in text", "sk-abcdefgh12345"},
		{"url creds", "https://user:p4ssw0rdsecret@host.example.com/x", "p4ssw0rdsecret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactTruncate(tc.in, 1000)
			if strings.Contains(got, tc.leak) {
				t.Fatalf("secret leaked: %q -> %q", tc.in, got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("expected redaction marker in %q", got)
			}
		})
	}
}

func TestRedactTruncateLeavesCleanText(t *testing.T) {
	in := "listing 3 files under src/app"
	if got := redactTruncate(in, 1000); got != in {
		t.Fatalf("non-secret text altered: %q -> %q", in, got)
	}
}

func TestRedactTruncateBounds(t *testing.T) {
	got := redactTruncate(strings.Repeat("a", 500), 100)
	if !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	if n := len([]rune(strings.TrimSuffix(got, "…[truncated]"))); n != 100 {
		t.Fatalf("expected 100 runes before marker, got %d", n)
	}
}

func TestRedactTruncateCollapsesWhitespace(t *testing.T) {
	got := redactTruncate("line one\n\n   line two\ttab", 1000)
	if got != "line one line two tab" {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
}

func TestLogPreviewRedactsAndIgnoresNonText(t *testing.T) {
	blocks := []types.ContentBlock{
		types.NewText("result with EXAMPLE_API_KEY=topsecret98765 and more"),
		{Type: types.BlockToolCall, Name: "x"}, // non-text: ignored
	}
	got := logPreview(blocks, 1000)
	if strings.Contains(got, "topsecret98765") {
		t.Fatalf("secret leaked in preview: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %q", got)
	}
}

func TestContentBytes(t *testing.T) {
	blocks := []types.ContentBlock{types.NewText("abc"), types.NewText("de")}
	if n := contentBytes(blocks); n != 5 {
		t.Fatalf("contentBytes = %d, want 5", n)
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

// TestExecutePreparedToolCallLogsOK proves the tool-call logging path: a
// successful tool logs "tool call ok" with result_bytes at info, and the debug
// content preview is redacted before it reaches the log.
func TestExecutePreparedToolCallLogsOK(t *testing.T) {
	buf := captureLogs(t)
	tool := AgentTool{
		Tool:  types.Tool{Name: "read"},
		Label: "read",
		Execute: func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("file body with EXAMPLE_API_KEY=leakme1234567 inside")}}, nil
		},
	}
	prepared := &preparedToolCall{
		toolCall: types.NewToolCall("call_1", "read", json.RawMessage(`{}`)),
		tool:     tool,
		args:     json.RawMessage(`{}`),
	}
	emit := func(ctx context.Context, ev AgentEvent) error { return nil }
	out := executePreparedToolCall(context.Background(), prepared, emit)
	if out.isError {
		t.Fatalf("unexpected error outcome")
	}
	logs := buf.String()
	if !strings.Contains(logs, "tool call ok") || !strings.Contains(logs, "result_bytes") {
		t.Fatalf("missing ok/result_bytes log: %s", logs)
	}
	if strings.Contains(logs, "leakme1234567") {
		t.Fatalf("secret leaked into logs: %s", logs)
	}
	if !strings.Contains(logs, "[REDACTED]") {
		t.Fatalf("expected redacted preview: %s", logs)
	}
}

// TestExecutePreparedToolCallLogsError proves a failing tool logs a redacted,
// bounded error at error level.
func TestExecutePreparedToolCallLogsError(t *testing.T) {
	buf := captureLogs(t)
	tool := AgentTool{
		Tool:  types.Tool{Name: "bash"},
		Label: "bash",
		Execute: func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
			return AgentToolResult{}, fmt.Errorf("boom token=leaksecret7654321")
		},
	}
	prepared := &preparedToolCall{
		toolCall: types.NewToolCall("call_2", "bash", json.RawMessage(`{}`)),
		tool:     tool,
		args:     json.RawMessage(`{}`),
	}
	emit := func(ctx context.Context, ev AgentEvent) error { return nil }
	out := executePreparedToolCall(context.Background(), prepared, emit)
	if !out.isError {
		t.Fatalf("expected error outcome")
	}
	logs := buf.String()
	if !strings.Contains(logs, "tool call failed") {
		t.Fatalf("missing error log: %s", logs)
	}
	if strings.Contains(logs, "leaksecret7654321") {
		t.Fatalf("secret leaked into error log: %s", logs)
	}
}

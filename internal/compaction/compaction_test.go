package compaction

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

func TestFileOpsAndFormatting(t *testing.T) {
	fileOps := CreateFileOps()
	ExtractFileOpsFromMessage(ptypes.AssistantMessage{Content: []ptypes.ContentBlock{
		ptypes.NewToolCall("r", "read", raw(`{"path":"a.txt"}`)),
		ptypes.NewToolCall("w", "write", raw(`{"path":"b.txt"}`)),
		ptypes.NewToolCall("e", "edit", raw(`{"path":"a.txt"}`)),
	}}, fileOps)
	readFiles, modifiedFiles := ComputeFileLists(fileOps)
	if strings.Join(readFiles, ",") != "" || strings.Join(modifiedFiles, ",") != "a.txt,b.txt" {
		t.Fatalf("read=%v modified=%v", readFiles, modifiedFiles)
	}
	formatted := FormatFileOperations([]string{"read.md"}, modifiedFiles)
	for _, want := range []string{"<read-files>", "read.md", "<modified-files>", "a.txt", "b.txt"} {
		if !strings.Contains(formatted, want) {
			t.Fatalf("formatted missing %q: %s", want, formatted)
		}
	}
}

func TestSerializeConversationAndEstimate(t *testing.T) {
	messages := []ptypes.Message{
		ptypes.UserMessage{Content: ptypes.StringContent("hello")},
		ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText("hi"), ptypes.NewToolCall("x", "read", raw(`{"path":"a","limit":2}`))}},
		ptypes.ToolResultMessage{Content: []ptypes.ContentBlock{ptypes.NewText(strings.Repeat("a", ToolResultMaxChars+4))}},
	}
	serialized := SerializeConversation(messages)
	for _, want := range []string{"[User]: hello", "[Assistant]: hi", "read(path=\"a\", limit=2)", "[... 4 more characters truncated]"} {
		if !strings.Contains(serialized, want) {
			t.Fatalf("serialized missing %q:\n%s", want, serialized)
		}
	}
	if got := EstimateTokens(messages[0]); got != 2 {
		t.Fatalf("EstimateTokens user = %d", got)
	}
}

func TestEstimateContextTokensUsesLastAssistantUsage(t *testing.T) {
	usage := ptypes.Usage{Input: 10, Output: 5, TotalTokens: 20}
	messages := []ptypes.Message{
		ptypes.UserMessage{Content: ptypes.StringContent("old")},
		ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText("done")}, Usage: usage, StopReason: ptypes.StopStop},
		ptypes.UserMessage{Content: ptypes.StringContent("next")},
	}
	estimate := EstimateContextTokens(messages)
	if estimate.Tokens != 21 || estimate.UsageTokens != 20 || estimate.TrailingTokens != 1 || estimate.LastUsageIndex == nil || *estimate.LastUsageIndex != 1 {
		t.Fatalf("estimate = %+v", estimate)
	}
	if !ShouldCompact(90, 100, CompactionSettings{Enabled: true, ReserveTokens: 20}) {
		t.Fatal("should compact")
	}
}

func TestFindCutPointAndPrepareCompaction(t *testing.T) {
	entries := []SessionEntry{
		{ID: "1", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent(strings.Repeat("u", 40))}},
		{ID: "2", Type: "message", Message: ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText(strings.Repeat("a", 40))}}},
		{ID: "3", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent("recent")}},
	}
	cut := FindCutPoint(entries, 0, len(entries), 2)
	if cut.FirstKeptEntryIndex != 2 || cut.TurnStartIndex != -1 || cut.IsSplitTurn {
		t.Fatalf("cut = %+v", cut)
	}
	prep := PrepareCompaction(entries, CompactionSettings{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 2})
	if prep == nil || prep.FirstKeptEntryID != "3" || len(prep.MessagesToSummarize) != 2 {
		t.Fatalf("prep = %+v", prep)
	}
}

func TestProjectToolResultsSpillsOldResults(t *testing.T) {
	projectDir := t.TempDir()
	spillDir := t.TempDir()
	messages := []ptypes.Message{
		ptypes.ToolResultMessage{ToolCallID: "old/1", ToolName: "bash", Content: []ptypes.ContentBlock{ptypes.NewText("old output")}},
		ptypes.ToolResultMessage{ToolCallID: "new", ToolName: "read", Content: []ptypes.ContentBlock{ptypes.NewText("new output")}},
	}
	projected, err := ProjectToolResults(context.Background(), messages, TransformOptions{Cwd: projectDir, ToolUseWindow: 1, SpillToolResults: true, SpillDir: spillDir})
	if err != nil {
		t.Fatal(err)
	}
	oldResult := projected[0].(ptypes.ToolResultMessage)
	placeholder := oldResult.Content[0].Text
	if !strings.Contains(placeholder, spillDir) {
		t.Fatalf("old result text = %q", placeholder)
	}
	_, path, ok := strings.Cut(placeholder, "Full output: ")
	if !ok {
		t.Fatalf("old result does not include spill path: %q", placeholder)
	}
	path = strings.TrimSuffix(path, "]")
	if !filepath.IsAbs(path) {
		t.Fatalf("spill path is not absolute: %q", path)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(spillDir, "old_1-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != path {
		t.Fatalf("spill matches = %v, path = %q", matches, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "old output") {
		t.Fatalf("spill file = %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".harness", "tool-outputs")); !os.IsNotExist(err) {
		t.Fatalf("project tool output dir should not exist; err=%v", err)
	}
	newResult := projected[1].(ptypes.ToolResultMessage)
	if newResult.Content[0].Text != "new output" {
		t.Fatalf("new result changed: %+v", newResult)
	}
}

func TestProjectToolResultsSpillFileNamesAvoidSanitizedCollisions(t *testing.T) {
	projectDir := t.TempDir()
	spillDir := t.TempDir()
	messages := []ptypes.Message{
		ptypes.ToolResultMessage{ToolCallID: "a/b", ToolName: "bash", Content: []ptypes.ContentBlock{ptypes.NewText("slash output")}},
		ptypes.ToolResultMessage{ToolCallID: "a_b", ToolName: "bash", Content: []ptypes.ContentBlock{ptypes.NewText("underscore output")}},
		ptypes.ToolResultMessage{ToolCallID: "new", ToolName: "read", Content: []ptypes.ContentBlock{ptypes.NewText("new output")}},
	}
	projected, err := ProjectToolResults(context.Background(), messages, TransformOptions{Cwd: projectDir, ToolUseWindow: 1, SpillToolResults: true, SpillDir: spillDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range projected[:2] {
		result := message.(ptypes.ToolResultMessage)
		placeholder := result.Content[0].Text
		if !strings.Contains(placeholder, spillDir) {
			t.Fatalf("spill placeholder = %q", placeholder)
		}
		_, path, ok := strings.Cut(placeholder, "Full output: ")
		if !ok {
			t.Fatalf("spill placeholder does not include path: %q", placeholder)
		}
		path = strings.TrimSuffix(path, "]")
		if !filepath.IsAbs(path) {
			t.Fatalf("spill path = %q", path)
		}
		if _, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}
	matches, err := filepath.Glob(filepath.Join(spillDir, "a_b-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || matches[0] == matches[1] {
		t.Fatalf("expected two distinct spill files, got %v", matches)
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".harness", "tool-outputs")); !os.IsNotExist(err) {
		t.Fatalf("project tool output dir should not exist; err=%v", err)
	}
}

func TestProjectToolResultsTightensSpillDirPerms(t *testing.T) {
	projectDir := t.TempDir()
	spillDir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(spillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	messages := []ptypes.Message{
		ptypes.ToolResultMessage{ToolCallID: "old", ToolName: "bash", Content: []ptypes.ContentBlock{ptypes.NewText("old output")}},
		ptypes.ToolResultMessage{ToolCallID: "new", ToolName: "read", Content: []ptypes.ContentBlock{ptypes.NewText("new output")}},
	}
	projected, err := ProjectToolResults(context.Background(), messages, TransformOptions{Cwd: projectDir, ToolUseWindow: 1, SpillToolResults: true, SpillDir: spillDir})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(spillDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("spill dir perm = %o, want 0700", info.Mode().Perm())
	}
	_, path, ok := strings.Cut(projected[0].(ptypes.ToolResultMessage).Content[0].Text, "Full output: ")
	if !ok {
		t.Fatalf("no spill path in placeholder")
	}
	fileInfo, err := os.Stat(strings.TrimSuffix(path, "]"))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("spill file perm = %o, want 0600", fileInfo.Mode().Perm())
	}
}

func TestProjectToolResultsCanAvoidSpill(t *testing.T) {
	dir := t.TempDir()
	messages := []ptypes.Message{
		ptypes.ToolResultMessage{ToolCallID: "old/1", ToolName: "bash", Content: []ptypes.ContentBlock{ptypes.NewText("old output")}},
		ptypes.ToolResultMessage{ToolCallID: "new", ToolName: "read", Content: []ptypes.ContentBlock{ptypes.NewText("new output")}},
	}
	projected, err := ProjectToolResults(context.Background(), messages, TransformOptions{Cwd: dir, ToolUseWindow: 1, SpillToolResults: false})
	if err != nil {
		t.Fatal(err)
	}
	oldResult := projected[0].(ptypes.ToolResultMessage)
	if strings.Contains(oldResult.Content[0].Text, "Full output:") {
		t.Fatalf("old result should not reference spill file: %q", oldResult.Content[0].Text)
	}
	if _, err := os.Stat(filepath.Join(dir, ".harness", "tool-outputs")); !os.IsNotExist(err) {
		t.Fatalf("tool output dir should not exist; err=%v", err)
	}
}

func TestProjectToolResultsRejectsSpillWithoutDirectory(t *testing.T) {
	projectDir := t.TempDir()
	messages := []ptypes.Message{
		ptypes.ToolResultMessage{ToolCallID: "old", ToolName: "bash", Content: []ptypes.ContentBlock{ptypes.NewText("old output")}},
		ptypes.ToolResultMessage{ToolCallID: "new", ToolName: "read", Content: []ptypes.ContentBlock{ptypes.NewText("new output")}},
	}
	if _, err := ProjectToolResults(context.Background(), messages, TransformOptions{Cwd: projectDir, ToolUseWindow: 1, SpillToolResults: true}); err == nil {
		t.Fatal("ProjectToolResults should reject spilling without SpillDir")
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("spill without directory wrote project files: %v", entries)
	}
}

func TestSummaryEntriesUseWrappedMessages(t *testing.T) {
	branch := getMessageFromEntry(SessionEntry{Type: "branch_summary", Summary: "branch", Timestamp: 1}).(ptypes.UserMessage)
	if got := branch.Content.Text; got != branchSummaryPrefix+"branch"+branchSummarySuffix {
		t.Fatalf("branch summary = %q", got)
	}
	compact := getMessageFromEntry(SessionEntry{Type: "compaction", Summary: "compact", Timestamp: 2}).(ptypes.UserMessage)
	if got := compact.Content.Text; got != compactionSummaryPrefix+"compact"+compactionSummarySuffix {
		t.Fatalf("compaction summary = %q", got)
	}
	if EstimateTokens(compact) != (len(compactionSummaryPrefix+"compact"+compactionSummarySuffix)+3)/4 {
		t.Fatalf("estimate mismatch for wrapped compaction summary")
	}
}

func TestBuildSessionContextSkipsPriorCompactedEntries(t *testing.T) {
	entries := []SessionEntry{
		{ID: "old", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent(strings.Repeat("o", 400))}},
		{ID: "keep", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent("keep")}},
		{ID: "c1", Type: "compaction", Summary: "summary", FirstKeptEntryID: "keep"},
		{ID: "new", Type: "message", Message: ptypes.UserMessage{Content: ptypes.StringContent("new")}},
	}
	contextEntries := buildContextEntries(entries)
	if len(contextEntries) != 3 || contextEntries[0].ID != "c1" || contextEntries[1].ID != "keep" || contextEntries[2].ID != "new" {
		t.Fatalf("context entries = %+v", contextEntries)
	}
	prep := PrepareCompaction(entries, CompactionSettings{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 1})
	if prep == nil {
		t.Fatal("expected compaction prep")
	}
	inflated := EstimateContextTokens([]ptypes.Message{
		entries[0].Message,
		entries[1].Message,
		getMessageFromEntry(entries[2]),
		entries[3].Message,
	}).Tokens
	if prep.TokensBefore >= inflated {
		t.Fatalf("tokensBefore still inflated: got %d inflated %d", prep.TokensBefore, inflated)
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

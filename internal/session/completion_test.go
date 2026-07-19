package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

func TestPromiseStatusFromText(t *testing.T) {
	if got := PromiseStatusFromText("done <promise>WORKFLOW_COMPLETE</promise>"); got != PromiseWorkflowComplete {
		t.Fatalf("complete = %s", got)
	}
	if got := PromiseStatusFromText("blocked <promise>STUCK</promise>"); got != PromiseStuck {
		t.Fatalf("stuck = %s", got)
	}
	if got := PromiseStatusFromText("step <promise>STEP_COMPLETE</promise>"); got != PromiseStepComplete {
		t.Fatalf("step = %s", got)
	}
}

func TestCompletionLoopContinuesUntilWorkflowComplete(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.Text("step 1 <promise>STEP_COMPLETE</promise>")),
		faux.Respond(faux.Text("step 2 <promise>STEP_COMPLETE</promise>")),
		faux.Respond(faux.Text("done <promise>WORKFLOW_COMPLETE</promise>")),
	)
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: t.TempDir(), MaxContinuations: 5})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseWorkflowComplete || f.CallCount() != 3 {
		t.Fatalf("outcome = %+v calls=%d", outcome, f.CallCount())
	}
}

func TestCompletionLoopInitialPromptPreservesOrderedMediaRefs(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("done <promise>WORKFLOW_COMPLETE</promise>")))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	prompt := InitialPrompt{
		Text: "inspect these",
		Media: []document.MediaRef{
			{Filename: "preview.png", MediaType: "image/png", SizeBytes: 11, Blob: document.BlobRef{Store: document.StoreSessionLocal, Key: "image-key"}},
			{Filename: "report.pdf", MediaType: "application/pdf", SizeBytes: 22, PageCount: 3, Blob: document.BlobRef{Store: document.StoreSessionLocal, Key: "document-key"}},
		},
	}

	if _, err := RunCompletionLoop(context.Background(), agt, prompt, CompletionOptions{Enabled: false, ProjectDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	history := agt.State().Messages
	if len(history) < 1 {
		t.Fatal("initial user message missing from history")
	}
	userMessage, ok := history[0].(types.UserMessage)
	if !ok {
		t.Fatalf("initial message type = %T, want types.UserMessage", history[0])
	}
	blocks := userMessage.Content.Blocks
	if len(blocks) != 3 || blocks[0].Type != types.BlockText || blocks[0].Text != prompt.Text {
		t.Fatalf("initial blocks = %#v, want text followed by two refs", blocks)
	}
	if image := blocks[1]; image.Type != types.BlockImageRef || image.RefKey != "image-key" || image.RefFilename != "preview.png" || image.RefSizeBytes != 11 {
		t.Fatalf("image block = %#v", image)
	}
	if documentBlock := blocks[2]; documentBlock.Type != types.BlockDocumentRef || documentBlock.RefKey != "document-key" || documentBlock.RefFilename != "report.pdf" || documentBlock.RefSizeBytes != 22 || documentBlock.RefPageCount != 3 {
		t.Fatalf("document block = %#v", documentBlock)
	}
}

// writeTaskCompletedStep returns a faux ResponseStep that writes a TASK_COMPLETED
// sentinel into dir during the turn (as a subagent would) and then emits text.
// Only a flag written DURING the turn counts toward completion.
func writeTaskCompletedStep(dir, text string) faux.ResponseStep {
	return func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
		if err := os.WriteFile(filepath.Join(dir, "TASK_COMPLETED"), []byte("done"), 0o644); err != nil {
			return types.AssistantMessage{}, err
		}
		return faux.Respond(faux.Text(text))(c, opts, state, model)
	}
}

func TestCompletionLoopStopsAtTaskCompleted(t *testing.T) {
	dir := t.TempDir()
	f := faux.New(faux.Options{})
	f.SetResponses(writeTaskCompletedStep(dir, "ui finished"))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseTaskCompleted || !outcome.CompletedByFile || f.CallCount() != 1 {
		t.Fatalf("outcome = %+v calls=%d", outcome, f.CallCount())
	}
	if _, err := os.Stat(filepath.Join(dir, "TASK_COMPLETED")); !os.IsNotExist(err) {
		t.Fatalf("TASK_COMPLETED should be removed after detection, err=%v", err)
	}
}

func TestCompletionLoopStopsAtContinuationCap(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.Text("not done")),
		faux.Respond(faux.Text("still not done")),
		faux.Respond(faux.Text("still not done again")),
	)
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: t.TempDir(), MaxContinuations: 2})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseMaxContinuations || outcome.ContinuationsSent != 2 || f.CallCount() != 3 {
		t.Fatalf("outcome = %+v calls=%d", outcome, f.CallCount())
	}
}

func TestCompletionLoopCustomContinuePromptStillCaps(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.Text("not done")),
		faux.Respond(faux.Text("still not done")),
		faux.Respond(faux.Text("still not done again")),
	)
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: t.TempDir(), MaxContinuations: 2, ContinuePrompt: "keep going"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseMaxContinuations || outcome.ContinuationsSent != 2 || f.CallCount() != 3 {
		t.Fatalf("outcome = %+v calls=%d", outcome, f.CallCount())
	}
}

func TestCompletionLoopStopsAtStuck(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("blocked <promise>STUCK</promise>")))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseStuck || f.CallCount() != 1 {
		t.Fatalf("outcome = %+v calls=%d", outcome, f.CallCount())
	}
}

func TestCompletionLoopEmptyFirstTurnReturnsEarly(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("")))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseNone || f.CallCount() != 1 {
		t.Fatalf("outcome = %+v calls=%d", outcome, f.CallCount())
	}
}

func TestCompletionLoopRemovesStaleTaskCompletedForNextRun(t *testing.T) {
	dir := t.TempDir()
	// A flag left on disk BEFORE the run (committed by an older build or a prior
	// turn) is stale: RunCompletionLoop pre-clears it before prompting, so it must
	// not be miscounted as this turn's completion.
	if err := os.WriteFile(filepath.Join(dir, "TASK_COMPLETED"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("not done")), faux.Respond(faux.Text("still not done")))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: dir, MaxContinuations: 1})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status == PromiseTaskCompleted {
		t.Fatalf("stale TASK_COMPLETED should have been pre-cleared, not counted: %+v", outcome)
	}
	if _, err := os.Stat(filepath.Join(dir, "TASK_COMPLETED")); !os.IsNotExist(err) {
		t.Fatalf("stale TASK_COMPLETED should no longer exist on disk, err=%v", err)
	}
}

func TestCompletionLoopWorkflowCompleteRemovesFlag(t *testing.T) {
	dir := t.TempDir()
	f := faux.New(faux.Options{})
	// The agent writes TASK_COMPLETED during the turn AND signals WORKFLOW_COMPLETE.
	// Regression: the sentinel must be removed before returning, or finalize's
	// `git add -A` commits it and it short-circuits the next turn.
	f.SetResponses(writeTaskCompletedStep(dir, "all done <promise>WORKFLOW_COMPLETE</promise>"))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseWorkflowComplete || f.CallCount() != 1 {
		t.Fatalf("outcome = %+v calls=%d", outcome, f.CallCount())
	}
	if _, err := os.Stat(filepath.Join(dir, "TASK_COMPLETED")); !os.IsNotExist(err) {
		t.Fatalf("TASK_COMPLETED must be removed on WORKFLOW_COMPLETE, err=%v", err)
	}
}

func TestCompletionLoopStuckRemovesFlagNotCompleted(t *testing.T) {
	dir := t.TempDir()
	f := faux.New(faux.Options{})
	// STUCK cleans up the sentinel but is NOT a completion: CompletedByFile stays
	// false so the server does not finalize a stuck turn.
	f.SetResponses(writeTaskCompletedStep(dir, "blocked <promise>STUCK</promise>"))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseStuck || outcome.CompletedByFile {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, err := os.Stat(filepath.Join(dir, "TASK_COMPLETED")); !os.IsNotExist(err) {
		t.Fatalf("TASK_COMPLETED must be removed on STUCK, err=%v", err)
	}
}

func TestCompletionLoopDisabledForPlanMode(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("plan only")))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: false, ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseNone || f.CallCount() != 1 {
		t.Fatalf("outcome = %+v calls=%d", outcome, f.CallCount())
	}
}

func TestCompletionLoopStopsOnTerminalError(t *testing.T) {
	f := faux.New(faux.Options{})
	// Only ONE scripted response; without the terminal-error guard the loop
	// would re-prompt to the cap, hitting queue depletion (another StopError)
	// each time — a doomed retry storm. The guard must stop after one call.
	f.SetResponses(faux.RespondMessage(types.AssistantMessage{
		Content:      []types.ContentBlock{faux.Text("partial output")},
		StopReason:   types.StopError,
		ErrorMessage: "invalid x-api-key",
	}))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: t.TempDir(), MaxContinuations: 5})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != PromiseNone || outcome.ContinuationsSent != 0 {
		t.Fatalf("outcome = %+v, want PromiseNone with 0 continuations", outcome)
	}
	if f.CallCount() != 1 {
		t.Fatalf("provider called %d times, want exactly 1 (no retry storm on error)", f.CallCount())
	}
}

func TestCompletionLoopTerminalErrorBeatsStalePromise(t *testing.T) {
	f := faux.New(faux.Options{})
	// A truncated error transcript may still contain a promise tag; the terminal
	// StopError must win so finalize never commits a failed turn.
	f.SetResponses(faux.RespondMessage(types.AssistantMessage{
		Content:      []types.ContentBlock{faux.Text("done <promise>WORKFLOW_COMPLETE</promise>")},
		StopReason:   types.StopError,
		ErrorMessage: "boom",
	}))
	agt := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model()}, StreamFn: f.StreamSimple})
	outcome, err := RunCompletionLoop(context.Background(), agt, InitialPrompt{Text: "start"}, CompletionOptions{Enabled: true, ProjectDir: t.TempDir(), MaxContinuations: 5})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status == PromiseWorkflowComplete || outcome.CompletedByFile {
		t.Fatalf("terminal error treated as completion: %+v", outcome)
	}
	if outcome.Status != PromiseNone {
		t.Fatalf("outcome = %+v, want PromiseNone", outcome)
	}
}

// TestConsumeTaskCompleted pins the contract that consumeTaskCompleted reports
// true only when the TASK_COMPLETED flag was found AND successfully removed.
// A flag that survives the call (because removal failed) must report false so a
// lingering sentinel is not miscounted as this turn's completion.
func TestConsumeTaskCompleted(t *testing.T) {
	t.Run("found and removed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "TASK_COMPLETED")
		if err := os.WriteFile(path, []byte("done"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !consumeTaskCompleted(dir) {
			t.Fatal("want true when the flag is present and removable")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("flag must be removed after a successful consume, err=%v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if consumeTaskCompleted(t.TempDir()) {
			t.Fatal("want false when no flag exists")
		}
	})

	t.Run("removal fails", func(t *testing.T) {
		// os.Stat succeeds but os.Remove fails: a non-empty directory named
		// TASK_COMPLETED cannot be unlinked (ENOTEMPTY on POSIX, also fails on
		// Windows), so the function must report false and leave it in place.
		dir := t.TempDir()
		path := filepath.Join(dir, "TASK_COMPLETED")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "child"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if consumeTaskCompleted(dir) {
			t.Fatal("want false when the flag is found but removal fails")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("flag must remain on disk when removal fails, err=%v", err)
		}
	})
}

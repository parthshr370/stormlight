// Package session wires the agent's tool registry, prompt rebuilding, hook
// installation, and the completion/continuation loop that drives the build
// flow.
//
// # Completion loop
//
// The completion loop runs the agent for one turn, then repeatedly continues
// until a termination signal is found:
//   - The assistant emits <promise>WORKFLOW_COMPLETE</promise> in its text.
//   - The assistant emits <promise>STUCK</promise> (stop immediately, no continue).
//   - A TASK_COMPLETED file appears in the project directory (written by a
//     subagent; consumed and removed here to prevent stale short-circuits).
//   - The agent produces no assistant content in the initial turn.
//   - The configured [CompletionOptions.MaxContinuations] cap is reached.
//
// The local [continuationAttempt] counter prevents a custom
// [CompletionOptions.ContinuePrompt] that doesn't start with "continue" from
// defeating the cap.
//
// Plan mode (when [CompletionOptions.Enabled] is false) skips the loop:
// the agent gets exactly one turn and whatever it produced is the result.
package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
)

// PromiseStatus is the termination signal extracted from the assistant's text.
type PromiseStatus string

const (
	PromiseNone             PromiseStatus = ""
	PromiseWorkflowComplete PromiseStatus = "WORKFLOW_COMPLETE"
	PromiseStepComplete     PromiseStatus = "STEP_COMPLETE"
	PromiseStuck            PromiseStatus = "STUCK"
	PromiseMaxContinuations PromiseStatus = "MAX_CONTINUATIONS"
	PromiseTaskCompleted    PromiseStatus = "TASK_COMPLETED"
)

// CompletionOptions controls the continuation loop behavior.
type CompletionOptions struct {
	// Enabled controls whether the loop runs. When false (plan mode), the
	// agent gets exactly one turn and the result reflects that output.
	Enabled bool
	// ProjectDir is the working directory checked for the TASK_COMPLETED file.
	ProjectDir string
	// MaxContinuations caps the number of injected continue prompts
	// (default 5 via [defaultMaxContinuations]).
	MaxContinuations int
	// ContinuePrompt is the text sent to the agent on each continuation.
	// When empty, defaults to [defaultContinuePrompt] which requests the
	// WORKFLOW_COMPLETE promise tag.
	ContinuePrompt string
}

// CompletionOutcome summarizes the result of a completion-loop run.
type CompletionOutcome struct {
	Status            PromiseStatus
	ContinuationsSent int
	CompletedByFile   bool
	Text              string
	Reason            string
}

const defaultMaxContinuations = 5

const defaultContinuePrompt = "continue\n\n" +
	"CRITICAL: When the task is fully complete you MUST output the EXACT tag below " +
	"as a standalone line - no paraphrasing, no alternatives:\n\n" +
	"<promise>WORKFLOW_COMPLETE</promise>\n\n" +
	"Without this tag the system will keep prompting you to continue."

// RunCompletionLoop prompts the agent once, then runs [ContinueUntilComplete].
func RunCompletionLoop(ctx context.Context, agt *agent.Agent, prompt InitialPrompt, opts CompletionOptions) (CompletionOutcome, error) {
	if agt == nil {
		return CompletionOutcome{}, errors.New("completion loop agent is nil")
	}
	// Clear any pre-existing TASK_COMPLETED before prompting: a flag left on
	// disk by a prior turn (or committed by an older build) is stale and must
	// not be miscounted as this turn's completion. Only a flag written during
	// this turn counts (consumed in ContinueUntilComplete).
	_ = consumeTaskCompleted(opts.ProjectDir)
	if len(prompt.Media) == 0 {
		if err := agt.PromptText(ctx, prompt.Text); err != nil {
			return CompletionOutcome{}, err
		}
	} else if err := agt.Prompt(ctx, initialUserMessage(prompt)); err != nil {
		return CompletionOutcome{}, err
	}
	return ContinueUntilComplete(ctx, agt, opts)
}

// ContinueUntilComplete repeatedly continues the agent until a termination
// signal is found or the continuation cap is reached. See [CompletionOptions]
// for the termination signals.
//
// The local [continuationAttempt] counter ensures a custom
// [CompletionOptions.ContinuePrompt] cannot defeat the cap. Each re-prompt
// increments the counter.
//
// When a TASK_COMPLETED file is detected, it is removed (os.Remove) to prevent
// a stale file from short-circuiting later runs in the same project directory.
func ContinueUntilComplete(ctx context.Context, agt *agent.Agent, opts CompletionOptions) (CompletionOutcome, error) {
	if agt == nil {
		return CompletionOutcome{}, errors.New("completion loop agent is nil")
	}
	if !opts.Enabled {
		text := AssistantTranscriptText(agt.State().Messages)
		return CompletionOutcome{Status: PromiseStatusFromText(text), Text: text}, nil
	}
	max := opts.MaxContinuations
	if max <= 0 {
		max = defaultMaxContinuations
	}
	continuePrompt := opts.ContinuePrompt
	if strings.TrimSpace(continuePrompt) == "" {
		continuePrompt = defaultContinuePrompt
	}
	continuationAttempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return CompletionOutcome{}, err
		}
		state := agt.State()
		text := AssistantTranscriptText(state.Messages)
		status := PromiseStatusFromText(text)
		// Consume the flag once per turn before any terminal return so it is never
		// left on disk: finalize's `git add -A` would commit the sentinel, and a
		// leftover short-circuits the next turn. STUCK removal is cleanup-only and
		// must not set CompletedByFile, or server.go would finalize a stuck turn.
		fileCompleted := consumeTaskCompleted(opts.ProjectDir)
		// A terminal StopError/StopAborted means this turn failed or was
		// cancelled. Stop immediately: re-prompting would repeat a doomed
		// provider call (e.g. a 401) up to the cap and delay the error, and a
		// stale <promise> tag in a truncated error transcript must not be read
		// as completion — so this is checked before the WORKFLOW_COMPLETE switch.
		// The flag is already consumed above, so nothing leaks to disk.
		if lastAssistantErrored(state.Messages) {
			return CompletionOutcome{Status: PromiseNone, ContinuationsSent: continuationAttempt, Text: text, Reason: "assistant ended with a terminal error"}, nil
		}
		switch status {
		case PromiseWorkflowComplete:
			return CompletionOutcome{Status: status, ContinuationsSent: 0, CompletedByFile: fileCompleted, Text: text}, nil
		case PromiseStuck:
			return CompletionOutcome{Status: status, Text: text, Reason: "agent reported STUCK"}, nil
		}
		if fileCompleted {
			return CompletionOutcome{Status: PromiseTaskCompleted, ContinuationsSent: continuationAttempt, CompletedByFile: true, Text: text}, nil
		}
		if strings.TrimSpace(text) == "" {
			return CompletionOutcome{Status: PromiseNone, ContinuationsSent: continuationAttempt, Text: text, Reason: "initial turn produced no assistant content"}, nil
		}
		if continuationAttempt >= max {
			return CompletionOutcome{Status: PromiseMaxContinuations, ContinuationsSent: continuationAttempt, Text: text, Reason: "workflow did not signal completion before continuation cap"}, nil
		}
		if err := agt.PromptText(ctx, continuePrompt); err != nil {
			return CompletionOutcome{}, err
		}
		continuationAttempt++
	}
}

// PromiseStatusFromText scans the assistant text for <promise> tags and returns
// the strongest match (WORKFLOW_COMPLETE > STUCK > STEP_COMPLETE).
func PromiseStatusFromText(text string) PromiseStatus {
	switch {
	case strings.Contains(text, "<promise>WORKFLOW_COMPLETE</promise>"):
		return PromiseWorkflowComplete
	case strings.Contains(text, "<promise>STUCK</promise>"):
		return PromiseStuck
	case strings.Contains(text, "<promise>STEP_COMPLETE</promise>"):
		return PromiseStepComplete
	default:
		return PromiseNone
	}
}

// AssistantTranscriptText concatenates all assistant messages' text content.
func AssistantTranscriptText(messages []ptypes.Message) string {
	parts := []string{}
	for _, message := range messages {
		if text := AssistantMessageText(message); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// LastAssistantText returns the text content of the most recent assistant
// message, scanning backward from the end of the conversation.
func LastAssistantText(messages []ptypes.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if text := AssistantMessageText(messages[i]); text != "" {
			return text
		}
	}
	return ""
}

// AssistantMessageText extracts the concatenated text from an
// [ptypes.Message]'s content blocks.
func AssistantMessageText(message ptypes.Message) string {
	var msg ptypes.AssistantMessage
	switch value := message.(type) {
	case ptypes.AssistantMessage:
		msg = value
	case *ptypes.AssistantMessage:
		if value == nil {
			return ""
		}
		msg = *value
	default:
		return ""
	}
	parts := []string{}
	for _, block := range msg.Content {
		if block.Type == ptypes.BlockText {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// lastAssistantErrored reports whether the most recent assistant message ended
// with a terminal StopError or StopAborted. Such a turn failed or was cancelled,
// so the completion loop must stop rather than re-prompt: re-prompting repeats a
// doomed provider call MaxContinuations times and delays the error surfacing.
func lastAssistantErrored(messages []ptypes.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		switch msg := messages[i].(type) {
		case ptypes.AssistantMessage:
			return msg.StopReason == ptypes.StopError || msg.StopReason == ptypes.StopAborted
		case *ptypes.AssistantMessage:
			if msg == nil {
				continue
			}
			return msg.StopReason == ptypes.StopError || msg.StopReason == ptypes.StopAborted
		}
	}
	return false
}

// consumeTaskCompleted reports whether a TASK_COMPLETED flag was found AND
// removed in projectDir. We only report completion when the removal succeeds:
// a flag we cannot delete would otherwise be miscounted as this turn's
// completion and then linger to short-circuit the next turn too.
func consumeTaskCompleted(projectDir string) bool {
	if strings.TrimSpace(projectDir) == "" {
		projectDir = "."
	}
	path := filepath.Join(projectDir, "TASK_COMPLETED")
	if _, err := os.Stat(path); err != nil {
		return false
	}
	if err := os.Remove(path); err != nil {
		return false
	}
	return true
}

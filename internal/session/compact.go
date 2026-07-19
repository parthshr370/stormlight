package session

import (
	"context"
	"strconv"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	ptypes "go.harness.dev/harness/internal/engine/types"
)

// compactionSummaryTimeout bounds a single summarization turn so a stalled
// provider call cannot wedge the build loop between turns.
const compactionSummaryTimeout = 120 * time.Second

// compactionRunner summarizes older turns when the estimated context approaches
// the model's window. It is wired into the agent loop's between-turn hook so a
// long, tool-heavy build never silently overflows the context window.
type compactionRunner struct {
	contextWindow int
	model         ptypes.Model
	streamFn      agent.StreamFn
	settings      compaction.CompactionSettings
}

// newCompactionRunner builds a runner for the given model and provider seam. It
// returns nil when compaction cannot run (no stream, or no usable context
// window to budget against), so the caller wires no hook.
func newCompactionRunner(model ptypes.Model, streamFn agent.StreamFn) *compactionRunner {
	if streamFn == nil || model.ContextWindow <= 0 {
		return nil
	}
	return &compactionRunner{
		contextWindow: model.ContextWindow,
		model:         model,
		streamFn:      streamFn,
		settings:      compaction.DefaultCompactionSettings,
	}
}

// hook returns a next-turn callback that compacts between turns. commit, when
// non-nil, persists the compacted history to the agent so a later Prompt/
// Continue starts from the summary rather than the full transcript; subagents
// pass nil because their child agent runs a single Prompt.
func (r *compactionRunner) hook(commit func([]ptypes.Message)) func(agent.ShouldStopAfterTurnContext) *agent.AgentLoopTurnUpdate {
	if r == nil {
		return nil
	}
	return func(c agent.ShouldStopAfterTurnContext) *agent.AgentLoopTurnUpdate {
		compacted, ok := r.maybeCompact(c.Context.Messages)
		if !ok {
			return nil
		}
		if commit != nil {
			commit(compacted)
		}
		next := c.Context
		next.Messages = compacted
		return &agent.AgentLoopTurnUpdate{Context: &next}
	}
}

// maybeCompact summarizes older turns when the estimate exceeds the window minus
// the reserve. It returns the compacted messages and true on success, or the
// input and false when compaction does not run: under budget, nothing to
// summarize, or the summarizer failed. On failure the loop keeps the original
// history rather than replaying an over-budget request blindly.
func (r *compactionRunner) maybeCompact(messages []ptypes.Message) ([]ptypes.Message, bool) {
	if r == nil || len(messages) == 0 {
		return messages, false
	}
	tokens := compaction.EstimateContextTokens(messages).Tokens
	if !compaction.ShouldCompact(tokens, r.contextWindow, r.settings) {
		return messages, false
	}
	entries := messagesToEntries(messages)
	prep := compaction.PrepareCompaction(entries, r.settings)
	if prep == nil || (len(prep.MessagesToSummarize) == 0 && len(prep.TurnPrefixMessages) == 0) {
		return messages, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), compactionSummaryTimeout)
	defer cancel()
	summary, err := compaction.Compact(ctx, r.streamFn, r.model, prep)
	if err != nil {
		return messages, false
	}
	return compaction.ApplyCompaction(entries, prep, summary), true
}

// messagesToEntries bridges the loop's flat message slice into the SessionEntry
// list the compaction engine reasons over. Every entry carries a non-empty ID
// (PrepareCompaction rejects empty IDs) and type "message" so role-based cut
// points resolve.
func messagesToEntries(messages []ptypes.Message) []compaction.SessionEntry {
	entries := make([]compaction.SessionEntry, 0, len(messages))
	for i, m := range messages {
		if m == nil {
			continue
		}
		entries = append(entries, compaction.SessionEntry{ID: strconv.Itoa(i + 1), Type: "message", Message: m})
	}
	return entries
}

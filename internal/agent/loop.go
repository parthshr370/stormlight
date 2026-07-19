// Package agent implements the owned agent loop. It runs turns (prompt →
// assistant stream → tool calls → results → next turn) and surfaces every step
// as an [AgentEvent] on a stream.
//
// The loop is self-owned (no framework owns it) and concurrency-native:
// streaming is a channel, parallel tool dispatch is goroutines + errgroup,
// and cancellation is [context.Context] threaded everywhere. See
// internal/agent/loop.go for the turn loop,
// internal/agent/executor.go for tool dispatch, and internal/agent/runtime.go
// for the stateful [Agent] wrapper.
package agent

import (
	"context"
	"errors"
	"log/slog"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

// createAgentStream builds the event stream for one agent run. The stream's
// terminal predicate is EventAgentEnd (so agent_end both closes Events() and
// resolves Result with the final message list); any other event flows through.
func createAgentStream() *stream.Stream[AgentEvent, []types.Message] {
	return stream.New[AgentEvent, []types.Message](64,
		func(ev AgentEvent) bool { return ev.Type == EventAgentEnd },
		func(ev AgentEvent) []types.Message {
			if ev.Type == EventAgentEnd {
				return ev.Messages
			}
			return nil
		},
	)
}

// AgentLoop runs a fresh agent run from prompts, returning a stream of events
// that terminates with an agent_end carrying the new messages. It is the
// primary entry point; the loop body runs on a goroutine and the caller ranges
// over Events() / awaits Result.
func AgentLoop(ctx context.Context, prompts []types.Message, agentContext AgentContext, config AgentLoopConfig, streamFn StreamFn) *stream.Stream[AgentEvent, []types.Message] {
	s := createAgentStream()
	emit := func(ctx context.Context, ev AgentEvent) error {
		s.Push(ev)
		return nil
	}

	go func() {
		messages, _ := runAgentLoop(ctx, prompts, agentContext, config, emit, streamFn)
		// EndWith, not End, resolves the stream with the message list (so
		// Result() returns it). A plain End would resolve to ErrNoResult.
		s.EndWith(messages)
	}()

	return s
}

// AgentLoopContinue resumes a run from an existing transcript (the last message
// must be a user/toolResult — never an assistant message, since the loop would
// immediately try to stream another assistant response from nothing). Returns
// an error up front when the resume precondition is violated.
func AgentLoopContinue(ctx context.Context, agentContext AgentContext, config AgentLoopConfig, streamFn StreamFn) (*stream.Stream[AgentEvent, []types.Message], error) {
	if len(agentContext.Messages) == 0 {
		return nil, errors.New("Cannot continue: no messages in context")
	}
	if agentContext.Messages[len(agentContext.Messages)-1].Role() == "assistant" {
		return nil, errors.New("Cannot continue from message role: assistant")
	}

	s := createAgentStream()
	emit := func(ctx context.Context, ev AgentEvent) error {
		s.Push(ev)
		return nil
	}

	go func() {
		messages, _ := runAgentLoopContinue(ctx, agentContext, config, emit, streamFn)
		s.EndWith(messages)
	}()

	return s, nil
}

// runAgentLoop is the synchronous core behind [AgentLoop]: emits agent_start,
// seeds the prompt messages, then enters [runLoop].
func runAgentLoop(ctx context.Context, prompts []types.Message, context AgentContext, config AgentLoopConfig, emit AgentEventSink, streamFn StreamFn) ([]types.Message, error) {
	newMessages := append([]types.Message{}, prompts...)
	currentContext := context
	currentContext.Messages = append(append([]types.Message{}, context.Messages...), prompts...)

	if err := emit(ctx, AgentStart()); err != nil {
		return newMessages, err
	}
	if err := emit(ctx, TurnStart()); err != nil {
		return newMessages, err
	}
	for _, prompt := range prompts {
		if err := emit(ctx, MessageStart(prompt)); err != nil {
			return newMessages, err
		}
		if err := emit(ctx, MessageEnd(prompt)); err != nil {
			return newMessages, err
		}
	}

	return runLoop(ctx, currentContext, newMessages, config, emit, streamFn)
}

// runAgentLoopContinue is the synchronous core behind [AgentLoopContinue].
func runAgentLoopContinue(ctx context.Context, context AgentContext, config AgentLoopConfig, emit AgentEventSink, streamFn StreamFn) ([]types.Message, error) {
	if len(context.Messages) == 0 {
		return nil, errors.New("Cannot continue: no messages in context")
	}
	if context.Messages[len(context.Messages)-1].Role() == "assistant" {
		return nil, errors.New("Cannot continue from message role: assistant")
	}

	newMessages := []types.Message{}
	currentContext := context

	if err := emit(ctx, AgentStart()); err != nil {
		return newMessages, err
	}
	if err := emit(ctx, TurnStart()); err != nil {
		return newMessages, err
	}

	return runLoop(ctx, currentContext, newMessages, config, emit, streamFn)
}

// runLoop is the core outer/inner turn loop.
//
// Outer loop: turns grouped by tool-batch termination; an inner turn streams an
// assistant response, executes its tool calls, appends results, then either
// continues (more tool calls expected) or hands control to follow-up messages.
// Steering messages are drained at the top of each inner iteration; follow-ups
// at the bottom of each outer iteration. PrepareNextTurn can swap the context/
// model/thinking between turns; ShouldStopAfterTurn can short-circuit the run.
//
// An error/aborted stop reason on the assistant message terminates immediately
// (no tool execution) so a truncated stream doesn't leave dangling tool calls.
func runLoop(ctx context.Context, currentContext AgentContext, newMessages []types.Message, config AgentLoopConfig, emit AgentEventSink, streamFn StreamFn) ([]types.Message, error) {
	turn := 0
	firstTurn := true
	pendingMessages := []types.Message(nil)
	if config.GetSteeringMessages != nil {
		pendingMessages = config.GetSteeringMessages()
	}

	for {
		hasMoreToolCalls := true

		for hasMoreToolCalls || len(pendingMessages) > 0 {
			turn++
			slog.Info("agent turn", "turn", turn)
			if !firstTurn {
				if err := emit(ctx, TurnStart()); err != nil {
					return newMessages, err
				}
			} else {
				firstTurn = false
			}

			if len(pendingMessages) > 0 {
				for _, message := range pendingMessages {
					if err := emit(ctx, MessageStart(message)); err != nil {
						return newMessages, err
					}
					if err := emit(ctx, MessageEnd(message)); err != nil {
						return newMessages, err
					}
					currentContext.Messages = append(currentContext.Messages, message)
					newMessages = append(newMessages, message)
				}
				pendingMessages = nil
			}

			message, err := streamAssistantResponse(ctx, &currentContext, config, emit, streamFn)
			if err != nil {
				return newMessages, err
			}
			newMessages = append(newMessages, message)

			if message.StopReason == types.StopError || message.StopReason == types.StopAborted {
				// Truncated/aborted stream: end the turn AND the run without
				// attempting tool execution — the assistant message may be
				// incomplete and its tool calls would be half-formed.
				if err := emit(ctx, TurnEnd(message, []types.ToolResultMessage{})); err != nil {
					return newMessages, err
				}
				if err := emit(ctx, AgentEnd(newMessages)); err != nil {
					return newMessages, err
				}
				return newMessages, nil
			}

			toolCalls := []types.ContentBlock{}
			for _, content := range message.Content {
				if content.Type == types.BlockToolCall {
					toolCalls = append(toolCalls, content)
				}
			}

			toolResults := []types.ToolResultMessage{}
			hasMoreToolCalls = false
			if len(toolCalls) > 0 {
				executedToolBatch, err := executeToolCalls(ctx, currentContext, message, config, emit)
				if err != nil {
					return newMessages, err
				}
				toolResults = append(toolResults, executedToolBatch.Messages...)
				// A batch terminates the turn only when EVERY result sets
				// Terminate — a single non-terminating result means the model
				// expects another round.
				hasMoreToolCalls = !executedToolBatch.Terminate

				for _, result := range toolResults {
					currentContext.Messages = append(currentContext.Messages, result)
					newMessages = append(newMessages, result)
				}
			}

			if err := emit(ctx, TurnEnd(message, toolResults)); err != nil {
				return newMessages, err
			}

			if config.PrepareNextTurn != nil {
				nextTurnSnapshot := config.PrepareNextTurn(ShouldStopAfterTurnContext{
					Message:     message,
					ToolResults: toolResults,
					Context:     currentContext,
					NewMessages: newMessages,
				})
				if nextTurnSnapshot != nil {
					if nextTurnSnapshot.Context != nil {
						currentContext = *nextTurnSnapshot.Context
					}
					if nextTurnSnapshot.Model != nil {
						config.Model = *nextTurnSnapshot.Model
					}
					// ThinkingOff explicitly clears reasoning (not the same as
					// leaving it unset, which would preserve the prior level).
					if nextTurnSnapshot.ThinkingLevel == ThinkingOff {
						config.Reasoning = ""
					} else if nextTurnSnapshot.ThinkingLevel != "" {
						config.Reasoning = string(nextTurnSnapshot.ThinkingLevel)
					}
				}
			}

			if config.ShouldStopAfterTurn != nil && config.ShouldStopAfterTurn(ShouldStopAfterTurnContext{
				Message:     message,
				ToolResults: toolResults,
				Context:     currentContext,
				NewMessages: newMessages,
			}) {
				if err := emit(ctx, AgentEnd(newMessages)); err != nil {
					return newMessages, err
				}
				return newMessages, nil
			}

			pendingMessages = nil
			if config.GetSteeringMessages != nil {
				pendingMessages = config.GetSteeringMessages()
			}
		}

		followUpMessages := []types.Message(nil)
		if config.GetFollowUpMessages != nil {
			followUpMessages = config.GetFollowUpMessages()
		}
		if len(followUpMessages) > 0 {
			pendingMessages = followUpMessages
			continue
		}

		break
	}

	if err := emit(ctx, AgentEnd(newMessages)); err != nil {
		return newMessages, err
	}
	return newMessages, nil
}

// snapshotAssistant returns a copy of m whose Content slice (and each tool-call's
// Arguments bytes) is independent of the source — used to decouple an emitted
// streaming partial from the AssistantBuilder's live, still-mutating buffer.
func snapshotAssistant(m types.AssistantMessage) types.AssistantMessage {
	if len(m.Content) == 0 {
		return m
	}
	blocks := make([]types.ContentBlock, len(m.Content))
	for i, b := range m.Content {
		if len(b.Arguments) > 0 {
			// Copy the Arguments backing array: the builder keeps appending
			// tool-call JSON deltas to its own slice on this goroutine, while
			// the emitted partial flows to listeners on OTHER goroutines.
			// Without this copy the shared backing array is a data race
			// (A10 verifier finding).
			b.Arguments = append(b.Arguments[:0:0], b.Arguments...)
		}
		blocks[i] = b
	}
	m.Content = blocks
	return m
}

// streamAssistantResponse drives one provider stream: transforms the context,
// resolves the API key, calls streamFn, folds each delta into a partial
// [AssistantMessage], and emits message_start/update/end events. The partial
// is appended to the context messages on start and overwritten on each update
// so a downstream turn sees the live, growing message.
func streamAssistantResponse(ctx context.Context, cctx *AgentContext, config AgentLoopConfig, emit AgentEventSink, streamFn StreamFn) (types.AssistantMessage, error) {
	messages := cctx.Messages
	if config.TransformContext != nil {
		messages = config.TransformContext(ctx, messages)
	}

	llmMessages := config.ConvertToLlm(messages)
	tools := make([]types.Tool, 0, len(cctx.Tools))
	for _, tool := range cctx.Tools {
		tools = append(tools, tool.Tool)
	}
	llmContext := types.Context{
		SystemPrompt: cctx.SystemPrompt,
		Messages:     llmMessages,
		Tools:        tools,
	}

	resolvedAPIKey := config.APIKey
	if config.GetAPIKey != nil {
		if key := config.GetAPIKey(config.Model.Provider); key != "" {
			resolvedAPIKey = key
		}
	}

	options := config.SimpleStreamOptions
	options.APIKey = resolvedAPIKey
	response := streamFn(ctx, config.Model, llmContext, &options)

	builder := types.NewAssistantBuilder(config.Model.API, config.Model.Provider, config.Model.ID)
	addedPartial := false
	logEnd := func(m types.AssistantMessage) {
		if m.StopReason == types.StopError {
			slog.Error("assistant stream error", "model", m.Model, "provider", m.Provider, "errorMessage", redactTruncate(m.ErrorMessage, 400), "errorCode", m.ErrorCode)
			return
		}
		slog.Info("assistant stream end", "model", m.Model, "provider", m.Provider, "stopReason", string(m.StopReason), "inputTokens", m.Usage.Input, "outputTokens", m.Usage.Output, "cacheReadTokens", m.Usage.CacheRead)
		slog.Debug("assistant text", "model", m.Model, "preview", logPreview(m.Content, 400))
	}

	for event := range response.Events() {
		builder.Fold(event)
		// Snapshot: builder.Message() shares the builder's live Content buffer, which
		// Fold keeps mutating on this goroutine. Emitted partials flow to Agent.State()
		// / listeners on OTHER goroutines, so hand out an independent copy to avoid a
		// data race on the shared backing array (A10 verifier finding).
		partialMessage := snapshotAssistant(builder.Message())

		switch event.Type {
		case types.EvStart:
			slog.Debug("assistant stream start", "model", config.Model.ID, "provider", config.Model.Provider)
			cctx.Messages = append(cctx.Messages, partialMessage)
			addedPartial = true
			if err := emit(ctx, MessageStart(partialMessage)); err != nil {
				return partialMessage, err
			}

		case types.EvTextStart, types.EvTextDelta, types.EvTextEnd,
			types.EvThinkingStart, types.EvThinkingDelta, types.EvThinkingEnd,
			types.EvToolCallStart, types.EvToolCallDelta, types.EvToolCallEnd:
			if addedPartial {
				// Overwrite the slot we added on EvStart so the context's
				// last message is always the current partial.
				cctx.Messages[len(cctx.Messages)-1] = partialMessage
				eventCopy := event
				if err := emit(ctx, MessageUpdate(partialMessage, &eventCopy)); err != nil {
					return partialMessage, err
				}
			}

		case types.EvDone, types.EvError:
			// Use background ctx for Result: we want the terminal message even
			// if runCtx was canceled (e.g. by /cancel) — the stream's own
			// settlement still resolves. Reading Result under a canceled ctx
			// would race the canceled run and lose the terminal metadata.
			finalMessage := assistantResult(context.Background(), response, config)
			if addedPartial {
				cctx.Messages[len(cctx.Messages)-1] = finalMessage
			} else {
				cctx.Messages = append(cctx.Messages, finalMessage)
			}
			if !addedPartial {
				// Stream produced no EvStart (some providers skip it): emit a
				// start now so listeners see the full start→end pairing.
				if err := emit(ctx, MessageStart(finalMessage)); err != nil {
					return finalMessage, err
				}
			}
			logEnd(finalMessage)
			if err := emit(ctx, MessageEnd(finalMessage)); err != nil {
				return finalMessage, err
			}
			return finalMessage, nil
		}
	}

	// Stream ended without an explicit EvDone/EvError (e.g. context cancel
	// mid-fold). Synthesize the final message from Result.
	finalMessage := assistantResult(ctx, response, config)
	if addedPartial {
		cctx.Messages[len(cctx.Messages)-1] = finalMessage
	} else {
		cctx.Messages = append(cctx.Messages, finalMessage)
		if err := emit(ctx, MessageStart(finalMessage)); err != nil {
			return finalMessage, err
		}
	}
	logEnd(finalMessage)
	if err := emit(ctx, MessageEnd(finalMessage)); err != nil {
		return finalMessage, err
	}
	return finalMessage, nil
}

// assistantResult extracts the final AssistantMessage from a completed stream.
// On error/abort it synthesizes a message with the right StopReason so the
// loop's truncated-abort branch fires and the run ends cleanly.
func assistantResult(ctx context.Context, response *stream.AssistantStream, config AgentLoopConfig) types.AssistantMessage {
	finalMessage, err := response.Result(ctx)
	if err == nil && finalMessage != nil {
		return *finalMessage
	}
	message := types.AssistantMessage{
		API:          config.Model.API,
		Provider:     config.Model.Provider,
		Model:        config.Model.ID,
		StopReason:   types.StopError,
		ErrorMessage: "assistant stream ended without a final message",
	}
	if err != nil {
		message.ErrorMessage = err.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Distinguish "user canceled" from "stream broke": the loop treats
			// StopAborted as a non-error termination path.
			message.StopReason = types.StopAborted
		}
	}
	return message
}

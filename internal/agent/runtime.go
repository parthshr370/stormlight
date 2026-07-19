package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

// PendingMessageQueue is the queued steering/follow-up buffer. The Agent mutex
// guards all calls.
//
// Drain returns a copy of the queued messages so the caller owns the slice
// independently of future Enqueue/Clear calls. QueueAll drains the whole queue;
// one-at-a-time hands out only the head, preserving single-message pacing.
type PendingMessageQueue struct {
	messages []types.Message
	Mode     QueueMode
}

// Enqueue appends a message to the queue.
func (q *PendingMessageQueue) Enqueue(m types.Message) {
	q.messages = append(q.messages, m)
}

// HasItems reports whether the queue is non-empty.
func (q *PendingMessageQueue) HasItems() bool {
	return len(q.messages) > 0
}

// Drain removes messages from the head. QueueAll drains everything and resets;
// one-at-a-time returns only the first message and shifts the rest forward.
// Copies the slice on drain so the returned messages are independent of future
// mutations — critical when the loop holds a reference across turns.
func (q *PendingMessageQueue) Drain() []types.Message {
	if q.Mode == QueueAll {
		drained := copyAgentMessages(q.messages)
		q.messages = nil
		return drained
	}

	if len(q.messages) == 0 {
		return nil
	}
	first := q.messages[0]
	if first == nil {
		return nil
	}
	q.messages = append([]types.Message(nil), q.messages[1:]...)
	return []types.Message{first}
}

// Clear empties the queue without returning anything.
func (q *PendingMessageQueue) Clear() {
	q.messages = nil
}

// MutableAgentState is AgentState with runtime-owned fields mutable under Agent.mu.
type MutableAgentState struct {
	SystemPrompt     string
	Model            types.Model
	ThinkingLevel    ThinkingLevel
	Tools            []AgentTool
	Messages         []types.Message
	IsStreaming      bool
	StreamingMessage types.Message
	PendingToolCalls map[string]struct{}
	ErrorMessage     string
}

// AgentOptions supplies the runtime configuration with provider-transport fields deferred.
type AgentOptions struct {
	// InitialState seeds the agent's state; nil gives defaults.
	InitialState *AgentState
	// ConvertToLlm filters/reshapes messages before the provider call.
	// When nil, defaults to passing through user/assistant/toolResult roles.
	ConvertToLlm func([]types.Message) []types.Message
	// TransformContext is called before every provider request (e.g. for compaction).
	TransformContext func(ctx context.Context, msgs []types.Message) []types.Message
	// StreamFn is the provider entry point. Required for Prompt/Continue.
	StreamFn StreamFn
	// GetApiKey resolves a provider-specific API key; overrides the ambient key.
	GetApiKey      func(provider string) string
	BeforeToolCall func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult
	AfterToolCall  func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult
	// PrepareNextTurn is called after each turn for a stateless config swap.
	PrepareNextTurn func() *AgentLoopTurnUpdate
	// PrepareNextTurnWithContext is the stateful variant — preferred when set;
	// it receives turn context and can inspect the last message + tool results.
	PrepareNextTurnWithContext func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate
	// SteeringMode controls how queued steering messages are drained:
	// QueueAll drains everything, QueueOneAtATime hands out one per turn.
	SteeringMode QueueMode
	// FollowUpMode controls follow-up message drain behavior. Defaults to
	// QueueOneAtATime.
	FollowUpMode  QueueMode
	SessionID     string
	ToolExecution ToolExecutionMode
}

// activeRun represents one in-flight agent run. done closes when the run
// settles (finishRun); cancel aborts it (Abort). Both always set or always nil
// under Agent.mu.
type activeRun struct {
	done   chan struct{}
	cancel context.CancelFunc
}

// agentListener is a subscriber to agent events. id is a monotonic identifier
// used to remove the listener later (Go closures aren't reliably comparable,
// so a funcval key would be unsafe).
type agentListener struct {
	id uint64
	fn func(ctx context.Context, ev AgentEvent) error
}

// Agent is the stateful wrapper around the low-level loop. It implements the
// public runtime API: Prompt (start a new turn), Continue (resume from a
// transcript), Abort (cancel the in-flight turn), Reset (clear conversation),
// and State (snapshot public observables).
//
// Concurrency: mu guards ALL mutable state (state, queues, activeRun,
// listeners, eventErr). An active run is enforced by a nil check under mu
// (single-run guard — a second Prompt/Continue while active returns a busy
// error). Listener fan-out happens OUTSIDE mu (processEvents copies the
// listener slice under mu, then iterates without holding the lock) so a slow
// listener cannot block state mutations. Abort cancels the run context without
// needing mu (the cancel func is immutable after startRunLocked).
type Agent struct {
	mu            sync.Mutex
	state         MutableAgentState
	listeners     []agentListener
	nextListener  uint64
	steeringQueue PendingMessageQueue
	followUpQueue PendingMessageQueue
	activeRun     *activeRun
	eventErr      error

	ConvertToLlm               func([]types.Message) []types.Message
	TransformContext           func(ctx context.Context, msgs []types.Message) []types.Message
	StreamFn                   StreamFn
	GetApiKey                  func(provider string) string
	BeforeToolCall             func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult
	AfterToolCall              func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult
	PrepareNextTurn            func() *AgentLoopTurnUpdate
	PrepareNextTurnWithContext func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate
	SessionID                  string
	ToolExecution              ToolExecutionMode
}

var defaultModel = types.Model{
	ID:       "unknown",
	Name:     "unknown",
	API:      "unknown",
	Provider: "unknown",
	BaseURL:  "",
	Input:    []string{},
}

// defaultConvertToLlm is the fallback ConvertToLlm: passes through user,
// assistant, and toolResult messages; drops system and nil entries.
func defaultConvertToLlm(messages []types.Message) []types.Message {
	converted := make([]types.Message, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		switch message.Role() {
		case "user", "assistant", "toolResult":
			converted = append(converted, message)
		}
	}
	return converted
}

// createMutableAgentState seeds the internal state from an optional public
// snapshot. Slices are copied into fresh backing arrays so the Agent's state
// is never backed by caller-owned memory.
func createMutableAgentState(initialState *AgentState) MutableAgentState {
	state := MutableAgentState{
		Model:            defaultModel,
		ThinkingLevel:    ThinkingOff,
		PendingToolCalls: map[string]struct{}{},
	}
	if initialState == nil {
		return state
	}
	state.SystemPrompt = initialState.SystemPrompt
	if !isZeroModel(initialState.Model) {
		state.Model = initialState.Model
	}
	if initialState.ThinkingLevel != "" {
		state.ThinkingLevel = initialState.ThinkingLevel
	}
	state.Tools = copyAgentTools(initialState.Tools)
	state.Messages = copyAgentMessages(initialState.Messages)
	return state
}

// NewAgent creates an Agent with the given options, filling defaults for nil
// callbacks and empty modes. The Agent is idle on return; call Prompt or
// Continue to start a turn.
func NewAgent(opts AgentOptions) *Agent {
	convertToLlm := opts.ConvertToLlm
	if convertToLlm == nil {
		convertToLlm = defaultConvertToLlm
	}
	steeringMode := opts.SteeringMode
	if steeringMode == "" {
		steeringMode = QueueOneAtATime
	}
	followUpMode := opts.FollowUpMode
	if followUpMode == "" {
		followUpMode = QueueOneAtATime
	}
	toolExecution := opts.ToolExecution
	if toolExecution == "" {
		toolExecution = ExecParallel
	}

	return &Agent{
		state:                      createMutableAgentState(opts.InitialState),
		ConvertToLlm:               convertToLlm,
		TransformContext:           opts.TransformContext,
		StreamFn:                   opts.StreamFn,
		GetApiKey:                  opts.GetApiKey,
		BeforeToolCall:             opts.BeforeToolCall,
		AfterToolCall:              opts.AfterToolCall,
		PrepareNextTurn:            opts.PrepareNextTurn,
		PrepareNextTurnWithContext: opts.PrepareNextTurnWithContext,
		steeringQueue:              PendingMessageQueue{Mode: steeringMode},
		followUpQueue:              PendingMessageQueue{Mode: followUpMode},
		SessionID:                  opts.SessionID,
		ToolExecution:              toolExecution,
	}
}

// Subscribe registers a listener that receives every AgentEvent in order.
// It returns an unsubscribe function. Listener errors stop fan-out immediately
// (subsequent listeners are NOT called) and are surfaced to the run.
//
// Subscribe uses a monotonically assigned identifier because Go closures aren't
// reliably comparable (captureless literals share one funcval), so every
// Subscribe gets a fresh identifier with no dedup. Two independently-created
// listeners both fire.
func (a *Agent) Subscribe(listener func(ctx context.Context, ev AgentEvent) error) (unsubscribe func()) {
	// Go closures aren't reliably comparable (captureless literals share one
	// funcval), so every Subscribe registers a distinct listener under a fresh
	// identifier — no dedup and no unsafe funcval-pointer key.
	a.mu.Lock()
	a.nextListener++
	id := a.nextListener
	a.listeners = append(a.listeners, agentListener{id: id, fn: listener})
	a.mu.Unlock()

	return func() { a.removeListener(id) }
}

// SetSteeringMode sets the steering queue drain mode.
func (a *Agent) SetSteeringMode(mode QueueMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.Mode = mode
}

// SteeringMode returns the steering queue drain mode.
func (a *Agent) SteeringMode() QueueMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.steeringQueue.Mode
}

// SetFollowUpMode sets the follow-up queue drain mode.
func (a *Agent) SetFollowUpMode(mode QueueMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue.Mode = mode
}

// FollowUpMode returns the follow-up queue drain mode.
func (a *Agent) FollowUpMode() QueueMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.followUpQueue.Mode
}

// Steer enqueues a steering message for the current (or next) run.
func (a *Agent) Steer(m types.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.Enqueue(m)
}

// FollowUp enqueues a follow-up message — consumed between outer-loop
// iterations rather than inner ones.
func (a *Agent) FollowUp(m types.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue.Enqueue(m)
}

// ClearSteeringQueue empties the steering queue.
func (a *Agent) ClearSteeringQueue() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.Clear()
}

// ClearFollowUpQueue empties the follow-up queue.
func (a *Agent) ClearFollowUpQueue() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue.Clear()
}

// ClearAllQueues empties both queues.
func (a *Agent) ClearAllQueues() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.Clear()
	a.followUpQueue.Clear()
}

// HasQueuedMessages reports whether either queue has pending messages.
func (a *Agent) HasQueuedMessages() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.steeringQueue.HasItems() || a.followUpQueue.HasItems()
}

// Abort cancels the in-flight run by calling its context's CancelFunc. Safe
// to call without holding mu (the cancel func is immutable after being set).
func (a *Agent) Abort() {
	a.mu.Lock()
	active := a.activeRun
	a.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

// WaitForIdle blocks until the current run finishes (or returns immediately
// if idle).
func (a *Agent) WaitForIdle() {
	a.mu.Lock()
	var done chan struct{}
	if a.activeRun != nil {
		done = a.activeRun.done
	}
	a.mu.Unlock()
	if done != nil {
		<-done
	}
}

// Reset clears the conversation state, queues, streaming flags, and error.
// It does NOT abort an in-flight run — call [Agent.Abort] + [Agent.WaitForIdle]
// before Reset if a turn is in progress.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = nil
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]struct{}{}
	a.state.ErrorMessage = ""
	a.steeringQueue.Clear()
	a.followUpQueue.Clear()
}

// State returns a snapshot of the public agent state. Slices are copied so
// the caller owns the returned values independently.
func (a *Agent) State() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AgentState{
		SystemPrompt:     a.state.SystemPrompt,
		Model:            a.state.Model,
		ThinkingLevel:    a.state.ThinkingLevel,
		Tools:            copyAgentTools(a.state.Tools),
		Messages:         copyAgentMessages(a.state.Messages),
		IsStreaming:      a.state.IsStreaming,
		StreamingMessage: a.state.StreamingMessage,
		PendingToolCalls: copyStringSet(a.state.PendingToolCalls),
		ErrorMessage:     a.state.ErrorMessage,
	}
}

// SetModel updates the model for the next turn.
func (a *Agent) SetModel(model types.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Model = model
}

// SetTools replaces the active tool set.
func (a *Agent) SetTools(tools []AgentTool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Tools = copyAgentTools(tools)
}

// SetMessages replaces the conversation history.
func (a *Agent) SetMessages(messages []types.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = copyAgentMessages(messages)
}

// SetSystemPrompt replaces the system prompt.
func (a *Agent) SetSystemPrompt(systemPrompt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.SystemPrompt = systemPrompt
}

// SetThinkingLevel sets the reasoning level for future turns.
func (a *Agent) SetThinkingLevel(level ThinkingLevel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.ThinkingLevel = level
}

// Prompt starts a new run with the given messages as the prompt. Returns
// immediately if the agent is already processing (uses the provided busy
// error). Blocks the calling goroutine until the run settles.
func (a *Agent) Prompt(ctx context.Context, messages ...types.Message) error {
	return a.runPromptMessages(ctx, messages, false, errors.New("Agent is already processing a prompt. Use steer() or followUp() to queue messages, or wait for completion."))
}

// PromptText is a convenience wrapper that wraps a text string in a user
// message and calls [Agent.Prompt]. Optional image blocks are appended to
// the content.
func (a *Agent) PromptText(ctx context.Context, text string, images ...types.ContentBlock) error {
	content := []types.ContentBlock{types.NewText(text)}
	if len(images) > 0 {
		content = append(content, images...)
	}
	return a.Prompt(ctx, types.UserMessage{Content: types.BlockContent(content...), Timestamp: time.Now().UnixMilli()})
}

// Continue resumes from the current transcript. If the last message is an
// assistant message, it checks for queued steering/follow-up messages and
// feeds them as new turns; otherwise it runs [AgentLoopContinue]. Returns
// an error immediately if a run is already in progress.
func (a *Agent) Continue(ctx context.Context) error {
	a.mu.Lock()
	if a.activeRun != nil {
		a.mu.Unlock()
		return errors.New("Agent is already processing. Wait for completion before continuing.")
	}
	if len(a.state.Messages) == 0 {
		a.mu.Unlock()
		return errors.New("No messages to continue from")
	}
	lastMessage := a.state.Messages[len(a.state.Messages)-1]
	if lastMessage.Role() == "assistant" {
		queuedSteering := a.steeringQueue.Drain()
		if len(queuedSteering) > 0 {
			runCtx := a.startRunLocked(ctx)
			a.mu.Unlock()
			return a.runStarted(runCtx, func(runCtx context.Context) error {
				if a.StreamFn == nil {
					return errors.New("Agent streamFn is nil")
				}
				_, err := runAgentLoop(runCtx, queuedSteering, a.createContextSnapshot(), a.createLoopConfig(true), a.processEvents, a.StreamFn)
				if err != nil {
					return err
				}
				return a.takeEventErr()
			})
		}

		queuedFollowUps := a.followUpQueue.Drain()
		if len(queuedFollowUps) > 0 {
			runCtx := a.startRunLocked(ctx)
			a.mu.Unlock()
			return a.runStarted(runCtx, func(runCtx context.Context) error {
				if a.StreamFn == nil {
					return errors.New("Agent streamFn is nil")
				}
				_, err := runAgentLoop(runCtx, queuedFollowUps, a.createContextSnapshot(), a.createLoopConfig(false), a.processEvents, a.StreamFn)
				if err != nil {
					return err
				}
				return a.takeEventErr()
			})
		}

		a.mu.Unlock()
		return errors.New("Cannot continue from message role: assistant")
	}
	a.mu.Unlock()

	return a.runContinuation(ctx)
}

// runPromptMessages is the implementation behind Prompt: acquires the run slot
// via runWithLifecycle, then runs the low-level loop. skipInitialSteeringPoll
// suppresses the first steering-message poll so the initial prompt doesn't
// consume queued steering messages before the run begins.
func (a *Agent) runPromptMessages(ctx context.Context, messages []types.Message, skipInitialSteeringPoll bool, busyErr error) error {
	return a.runWithLifecycle(ctx, busyErr, func(runCtx context.Context) error {
		if a.StreamFn == nil {
			return errors.New("Agent streamFn is nil")
		}
		_, err := runAgentLoop(runCtx, messages, a.createContextSnapshot(), a.createLoopConfig(skipInitialSteeringPoll), a.processEvents, a.StreamFn)
		if err != nil {
			return err
		}
		return a.takeEventErr()
	})
}

// runContinuation runs [AgentLoopContinue] under the lifecycle wrapper.
func (a *Agent) runContinuation(ctx context.Context) error {
	return a.runWithLifecycle(ctx, errors.New("Agent is already processing. Wait for completion before continuing."), func(runCtx context.Context) error {
		if a.StreamFn == nil {
			return errors.New("Agent streamFn is nil")
		}
		_, err := runAgentLoopContinue(runCtx, a.createContextSnapshot(), a.createLoopConfig(false), a.processEvents, a.StreamFn)
		if err != nil {
			return err
		}
		return a.takeEventErr()
	})
}

// createContextSnapshot captures a copy of the current state for the loop.
// Called under Agent.mu.
func (a *Agent) createContextSnapshot() AgentContext {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AgentContext{
		SystemPrompt: a.state.SystemPrompt,
		Messages:     copyAgentMessages(a.state.Messages),
		Tools:        copyAgentTools(a.state.Tools),
	}
}

// createLoopConfig builds the [AgentLoopConfig] for one run. It reads the
// current state values under mu, then constructs closures that drain the
// steering/follow-up queues on each turn.
//
// skipInitialSteeringPoll suppresses the first GetSteeringMessages call so
// the initial prompt isn't accompanied by queued steering. The suppression
// is a one-shot bool captured by the GetSteeringMessages closure — it flips
// to false after the first drain.
func (a *Agent) createLoopConfig(skipInitialSteeringPoll bool) AgentLoopConfig {
	a.mu.Lock()
	model := a.state.Model
	thinkingLevel := a.state.ThinkingLevel
	toolExecution := a.ToolExecution
	convertToLlm := a.ConvertToLlm
	transformContext := a.TransformContext
	getAPIKey := a.GetApiKey
	beforeToolCall := a.BeforeToolCall
	afterToolCall := a.AfterToolCall
	prepareNextTurn := a.PrepareNextTurn
	prepareNextTurnWithContext := a.PrepareNextTurnWithContext
	sessionID := a.SessionID
	a.mu.Unlock()

	if convertToLlm == nil {
		convertToLlm = defaultConvertToLlm
	}
	if toolExecution == "" {
		toolExecution = ExecParallel
	}

	config := AgentLoopConfig{
		Model:            model,
		ToolExecution:    toolExecution,
		BeforeToolCall:   beforeToolCall,
		AfterToolCall:    afterToolCall,
		ConvertToLlm:     convertToLlm,
		TransformContext: transformContext,
		GetAPIKey:        getAPIKey,
	}
	if thinkingLevel != "" && thinkingLevel != ThinkingOff {
		config.Reasoning = string(thinkingLevel)
	}
	config.SessionID = sessionID
	if prepareNextTurnWithContext != nil || prepareNextTurn != nil {
		config.PrepareNextTurn = func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate {
			if prepareNextTurnWithContext != nil {
				return prepareNextTurnWithContext(c)
			}
			return prepareNextTurn()
		}
	}
	config.GetSteeringMessages = func() []types.Message {
		a.mu.Lock()
		defer a.mu.Unlock()
		if skipInitialSteeringPoll {
			skipInitialSteeringPoll = false
			return nil
		}
		return a.steeringQueue.Drain()
	}
	config.GetFollowUpMessages = func() []types.Message {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.followUpQueue.Drain()
	}
	return config
}

// runWithLifecycle enforces the single-run guard, starts a run, and returns
// its result. busyErr is returned immediately when a run is already active
// (different callers use different busy messages). The executor runs under a
// derived context so Abort cancels it without canceling the caller's context.
func (a *Agent) runWithLifecycle(ctx context.Context, busyErr error, executor func(context.Context) error) error {
	a.mu.Lock()
	if a.activeRun != nil {
		a.mu.Unlock()
		if busyErr != nil {
			return busyErr
		}
		return errors.New("Agent is already processing.")
	}
	runCtx := a.startRunLocked(ctx)
	a.mu.Unlock()

	return a.runStarted(runCtx, executor)
}

// startRunLocked creates a fresh run context and marks the agent as active.
// Caller holds mu.
func (a *Agent) startRunLocked(ctx context.Context) context.Context {
	runCtx, cancel := context.WithCancel(ctx)
	a.activeRun = &activeRun{done: make(chan struct{}), cancel: cancel}
	a.eventErr = nil
	a.state.IsStreaming = true
	a.state.StreamingMessage = nil
	a.state.ErrorMessage = ""
	return runCtx
}

// runStarted runs the executor, recovers panics, and defers finishRun.
// finishRun always runs (even after a panic) so the run's done channel closes
// and WaitForIdle unblocks.
func (a *Agent) runStarted(runCtx context.Context, executor func(context.Context) error) error {
	defer a.finishRun()

	var execErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				execErr = errors.New(recoveredMessage(recovered))
			}
		}()
		execErr = executor(runCtx)
	}()
	if execErr != nil {
		if err := a.handleRunFailure(runCtx, execErr, runCtx.Err() != nil); err != nil {
			return err
		}
	}
	return nil
}

// handleRunFailure synthesizes failure events so listeners see a complete
// start→end cycle even when the loop itself threw. Without this, a loop panic
// would leave listeners waiting for events that never come.
func (a *Agent) handleRunFailure(ctx context.Context, err error, aborted bool) error {
	a.mu.Lock()
	model := a.state.Model
	a.mu.Unlock()

	stopReason := types.StopError
	if aborted {
		stopReason = types.StopAborted
	}
	failureMessage := types.AssistantMessage{
		Content:      []types.ContentBlock{types.NewText("")},
		API:          model.API,
		Provider:     model.Provider,
		Model:        model.ID,
		Usage:        types.Usage{},
		StopReason:   stopReason,
		ErrorMessage: err.Error(),
		Timestamp:    time.Now().UnixMilli(),
	}
	if eventErr := a.processEvents(ctx, MessageStart(failureMessage)); eventErr != nil {
		return eventErr
	}
	if eventErr := a.processEvents(ctx, MessageEnd(failureMessage)); eventErr != nil {
		return eventErr
	}
	if eventErr := a.processEvents(ctx, TurnEnd(failureMessage, []types.ToolResultMessage{})); eventErr != nil {
		return eventErr
	}
	return a.processEvents(ctx, AgentEnd([]types.Message{failureMessage}))
}

// finishRun marks the run as settled. IMPORTANT: close(done) happens before
// cancel() — WaitForIdle reads done under mu then unlocks, so a racing
// WaitForIdle must see the closed channel before cancel kills in-flight work.
// The cancel() at the end is a safety net; the run context is usually already
// done by this point unless the run leaked a goroutine.
func (a *Agent) finishRun() {
	a.mu.Lock()
	active := a.activeRun
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]struct{}{}
	if active != nil {
		close(active.done)
		a.activeRun = nil
	}
	a.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

// processEvents is the single event sink wired into the loop config.
// It mutates the streaming/pending state under mu, then fans out to
// listeners OUTSIDE mu so a slow listener can't block state mutations.
//
// State mutations that need to happen:
//   - message_start/update: set StreamingMessage so State() shows it live
//   - message_end: commit the message to history, clear StreamingMessage
//   - tool_execution_start/end: track PendingToolCalls (in-flight tool set)
//   - turn_end: capture error message from assistant messages
//   - agent_end: clear StreamingMessage
func (a *Agent) processEvents(ctx context.Context, ev AgentEvent) error {
	a.mu.Lock()
	switch ev.Type {
	case EventMessageStart, EventMessageUpdate:
		a.state.StreamingMessage = ev.Message
	case EventMessageEnd:
		a.state.StreamingMessage = nil
		a.state.Messages = append(copyAgentMessages(a.state.Messages), ev.Message)
	case EventToolExecutionStart:
		pendingToolCalls := copyStringSet(a.state.PendingToolCalls)
		pendingToolCalls[ev.ToolCallID] = struct{}{}
		a.state.PendingToolCalls = pendingToolCalls
	case EventToolExecutionEnd:
		pendingToolCalls := copyStringSet(a.state.PendingToolCalls)
		delete(pendingToolCalls, ev.ToolCallID)
		a.state.PendingToolCalls = pendingToolCalls
	case EventTurnEnd:
		if errorMessage, ok := assistantErrorMessage(ev.Message); ok {
			a.state.ErrorMessage = errorMessage
		}
	case EventAgentEnd:
		a.state.StreamingMessage = nil
	}

	listeners := append([]agentListener(nil), a.listeners...)
	if a.activeRun == nil {
		a.mu.Unlock()
		return errors.New("Agent listener invoked outside active run")
	}
	a.mu.Unlock()

	for _, listener := range listeners {
		if err := listener.fn(ctx, ev); err != nil {
			a.setEventErr(err)
			return err
		}
	}
	return nil
}

// setEventErr records the first listener error so the run can surface it
// after all tool calls complete (via takeEventErr).
func (a *Agent) setEventErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.eventErr == nil {
		a.eventErr = err
	}
}

// takeEventErr returns and clears the stored listener error.
func (a *Agent) takeEventErr() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.eventErr
	a.eventErr = nil
	return err
}

// isActive reports whether a run is in progress (nil-safe).
func (a *Agent) isActive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activeRun != nil
}

// assistantErrorMessage extracts an error message from an assistant message.
// It handles direct [types.AssistantMessage], pointer, and structs that
// implement the role interface with a reflect fallback.
func assistantErrorMessage(message types.Message) (string, bool) {
	if message == nil || message.Role() != "assistant" {
		return "", false
	}
	switch value := message.(type) {
	case types.AssistantMessage:
		if value.ErrorMessage != "" {
			return value.ErrorMessage, true
		}
	case *types.AssistantMessage:
		if value != nil && value.ErrorMessage != "" {
			return value.ErrorMessage, true
		}
	}
	reflected := reflect.ValueOf(message)
	if reflected.Kind() == reflect.Pointer {
		if reflected.IsNil() {
			return "", false
		}
		reflected = reflected.Elem()
	}
	if reflected.Kind() == reflect.Struct {
		field := reflected.FieldByName("ErrorMessage")
		if field.IsValid() && field.Kind() == reflect.String && field.String() != "" {
			return field.String(), true
		}
	}
	return "", false
}

// removeListener unsubscribes a listener by its monotonic id.
func (a *Agent) removeListener(id uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, current := range a.listeners {
		if current.id == id {
			a.listeners = append(a.listeners[:i], a.listeners[i+1:]...)
			return
		}
	}
}

// isZeroModel reports whether m is the empty value (all fields zero/nil).
func isZeroModel(model types.Model) bool {
	return model.ID == "" && model.Name == "" && model.API == "" && model.Provider == "" && model.BaseURL == "" && !model.Reasoning && len(model.ThinkingLevelMap) == 0 && len(model.Input) == 0 && model.Cost == (types.ModelCost{}) && model.ContextWindow == 0 && model.MaxTokens == 0 && len(model.Headers) == 0 && model.Compat == nil
}

// copyAgentTools returns a shallow copy of tools (the backing array is fresh,
// the elements are shared).
func copyAgentTools(tools []AgentTool) []AgentTool {
	if tools == nil {
		return nil
	}
	return append([]AgentTool(nil), tools...)
}

// copyAgentMessages returns a shallow copy of messages.
func copyAgentMessages(messages []types.Message) []types.Message {
	if messages == nil {
		return nil
	}
	return append([]types.Message(nil), messages...)
}

// copyStringSet returns a shallow copy of a mapset.
func copyStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for key := range in {
		out[key] = struct{}{}
	}
	return out
}

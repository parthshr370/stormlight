package agent

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

// StreamFn is the loop's provider entry. It never throws: failures are encoded
// in the returned stream as error/aborted terminal events.
type StreamFn func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream

// ToolExecutionMode configures how tool calls from one assistant message run.
type ToolExecutionMode string

const (
	ExecSequential ToolExecutionMode = "sequential"
	ExecParallel   ToolExecutionMode = "parallel"
)

// QueueMode controls how many queued messages are injected at drain points.
type QueueMode string

const (
	QueueAll        QueueMode = "all"
	QueueOneAtATime QueueMode = "one-at-a-time"
)

// ThinkingLevel is the model reasoning level requested for future turns.
type ThinkingLevel string

const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
)

// AgentToolResult is the final or partial result produced by a tool.
type AgentToolResult struct {
	Content   []types.ContentBlock
	Details   json.RawMessage
	Terminate bool
}

// AgentToolUpdateCallback streams partial tool results while Execute runs.
type AgentToolUpdateCallback func(AgentToolResult)

// AgentTool is a runtime tool definition used by the agent loop.
type AgentTool struct {
	types.Tool
	Label            string
	PrepareArguments func(args json.RawMessage) json.RawMessage
	ExecutionMode    ToolExecutionMode
	Execute          func(ctx context.Context, toolCallID string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error)
}

// AgentContext is the context snapshot passed into the low-level agent loop.
type AgentContext struct {
	SystemPrompt string
	Messages     []types.Message
	Tools        []AgentTool
}

// AgentState is the public observable agent state. Runtime code copies slices on
// assignment; this type is only the plain snapshot shape.
type AgentState struct {
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

// BeforeToolCallResult is returned by BeforeToolCall.
type BeforeToolCallResult struct {
	Block  bool
	Reason string
}

// AfterToolCallResult is a field-by-field override for a completed tool result.
// Nil pointer fields and nil Content mean "omitted; keep the original".
type AfterToolCallResult struct {
	Content    []types.ContentBlock
	Details    json.RawMessage
	HasDetails bool
	IsError    *bool
	Terminate  *bool
}

// BeforeToolCallContext is passed to BeforeToolCall after argument validation.
type BeforeToolCallContext struct {
	AssistantMessage types.AssistantMessage
	ToolCall         types.ContentBlock
	Args             any
	Context          AgentContext
}

// AfterToolCallContext is passed to AfterToolCall before result events are emitted.
type AfterToolCallContext struct {
	AssistantMessage types.AssistantMessage
	ToolCall         types.ContentBlock
	Args             any
	Result           AgentToolResult
	IsError          bool
	Context          AgentContext
}

// ShouldStopAfterTurnContext is passed to ShouldStopAfterTurn.
type ShouldStopAfterTurnContext struct {
	Message     types.AssistantMessage
	ToolResults []types.ToolResultMessage
	Context     AgentContext
	NewMessages []types.Message
}

// AgentLoopTurnUpdate replaces selected state before the next provider request.
type AgentLoopTurnUpdate struct {
	Context       *AgentContext
	Model         *types.Model
	ThinkingLevel ThinkingLevel
}

// AgentLoopConfig configures the low-level loop. Hooks return plain values; the
// loop edge recovers panics.
type AgentLoopConfig struct {
	types.SimpleStreamOptions
	Model         types.Model
	ToolExecution ToolExecutionMode

	ConvertToLlm        func(messages []types.Message) []types.Message
	TransformContext    func(ctx context.Context, messages []types.Message) []types.Message
	GetAPIKey           func(provider string) string
	ShouldStopAfterTurn func(c ShouldStopAfterTurnContext) bool
	PrepareNextTurn     func(c ShouldStopAfterTurnContext) *AgentLoopTurnUpdate
	GetSteeringMessages func() []types.Message
	GetFollowUpMessages func() []types.Message
	BeforeToolCall      func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult
	AfterToolCall       func(ctx context.Context, c AfterToolCallContext) *AfterToolCallResult
}

// AgentEventType is the discriminant for AgentEvent.
type AgentEventType string

const (
	EventAgentStart          AgentEventType = "agent_start"
	EventAgentEnd            AgentEventType = "agent_end"
	EventTurnStart           AgentEventType = "turn_start"
	EventTurnEnd             AgentEventType = "turn_end"
	EventMessageStart        AgentEventType = "message_start"
	EventMessageUpdate       AgentEventType = "message_update"
	EventMessageEnd          AgentEventType = "message_end"
	EventToolExecutionStart  AgentEventType = "tool_execution_start"
	EventToolExecutionUpdate AgentEventType = "tool_execution_update"
	EventToolExecutionEnd    AgentEventType = "tool_execution_end"
)

// AgentEvent is a tagged struct over the event union. Only fields relevant to
// Type are populated.
type AgentEvent struct {
	Type                  AgentEventType
	Message               types.Message
	Messages              []types.Message
	ToolResults           []types.ToolResultMessage
	AssistantMessageEvent *types.StreamEvent
	ToolCallID            string
	ToolName              string
	Args                  any
	Result                any
	PartialResult         any
	IsError               bool
}

// AgentStart constructs an agent_start event.
func AgentStart() AgentEvent { return AgentEvent{Type: EventAgentStart} }

// AgentEnd constructs an agent_end event.
func AgentEnd(messages []types.Message) AgentEvent {
	return AgentEvent{Type: EventAgentEnd, Messages: messages}
}

// TurnStart constructs a turn_start event.
func TurnStart() AgentEvent { return AgentEvent{Type: EventTurnStart} }

// TurnEnd constructs a turn_end event.
func TurnEnd(message types.Message, results []types.ToolResultMessage) AgentEvent {
	return AgentEvent{Type: EventTurnEnd, Message: message, ToolResults: results}
}

// MessageStart constructs a message_start event.
func MessageStart(message types.Message) AgentEvent {
	return AgentEvent{Type: EventMessageStart, Message: message}
}

// MessageUpdate constructs a message_update event.
func MessageUpdate(message types.Message, assistantEvent *types.StreamEvent) AgentEvent {
	return AgentEvent{Type: EventMessageUpdate, Message: message, AssistantMessageEvent: assistantEvent}
}

// MessageEnd constructs a message_end event.
func MessageEnd(message types.Message) AgentEvent {
	return AgentEvent{Type: EventMessageEnd, Message: message}
}

// ToolExecutionStart constructs a tool_execution_start event.
func ToolExecutionStart(id, name string, args any) AgentEvent {
	return AgentEvent{Type: EventToolExecutionStart, ToolCallID: id, ToolName: name, Args: args}
}

// ToolExecutionUpdate constructs a tool_execution_update event.
func ToolExecutionUpdate(id, name string, args, partial any) AgentEvent {
	return AgentEvent{Type: EventToolExecutionUpdate, ToolCallID: id, ToolName: name, Args: args, PartialResult: partial}
}

// ToolExecutionEnd constructs a tool_execution_end event.
func ToolExecutionEnd(id, name string, result any, isError bool) AgentEvent {
	return AgentEvent{Type: EventToolExecutionEnd, ToolCallID: id, ToolName: name, Result: result, IsError: isError}
}

// AgentEventSink consumes events in order; A9 awaits it serially for backpressure.
type AgentEventSink func(ctx context.Context, ev AgentEvent) error

package stream

import "go.harness.dev/harness/internal/engine/types"

const assistantStreamBuffer = 64

// AssistantStream is a stream of types.StreamEvent values whose terminal result
// is the final assistant message. It also keeps a types.AssistantBuilder so
// callers can ask for the fully folded message via [AssistantStream.Final].
type AssistantStream struct {
	*Stream[types.StreamEvent, *types.AssistantMessage]
	builder *types.AssistantBuilder
	final   *types.AssistantMessage
}

// NewAssistantStream constructs an AssistantStream and tags the folded message
// with provider provenance.
// AssistantStream wraps a [Stream] typed for assistant message events.
// Push shadows the base Push to fold each [types.StreamEvent] through an
// [types.AssistantBuilder] before sending it downstream, so callers see the
// accumulated message without needing their own fold logic.
func NewAssistantStream(api, provider, model string) *AssistantStream {
	builder := types.NewAssistantBuilder(api, provider, model)
	return &AssistantStream{
		Stream: New[types.StreamEvent, *types.AssistantMessage](
			assistantStreamBuffer,
			func(ev types.StreamEvent) bool { return ev.Type == types.EvDone || ev.Type == types.EvError },
			func(ev types.StreamEvent) *types.AssistantMessage {
				switch ev.Type {
				case types.EvDone:
					return ev.Message
				case types.EvError:
					return ev.Err
				default:
					return nil
				}
			},
		),
		builder: builder,
	}
}

// Push folds the event into the assistant builder, then forwards it to the
// underlying stream. Call this method, not the embedded Stream.Push, if you want
// [AssistantStream.Final] to be accurate.
func (a *AssistantStream) Push(ev types.StreamEvent) {
	a.builder.Fold(ev)
	if ev.Type == types.EvDone && ev.Message != nil {
		msg := *ev.Message
		a.final = &msg
	} else if ev.Type == types.EvError && ev.Err != nil {
		msg := *ev.Err
		a.final = &msg
	}
	a.Stream.Push(ev)
}

// Final returns the folded assistant message. Call it after Result has returned
// or Events has closed; reading the builder while Push is still folding events
// would race.
func (a *AssistantStream) Final() *types.AssistantMessage {
	if a.final != nil {
		msg := *a.final
		return &msg
	}
	msg := a.builder.Message()
	return &msg
}

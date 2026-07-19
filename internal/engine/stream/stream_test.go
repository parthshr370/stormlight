package stream

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

func TestStreamOrderAndResult(t *testing.T) {
	s := NewAssistantStream("anthropic-messages", "anthropic", "claude-x")
	doneMessage := &types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}}
	const n = 5

	collected := make(chan []types.StreamEvent, 1)
	go func() {
		var events []types.StreamEvent
		for ev := range s.Events() {
			events = append(events, ev)
		}
		collected <- events
	}()

	go func() {
		for i := 0; i < n; i++ {
			s.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: string(rune('a' + i))})
		}
		s.Push(types.StreamEvent{Type: types.EvDone, Message: doneMessage, Reason: types.StopStop})
	}()

	gotResult, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if gotResult != doneMessage {
		t.Fatalf("Result returned %p, want %p", gotResult, doneMessage)
	}

	events := <-collected
	if len(events) != n+1 {
		t.Fatalf("got %d events, want %d", len(events), n+1)
	}
	for i := 0; i < n; i++ {
		want := string(rune('a' + i))
		if events[i].Type != types.EvTextDelta || events[i].Delta != want {
			t.Fatalf("event %d = %+v, want text_delta %q", i, events[i], want)
		}
	}
	if events[n].Type != types.EvDone || events[n].Message != doneMessage {
		t.Fatalf("terminal event = %+v, want done with message", events[n])
	}
}

func TestStreamErrorTerminal(t *testing.T) {
	s := NewAssistantStream("", "", "")
	terminalErr := &types.AssistantMessage{ErrorMessage: "boom"}

	go func() {
		for range s.Events() {
		}
	}()
	s.Push(types.StreamEvent{Type: types.EvError, Err: terminalErr, Reason: types.StopError})

	got, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got != terminalErr {
		t.Fatalf("Result returned %p, want %p", got, terminalErr)
	}
}

func TestStreamEndWithoutResult(t *testing.T) {
	s := New[int, int](1, func(v int) bool { return v == 10 }, func(v int) int { return v })
	s.End()

	_, err := s.Result(context.Background())
	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("Result err = %v, want ErrNoResult", err)
	}
}

func TestStreamEndWithResult(t *testing.T) {
	s := New[int, int](1, func(v int) bool { return v == 10 }, func(v int) int { return v })
	s.EndWith(99) // resolves result() with no completing event

	got, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got != 99 {
		t.Fatalf("Result = %d, want 99", got)
	}
	if _, ok := <-s.Events(); ok {
		t.Fatalf("Events not closed after EndWith")
	}
}

func TestStreamResultDoesNotRequireEventConsumer(t *testing.T) {
	s := New[int, int](1, func(v int) bool { return v == 100 }, func(v int) int { return v })
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for i := 0; i <= 100; i++ {
			s.Push(i)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := s.Result(ctx)
	if err != nil {
		t.Fatalf("Result without event consumer: %v", err)
	}
	if got != 100 {
		t.Fatalf("Result = %d, want 100", got)
	}
	select {
	case <-producerDone:
	case <-time.After(time.Second):
		t.Fatal("producer blocked while no event consumer was draining")
	}
	for range s.Events() {
	}
}

func TestStreamResultContextCancel(t *testing.T) {
	s := New[int, int](1, func(v int) bool { return v == 10 }, func(v int) int { return v })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Result(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Result err = %v, want context.Canceled", err)
	}
}

func TestAssistantStreamFinal(t *testing.T) {
	s := NewAssistantStream("anthropic-messages", "anthropic", "claude-x")
	doneMessage := &types.AssistantMessage{
		Content:    []types.ContentBlock{types.NewText("terminal")},
		API:        "terminal-api",
		Provider:   "terminal-provider",
		Model:      "terminal-model",
		Usage:      types.Usage{Input: 7},
		StopReason: types.StopStop,
		Timestamp:  123,
	}

	s.Push(types.StreamEvent{Type: types.EvTextStart, ContentIndex: 0})
	s.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: "Hel"})
	s.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: "lo"})
	s.Push(types.StreamEvent{Type: types.EvTextEnd, ContentIndex: 0, Content: "Hello"})
	s.Push(types.StreamEvent{Type: types.EvDone, Message: doneMessage, Reason: types.StopStop})

	for range s.Events() {
	}

	gotResult, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if gotResult != doneMessage {
		t.Fatalf("Result returned %p, want %p", gotResult, doneMessage)
	}

	final := s.Final()
	if final.API != "terminal-api" || final.Provider != "terminal-provider" || final.Model != "terminal-model" || final.Usage.Input != 7 || final.Timestamp != 123 {
		t.Fatalf("Final provenance = %+v", final)
	}
	if final.StopReason != types.StopStop {
		t.Fatalf("Final stopReason = %q, want stop", final.StopReason)
	}
	if len(final.Content) != 1 || final.Content[0].Type != types.BlockText || final.Content[0].Text != "terminal" {
		t.Fatalf("Final content = %+v, want terminal message content", final.Content)
	}
}

func TestStreamConcurrentResultAndEvents(t *testing.T) {
	s := New[int, int](2, func(v int) bool { return v == 4 }, func(v int) int { return v * 10 })
	var wg sync.WaitGroup
	gotEvents := make(chan []int, 1)
	gotResult := make(chan int, 1)
	gotErr := make(chan error, 1)

	wg.Add(3)
	go func() {
		defer wg.Done()
		var events []int
		for ev := range s.Events() {
			events = append(events, ev)
		}
		gotEvents <- events
	}()
	go func() {
		defer wg.Done()
		result, err := s.Result(context.Background())
		gotResult <- result
		gotErr <- err
	}()
	go func() {
		defer wg.Done()
		for i := 1; i <= 4; i++ {
			s.Push(i)
		}
	}()

	wg.Wait()
	close(gotEvents)
	close(gotResult)
	close(gotErr)

	if err := <-gotErr; err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result := <-gotResult; result != 40 {
		t.Fatalf("Result = %d, want 40", result)
	}
	if events := <-gotEvents; !reflect.DeepEqual(events, []int{1, 2, 3, 4}) {
		t.Fatalf("events = %v, want [1 2 3 4]", events)
	}
}

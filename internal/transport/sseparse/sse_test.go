package sseparse

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeSseLine(t *testing.T) {
	state := decoderState{}
	if event := decodeSseLine(":comment", &state); event != nil {
		t.Fatalf("comment produced event: %#v", event)
	}
	decodeSseLine("event: message_start", &state)
	decodeSseLine("data: value", &state)
	decodeSseLine("data:no-space", &state)
	decodeSseLine("data:  one-leading-space-kept", &state)

	event := decodeSseLine("", &state)
	if event == nil {
		t.Fatalf("blank line did not flush event")
	}
	want := &ServerSentEvent{
		Event: "message_start",
		Data:  "value\nno-space\n one-leading-space-kept",
		Raw:   []string{":comment", "event: message_start", "data: value", "data:no-space", "data:  one-leading-space-kept"},
	}
	if !reflect.DeepEqual(event, want) {
		t.Fatalf("event = %#v, want %#v", event, want)
	}
	if trailing := flushSseEvent(&state); trailing != nil {
		t.Fatalf("state was not reset, trailing = %#v", trailing)
	}

	decodeSseLine("data", &state)
	noColon := decodeSseLine("", &state)
	if noColon == nil || noColon.Data != "" || !reflect.DeepEqual(noColon.Raw, []string{"data"}) {
		t.Fatalf("field without colon event = %#v, want empty data field", noColon)
	}
}

func TestLineHelpers(t *testing.T) {
	indexCases := []struct {
		text string
		want int
	}{
		{"a\nb", 1},
		{"a\rb", 1},
		{"a\r\nb", 1},
		{"abc", -1},
	}
	for _, tc := range indexCases {
		if got := nextLineBreakIndex(tc.text); got != tc.want {
			t.Fatalf("nextLineBreakIndex(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}

	consumeCases := []struct {
		text     string
		wantLine string
		wantRest string
		wantOK   bool
	}{
		{"a\nb", "a", "b", true},
		{"a\rb", "a", "b", true},
		{"a\r\nb", "a", "b", true},
		{"abc", "", "", false},
	}
	for _, tc := range consumeCases {
		line, rest, ok := consumeLine(tc.text)
		if line != tc.wantLine || rest != tc.wantRest || ok != tc.wantOK {
			t.Fatalf("consumeLine(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.text, line, rest, ok, tc.wantLine, tc.wantRest, tc.wantOK)
		}
	}
}

func TestIterateSSE(t *testing.T) {
	input := "event: one\ndata: hello 🙈\n\n" +
		"data: no-event\n\n" +
		"event: trailing\ndata: last"
	reader := &oneByteReader{data: []byte(input)}

	var events []ServerSentEvent
	err := IterateSSE(context.Background(), reader, func(event ServerSentEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("IterateSSE: %v", err)
	}

	want := []ServerSentEvent{
		{Event: "one", Data: "hello 🙈", Raw: []string{"event: one", "data: hello 🙈"}},
		{Event: "", Data: "no-event", Raw: []string{"data: no-event"}},
		{Event: "trailing", Data: "last", Raw: []string{"event: trailing", "data: last"}},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestIterateSSECallbackError(t *testing.T) {
	wantErr := errors.New("stop")
	err := IterateSSE(context.Background(), strings.NewReader("data: x\n\n"), func(ServerSentEvent) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("IterateSSE callback err = %v, want %v", err, wantErr)
	}
}

func TestIterateSSECancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := IterateSSE(ctx, strings.NewReader("data: x\n\n"), func(ServerSentEvent) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("IterateSSE canceled err = %v, want context.Canceled", err)
	}
}

type oneByteReader struct {
	data []byte
	pos  int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

// Package sseparse decodes Server-Sent Events streams from provider APIs into
// discrete events. It provides a push-callback SSE iterator. IterateSSE reads
// from an [io.Reader] until context cancellation, calling a callback for each
// parsed event. It handles split-mid-line reads and \r\n boundaries (the full
// SSE wire format) so callers only see complete event:data pairs.
package sseparse

import (
	"context"
	"io"
	"strings"
)

// ServerSentEvent is one decoded SSE event.
// Event is the type from the "event:" field (empty if unset); Data is the
// payload joined from all "data:" fields; Raw holds the verbatim lines that
// composed this event.
type ServerSentEvent struct {
	Event string   `json:"event"`
	Data  string   `json:"data"`
	Raw   []string `json:"raw"`
}

// decoderState holds one in-progress event across reads until a blank line ends it.
type decoderState struct {
	event string
	data  []string
	raw   []string
}

// flushSseEvent emits the accumulated event and resets state, or returns nil
// when nothing has been buffered.
func flushSseEvent(state *decoderState) *ServerSentEvent {
	if state.event == "" && len(state.data) == 0 {
		return nil
	}

	event := &ServerSentEvent{
		Event: state.event,
		Data:  strings.Join(state.data, "\n"),
		Raw:   append([]string(nil), state.raw...),
	}
	state.event = ""
	state.data = nil
	state.raw = nil
	return event
}

// decodeSseLine folds one line into state; a blank line flushes the current
// event, and lines beginning with ":" are comments that are ignored.
func decodeSseLine(line string, state *decoderState) *ServerSentEvent {
	if line == "" {
		return flushSseEvent(state)
	}

	state.raw = append(state.raw, line)
	if strings.HasPrefix(line, ":") {
		return nil
	}

	delimiterIndex := strings.Index(line, ":")
	fieldName := line
	value := ""
	if delimiterIndex != -1 {
		fieldName = line[:delimiterIndex]
		value = line[delimiterIndex+1:]
	}
	if strings.HasPrefix(value, " ") {
		value = value[1:]
	}

	if fieldName == "event" {
		state.event = value
	} else if fieldName == "data" {
		state.data = append(state.data, value)
	}

	return nil
}

func nextLineBreakIndex(text string) int {
	carriageReturnIndex := strings.Index(text, "\r")
	newlineIndex := strings.Index(text, "\n")
	if carriageReturnIndex == -1 {
		return newlineIndex
	}
	if newlineIndex == -1 {
		return carriageReturnIndex
	}
	if carriageReturnIndex < newlineIndex {
		return carriageReturnIndex
	}
	return newlineIndex
}

func consumeLine(text string) (line, rest string, ok bool) {
	lineBreakIndex := nextLineBreakIndex(text)
	if lineBreakIndex == -1 {
		return "", "", false
	}

	nextIndex := lineBreakIndex + 1
	if text[lineBreakIndex] == '\r' && nextIndex < len(text) && text[nextIndex] == '\n' {
		nextIndex++
	}
	return text[:lineBreakIndex], text[nextIndex:], true
}

// IterateSSE decodes an SSE byte stream and calls fn for each event.
func IterateSSE(ctx context.Context, r io.Reader, fn func(ServerSentEvent) error) error {
	state := decoderState{}
	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		n, err := r.Read(chunk)
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
			var callErr error
			buffer, callErr = drainCompleteLines(buffer, &state, fn)
			if callErr != nil {
				return callErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	var callErr error
	buffer, callErr = drainCompleteLines(buffer, &state, fn)
	if callErr != nil {
		return callErr
	}
	if len(buffer) > 0 {
		if event := decodeSseLine(string(buffer), &state); event != nil {
			if err := fn(*event); err != nil {
				return err
			}
		}
	}
	if event := flushSseEvent(&state); event != nil {
		if err := fn(*event); err != nil {
			return err
		}
	}
	return nil
}

// drainCompleteLines decodes every complete line in buffer, returning the
// unconsumed trailing bytes that lack a line break.
func drainCompleteLines(buffer []byte, state *decoderState, fn func(ServerSentEvent) error) ([]byte, error) {
	for {
		line, rest, ok := consumeLineBytes(buffer)
		if !ok {
			return buffer, nil
		}
		buffer = rest
		if event := decodeSseLine(string(line), state); event != nil {
			if err := fn(*event); err != nil {
				return buffer, err
			}
		}
	}
}

func consumeLineBytes(text []byte) (line, rest []byte, ok bool) {
	lineBreakIndex := nextLineBreakIndexBytes(text)
	if lineBreakIndex == -1 {
		return nil, nil, false
	}

	nextIndex := lineBreakIndex + 1
	if text[lineBreakIndex] == '\r' && nextIndex < len(text) && text[nextIndex] == '\n' {
		nextIndex++
	}
	return text[:lineBreakIndex], text[nextIndex:], true
}

func nextLineBreakIndexBytes(text []byte) int {
	for i, b := range text {
		if b == '\r' || b == '\n' {
			return i
		}
	}
	return -1
}

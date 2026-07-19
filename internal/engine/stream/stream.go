// Package stream implements a single-producer, multi-consumer event stream with
// a terminal-result contract. Stream[T,R] delivers events via Push and resolves
// a single result R via End/EndWith. The once-guarded finish()
// ensures idempotent close; close-based happens-before gives Result()
// visibility without a mutex.
package stream

import (
	"context"
	"errors"
	"sync"
)

// ErrNoResult is returned by [Stream.Result] when the stream ended without a
// completing event. Go makes the state explicit so callers can recover.
var ErrNoResult = errors.New("stream ended without a result")

// Stream is a generic buffered channel with a final-result contract. T is the
// event type, and R is the final-result type extracted from a completing event.
//
// A single producer calls [Stream.Push] and [Stream.End]. Any number of
// consumers may call [Stream.Events] or [Stream.Result]. Result is independent
// of event consumption: Push appends to an unbounded in-memory queue and
// resolves the final result as soon as a completing event arrives.
type Stream[T, R any] struct {
	events chan T
	done   chan struct{}
	cond   *sync.Cond

	mu     sync.Mutex
	queue  []T
	closed bool
	once   sync.Once

	isComplete func(T) bool
	extract    func(T) R
	result     R
	haveResult bool
}

// New builds a Stream. buf is the channel buffer size, isComplete reports
// whether an event ends the stream, and extract derives the final result from
// that event.
func New[T, R any](buf int, isComplete func(T) bool, extract func(T) R) *Stream[T, R] {
	s := &Stream[T, R]{
		events:     make(chan T, buf),
		done:       make(chan struct{}),
		isComplete: isComplete,
		extract:    extract,
	}
	s.cond = sync.NewCond(&s.mu)
	go s.dispatch()
	return s
}

// Push delivers one event. If the stream has already ended, the event is
// dropped. Completing events are still sent to [Stream.Events] before close.
func (s *Stream[T, R]) Push(ev T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	s.queue = append(s.queue, ev)
	if s.isComplete(ev) {
		s.once.Do(func() { s.finishLocked(s.extract(ev), true) })
	}
	s.cond.Signal()
}

// End closes the stream without a result. It is safe to call more than once.
func (s *Stream[T, R]) End() {
	var zero R
	s.finish(zero, false)
}

// EndWith closes the stream with an explicit final result. This is the path used
// to resolve result() when no completing event was ever pushed (for example, an
// agent loop ending its event stream with the accumulated message list). Go
// splits the optional-result behavior into End / EndWith because Go has no
// optional argument. Safe to call more than once; the first finish wins.
func (s *Stream[T, R]) EndWith(result R) {
	s.finish(result, true)
}

func (s *Stream[T, R]) finish(result R, haveResult bool) {
	s.once.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.finishLocked(result, haveResult)
		s.cond.Signal()
	})
}

func (s *Stream[T, R]) finishLocked(result R, haveResult bool) {
	s.result = result
	s.haveResult = haveResult
	s.closed = true
	close(s.done)
}

// dispatch drains the queue onto the events channel in a dedicated goroutine,
// closing events once the stream is closed and drained.
func (s *Stream[T, R]) dispatch() {
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closed {
			s.cond.Wait()
		}
		if len(s.queue) == 0 && s.closed {
			s.mu.Unlock()
			close(s.events)
			return
		}
		ev := s.queue[0]
		copy(s.queue, s.queue[1:])
		var zero T
		s.queue[len(s.queue)-1] = zero
		s.queue = s.queue[:len(s.queue)-1]
		s.mu.Unlock()

		s.events <- ev
	}
}

// Events returns the receive channel. Range over it until it closes on End.
func (s *Stream[T, R]) Events() <-chan T { return s.events }

// Result blocks until a terminal result is available or ctx is canceled. It
// returns [ErrNoResult] when the stream ended without a completing event.
func (s *Stream[T, R]) Result(ctx context.Context) (R, error) {
	select {
	case <-s.done:
		return s.resultLocked()
	default:
	}

	select {
	case <-s.done:
		return s.resultLocked()
	case <-ctx.Done():
		var zero R
		return zero, ctx.Err()
	}
}

func (s *Stream[T, R]) resultLocked() (R, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.haveResult {
		var zero R
		return zero, ErrNoResult
	}
	return s.result, nil
}

// Package retry decorates provider streams with bounded retry policy.
package retry

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"sync"
	"time"
)

// Jitter bounds the multiplier applied to a local backoff delay.
type Jitter struct {
	Min float64
	Max float64
}

// Clock supplies the current time for retry hints.
type Clock interface {
	Now() time.Time
}

// Sleeper waits for a retry delay while honoring cancellation.
type Sleeper interface {
	Sleep(ctx context.Context, delay time.Duration) error
}

// Random supplies independent jitter samples in [0, 1).
type Random interface {
	Float64() float64
}

// Config controls retry attempts and runtime dependencies.
type Config struct {
	MaxAttempts int
	BaseDelay   time.Duration
	BackoffCap  time.Duration
	MaxDelay    time.Duration
	Jitter      Jitter
	Clock       Clock
	Sleeper     Sleeper
	Random      Random
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type timerSleeper struct{}

func (timerSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}

}

// lockedRandom serializes shared jitter samples because several retry streams can run at once.
type lockedRandom struct {
	mu sync.Mutex
	r  *rand.Rand
}

func (r *lockedRandom) Float64() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.Float64()
}

// DefaultConfig returns the production retry policy and isolated dependencies.
func DefaultConfig() Config {
	seed := uint64(time.Now().UnixNano())
	return Config{
		MaxAttempts: 11,
		BaseDelay:   500 * time.Millisecond,
		BackoffCap:  8 * time.Second,
		MaxDelay:    5 * time.Minute,
		Jitter:      Jitter{Min: 0.75, Max: 1},
		Clock:       realClock{},
		Sleeper:     timerSleeper{},
		Random:      &lockedRandom{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))},
	}
}

// Validate reports whether Config can run a retry stream.
func (c Config) Validate() error {
	if c.MaxAttempts < 1 {
		return fmt.Errorf("max attempts must be at least one")
	}
	if c.BaseDelay <= 0 {
		return fmt.Errorf("base delay must be positive")
	}
	if c.BackoffCap < c.BaseDelay {
		return fmt.Errorf("backoff cap must be at least base delay")
	}
	if nilDependency(c.Clock) || nilDependency(c.Sleeper) || nilDependency(c.Random) {
		return fmt.Errorf("clock, sleeper, and random must be provided")
	}
	if math.IsNaN(c.Jitter.Min) || math.IsInf(c.Jitter.Min, 0) || math.IsNaN(c.Jitter.Max) || math.IsInf(c.Jitter.Max, 0) {
		return fmt.Errorf("jitter bounds must be finite")
	}
	if c.Jitter.Min < 0 || c.Jitter.Max > 1 || c.Jitter.Min > c.Jitter.Max {
		return fmt.Errorf("jitter bounds must be within [0, 1] and ordered")
	}
	return nil
}

// nilDependency catches typed nils inside interfaces before a retry attempt reaches a method call.
func nilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// nominalDelay doubles locally while capping before Duration arithmetic can overflow.
func nominalDelay(base, cap time.Duration, ordinal int) time.Duration {
	delay := base
	for i := 1; i < ordinal && delay < cap; i++ {
		if delay > cap/2 {
			return cap
		}
		delay *= 2
	}
	if delay > cap {
		return cap
	}
	return delay
}

// jitterDelay contains bad injected samples so they can't produce a negative or unbounded wait.
func jitterDelay(nominal time.Duration, jitter Jitter, random Random) time.Duration {
	sample := random.Float64()
	if sample < 0 || math.IsNaN(sample) {
		sample = 0
	} else if sample > 1 || math.IsInf(sample, 0) {
		sample = 1
	}
	factor := jitter.Min + (jitter.Max-jitter.Min)*sample
	delay := math.Round(float64(nominal) * factor)
	if delay <= 0 {
		return 0
	}
	if delay >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(delay)
}

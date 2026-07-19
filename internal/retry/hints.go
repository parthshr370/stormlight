package retry

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryAfter returns the largest valid provider retry hint.
func RetryAfter(headers map[string]string, now time.Time) (time.Duration, bool) {
	var largest time.Duration
	valid := false
	for name, value := range headers {
		var delay time.Duration
		var ok bool
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "retry-after-ms":
			delay, ok = decimalDuration(value, time.Millisecond)
		case "retry-after":
			delay, ok = retryAfterSecondsOrDate(value, now)
		case "x-ratelimit-reset-ms":
			delay, ok = resetDelay(value, now, time.Millisecond)
		case "x-ratelimit-reset":
			delay, ok = resetDelay(value, now, time.Second)
		default:
			continue
		}
		if ok && (!valid || delay > largest) {
			largest, valid = delay, true
		}
	}
	return largest, valid
}

func retryAfterSecondsOrDate(value string, now time.Time) (time.Duration, bool) {
	if delay, ok := decimalDuration(value, time.Second); ok {
		return delay, true
	}
	date, err := http.ParseTime(strings.TrimSpace(value))
	if err != nil || !date.After(now) {
		return 0, false
	}
	return date.Sub(now), true
}

// resetDelay interprets ambiguous reset values, treating large ones as epoch timestamps and smaller ones as relative waits.
func resetDelay(value string, now time.Time, relativeUnit time.Duration) (time.Duration, bool) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return 0, false
	}
	if parsed > 1e12 {
		return absoluteDelay(parsed, time.Millisecond, now)
	}
	if parsed > 1e9 {
		return absoluteDelay(parsed, time.Second, now)
	}
	return durationCeilMillis(parsed, relativeUnit)
}

// absoluteDelay turns a numeric reset timestamp into a remaining wait without accepting stale or overflowing hints.
func absoluteDelay(value float64, unit time.Duration, now time.Time) (time.Duration, bool) {
	nanos := value * float64(unit)
	if nanos >= float64(math.MaxInt64) || nanos <= float64(math.MinInt64) {
		return 0, false
	}
	at := time.Unix(0, int64(math.Ceil(nanos)))
	if !at.After(now) {
		return 0, false
	}
	return at.Sub(now), true
}

func decimalDuration(value string, unit time.Duration) (time.Duration, bool) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return 0, false
	}
	return durationCeilMillis(parsed, unit)
}

// durationCeilMillis converts a positive value expressed in unit into a Duration
// rounded up to a whole millisecond, per the retry-hint contract (§3.4). It
// bounds-checks in millisecond units before the Duration conversion so an
// oversized hint is reported invalid rather than wrapping to a negative value.
func durationCeilMillis(value float64, unit time.Duration) (time.Duration, bool) {
	millis := value * (float64(unit) / float64(time.Millisecond))
	if math.IsNaN(millis) || millis <= 0 {
		return 0, false
	}
	millis = math.Ceil(millis)
	if millis >= float64(math.MaxInt64)/float64(time.Millisecond) {
		return 0, false
	}
	return time.Duration(millis) * time.Millisecond, true
}

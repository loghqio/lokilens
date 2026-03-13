package loki

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var relativeTimePattern = regexp.MustCompile(`^(\d+)\s*(s|sec|second|seconds|m|min|minute|minutes|h|hour|hours|d|day|days|w|week|weeks)\s*(ago)?$`)

// ParseRelativeTime parses time strings in various formats:
// - "now" → current time
// - "2h ago", "30m ago", "1d ago" → relative to now
// - "2h", "30m" → also treated as relative (ago implied)
// - RFC3339 → parsed directly
// - Unix nanosecond timestamp → parsed as integer
func ParseRelativeTime(input string) (time.Time, error) {
	input = strings.TrimSpace(input)

	if input == "" || strings.EqualFold(input, "now") {
		return time.Now(), nil
	}

	// Try relative time: "2h ago", "30m", "1d ago" (case-insensitive)
	lower := strings.ToLower(input)
	if matches := relativeTimePattern.FindStringSubmatch(lower); matches != nil {
		amount, _ := strconv.Atoi(matches[1])
		unit := matches[2]
		dur := toDuration(amount, unit)
		return time.Now().Add(-dur), nil
	}

	// Try Go duration format: "2h30m", "45s"
	if d, err := time.ParseDuration(lower); err == nil {
		return time.Now().Add(-d), nil
	}

	// Try RFC3339
	if t, err := time.Parse(time.RFC3339, input); err == nil {
		return t, nil
	}

	// Try RFC3339Nano
	if t, err := time.Parse(time.RFC3339Nano, input); err == nil {
		return t, nil
	}

	// Try Unix nanosecond timestamp
	if ns, err := strconv.ParseInt(input, 10, 64); err == nil {
		return time.Unix(0, ns), nil
	}

	return time.Time{}, fmt.Errorf("cannot parse time %q: expected relative (e.g., '2h ago'), RFC3339, or Unix nanoseconds", input)
}

func toDuration(amount int, unit string) time.Duration {
	switch unit {
	case "s", "sec", "second", "seconds":
		return time.Duration(amount) * time.Second
	case "m", "min", "minute", "minutes":
		return time.Duration(amount) * time.Minute
	case "h", "hour", "hours":
		return time.Duration(amount) * time.Hour
	case "d", "day", "days":
		return time.Duration(amount) * 24 * time.Hour
	case "w", "week", "weeks":
		return time.Duration(amount) * 7 * 24 * time.Hour
	default:
		return time.Duration(amount) * time.Hour
	}
}

// FormatNano formats a time.Time as nanosecond Unix epoch string for Loki API.
func FormatNano(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}

// ParseNanoTimestamp parses a nanosecond Unix epoch string into time.Time.
func ParseNanoTimestamp(ns string) (time.Time, error) {
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid nanosecond timestamp %q: %w", ns, err)
	}
	return time.Unix(0, n), nil
}

// Clamp constrains a value between min and max.
func Clamp(val, min, max int) int {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

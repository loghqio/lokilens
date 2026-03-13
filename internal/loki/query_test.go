package loki

import (
	"testing"
	"time"
)

func TestParseRelativeTime_Now(t *testing.T) {
	before := time.Now()
	result, err := ParseRelativeTime("now")
	after := time.Now()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Before(before) || result.After(after) {
		t.Error("'now' should be approximately current time")
	}
}

func TestParseRelativeTime_Empty(t *testing.T) {
	_, err := ParseRelativeTime("")
	if err != nil {
		t.Fatalf("empty should be treated as now: %v", err)
	}
}

func TestParseRelativeTime_RelativeFormats(t *testing.T) {
	cases := []struct {
		input       string
		minAgo      time.Duration
		maxAgo      time.Duration
	}{
		{"2h ago", 2*time.Hour - time.Second, 2*time.Hour + time.Second},
		{"30m ago", 30*time.Minute - time.Second, 30*time.Minute + time.Second},
		{"1d ago", 24*time.Hour - time.Second, 24*time.Hour + time.Second},
		{"30m", 30*time.Minute - time.Second, 30*time.Minute + time.Second},
		{"2h", 2*time.Hour - time.Second, 2*time.Hour + time.Second},
	}
	for _, tc := range cases {
		result, err := ParseRelativeTime(tc.input)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", tc.input, err)
			continue
		}
		ago := time.Since(result)
		if ago < tc.minAgo || ago > tc.maxAgo {
			t.Errorf("%q: expected ~%v ago, got %v ago", tc.input, tc.minAgo, ago)
		}
	}
}

func TestParseRelativeTime_RFC3339(t *testing.T) {
	input := "2024-01-15T10:30:00Z"
	result, err := ParseRelativeTime(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected, _ := time.Parse(time.RFC3339, input)
	if !result.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, result)
	}
}

func TestParseRelativeTime_Invalid(t *testing.T) {
	_, err := ParseRelativeTime("yesterday")
	if err == nil {
		t.Error("expected error for invalid time format")
	}
}

func TestClamp(t *testing.T) {
	cases := []struct {
		val, min, max, expected int
	}{
		{50, 1, 100, 50},
		{0, 1, 100, 1},
		{200, 1, 100, 100},
		{1, 1, 100, 1},
		{100, 1, 100, 100},
	}
	for _, tc := range cases {
		result := Clamp(tc.val, tc.min, tc.max)
		if result != tc.expected {
			t.Errorf("Clamp(%d, %d, %d) = %d, want %d", tc.val, tc.min, tc.max, result, tc.expected)
		}
	}
}

func TestFormatNano(t *testing.T) {
	ts := time.Unix(1609459200, 0)
	result := FormatNano(ts)
	if result == "" {
		t.Error("expected non-empty string")
	}
}

func TestParseNanoTimestamp(t *testing.T) {
	ts := time.Now()
	nano := FormatNano(ts)
	result, err := ParseNanoTimestamp(nano)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.UnixNano() != ts.UnixNano() {
		t.Errorf("roundtrip failed: %v != %v", result, ts)
	}
}

func TestParseNanoTimestamp_Invalid(t *testing.T) {
	_, err := ParseNanoTimestamp("not-a-number")
	if err == nil {
		t.Error("expected error for invalid input")
	}
}

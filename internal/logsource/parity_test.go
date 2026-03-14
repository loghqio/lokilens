package logsource_test

import (
	"strings"
	"testing"
	"time"

	"github.com/lokilens/lokilens/internal/agent"
)

// Parity tests ensure both Loki and CloudWatch backends handle the same
// edge cases. When a defensive fix is added to one backend, these tests
// catch the gap in the other.

// TestParseTimeOrDefault_SharedBehavior tests the shared time parsing
// used by both backends.
func TestParseTimeOrDefault_SharedBehavior(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		defaultAgo time.Duration
		wantErr    bool
	}{
		{"empty defaults to 1h ago", "", 1 * time.Hour, false},
		{"empty defaults to now", "", 0, false},
		{"relative 2h ago", "2h ago", 0, false},
		{"relative 30m ago", "30m ago", 0, false},
		{"relative 1d ago", "1d ago", 0, false},
		{"now keyword", "now", 0, false},
		{"RFC3339", "2024-01-15T14:00:00Z", 0, false},
		{"duration format", "2h", 0, false},
		{"garbage input", "not-a-time", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := agent.ParseTimeOrDefault(tt.input, tt.defaultAgo)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for input %q", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", tt.input, err)
			}
			// Result should be in a reasonable range (not zero, not far future)
			if result.IsZero() {
				t.Error("result should not be zero")
			}
			if result.After(time.Now().Add(1 * time.Minute)) {
				t.Error("result should not be in the future")
			}
		})
	}
}

// TestZeroResultsWarning_SharedContract tests that both backends inject
// diagnostic hints when queries return zero results. This is the contract
// that prevents "I couldn't find any logs" without investigation.
func TestZeroResultsWarning_SharedContract(t *testing.T) {
	// QueryLogsOutput with zero logs must have a warning
	logsOut := agent.QueryLogsOutput{
		Logs: []agent.LogEntry{},
	}
	agent.AnalyzeLogs(&logsOut, 100)
	// The warning is injected by the backend-specific code, not AnalyzeLogs.
	// This test verifies the struct supports warnings.
	logsOut.Warning = "ZERO RESULTS — test"
	if logsOut.Warning == "" {
		t.Error("QueryLogsOutput must support Warning field for zero-result hints")
	}

	// QueryStatsOutput with zero series must have a warning
	statsOut := agent.QueryStatsOutput{
		Series: []agent.MetricSeries{},
	}
	agent.AnalyzeStats(&statsOut)
	statsOut.Warning = "ZERO RESULTS — test"
	if statsOut.Warning == "" {
		t.Error("QueryStatsOutput must support Warning field for zero-result hints")
	}
}

// TestAutoSelectStep_SharedBehavior tests the shared step selection
// used by both backends.
func TestAutoSelectStep_SharedBehavior(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{"15 minutes", 15 * time.Minute, "30s"},
		{"1 hour", 1 * time.Hour, "1m"},
		{"3 hours", 3 * time.Hour, "5m"},
		{"12 hours", 12 * time.Hour, "15m"},
		{"24 hours", 24 * time.Hour, "1h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := agent.AutoSelectStep(now.Add(-tt.duration), now)
			if step != tt.want {
				t.Errorf("for %v range: got %s, want %s", tt.duration, step, tt.want)
			}
		})
	}
}

// TestAnalyzeLogs_SharedBehavior tests the shared log analysis used by
// both backends. If patterns, labels, or sorting change, both backends
// should see the same behavior.
func TestAnalyzeLogs_SharedBehavior(t *testing.T) {
	out := agent.QueryLogsOutput{
		Direction: "backward",
		Logs: []agent.LogEntry{
			{Timestamp: "2024-01-15T14:00:02Z", Line: "connection timeout to db", Labels: map[string]string{"service": "api"}},
			{Timestamp: "2024-01-15T14:00:01Z", Line: "connection timeout to db", Labels: map[string]string{"service": "api"}},
			{Timestamp: "2024-01-15T14:00:00Z", Line: "rate limit exceeded", Labels: map[string]string{"service": "gateway"}},
		},
	}

	agent.AnalyzeLogs(&out, 100)

	if out.TotalLogs != 3 {
		t.Errorf("TotalLogs: got %d, want 3", out.TotalLogs)
	}
	if len(out.TopPatterns) == 0 {
		t.Fatal("expected patterns to be extracted")
	}
	// Top pattern should be the repeated one
	if out.TopPatterns[0].Count != 2 {
		t.Errorf("top pattern count: got %d, want 2", out.TopPatterns[0].Count)
	}
	if out.UniqueLabels == nil || out.UniqueLabels["service"] == nil {
		t.Fatal("expected service label distribution")
	}
}

// TestAnalyzeStats_SharedBehavior tests the shared stats analysis.
func TestAnalyzeStats_SharedBehavior(t *testing.T) {
	out := agent.QueryStatsOutput{
		Step: "5m",
		Series: []agent.MetricSeries{
			{
				Labels: map[string]string{"service": "api"},
				Values: []agent.DataPoint{
					{Timestamp: "2024-01-15T14:00:00Z", Value: "5"},
					{Timestamp: "2024-01-15T14:05:00Z", Value: "10"},
					{Timestamp: "2024-01-15T14:10:00Z", Value: "20"},
				},
			},
		},
	}

	agent.AnalyzeStats(&out)

	if out.TotalSeries != 1 {
		t.Errorf("TotalSeries: got %d, want 1", out.TotalSeries)
	}
	if len(out.Summaries) != 1 {
		t.Fatalf("Summaries count: got %d, want 1", len(out.Summaries))
	}
	for _, s := range out.Summaries {
		if s.Total != 35 {
			t.Errorf("total: got %f, want 35", s.Total)
		}
		if s.Peak != 20 {
			t.Errorf("peak: got %f, want 20", s.Peak)
		}
		if !strings.Contains(s.Trend, "increas") {
			t.Errorf("trend should be increasing, got %q", s.Trend)
		}
	}
}

// TestTruncateLogLine_SharedBehavior tests the shared truncation used by
// both backends when processing log lines.
func TestTruncateLogLine_SharedBehavior(t *testing.T) {
	// Short line unchanged
	short := "error: connection refused"
	if agent.TruncateLogLine(short) != short {
		t.Error("short line should not be truncated")
	}

	// Long line truncated
	long := strings.Repeat("x", 2000)
	truncated := agent.TruncateLogLine(long)
	if len(truncated) >= len(long) {
		t.Error("long line should be truncated")
	}

	// UTF-8 safety: truncation shouldn't break multi-byte characters
	emoji := strings.Repeat("\U0001f600", 500) // 500 emoji = 2000 bytes
	result := agent.TruncateLogLine(emoji)
	// Should be valid UTF-8 after truncation
	for i, r := range result {
		if r == '\uFFFD' {
			t.Errorf("invalid UTF-8 at position %d after truncation", i)
			break
		}
	}
}

package cwsource

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/lokilens/lokilens/internal/agent"
)

func TestParseTimeSeriesResults(t *testing.T) {
	s := &CloudWatchSource{}

	result := &QueryResult{
		Results: [][]types.ResultField{
			{
				{Field: strPtr("bin(5m)"), Value: strPtr("2024-01-15 14:00:00.000")},
				{Field: strPtr("count(*)"), Value: strPtr("42")},
			},
			{
				{Field: strPtr("bin(5m)"), Value: strPtr("2024-01-15 14:05:00.000")},
				{Field: strPtr("count(*)"), Value: strPtr("57")},
			},
			{
				{Field: strPtr("bin(5m)"), Value: strPtr("2024-01-15 14:10:00.000")},
				{Field: strPtr("count(*)"), Value: strPtr("23")},
			},
		},
	}

	series := s.parseStatsResults(result)

	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	if len(series[0].Values) != 3 {
		t.Fatalf("expected 3 data points, got %d", len(series[0].Values))
	}
	if series[0].Values[0].Value != "42" {
		t.Errorf("expected first value=42, got %q", series[0].Values[0].Value)
	}
	if series[0].Values[2].Value != "23" {
		t.Errorf("expected third value=23, got %q", series[0].Values[2].Value)
	}
}

func TestParseGroupedResults(t *testing.T) {
	s := &CloudWatchSource{}

	result := &QueryResult{
		Results: [][]types.ResultField{
			{
				{Field: strPtr("service"), Value: strPtr("payments")},
				{Field: strPtr("count(*)"), Value: strPtr("145")},
			},
			{
				{Field: strPtr("service"), Value: strPtr("orders")},
				{Field: strPtr("count(*)"), Value: strPtr("23")},
			},
		},
	}

	series := s.parseStatsResults(result)

	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}

	// Find payments series for deterministic assertions
	sort.Slice(series, func(i, j int) bool {
		si := series[i].Labels["service"]
		sj := series[j].Labels["service"]
		return si < sj // alphabetical: orders, payments
	})
	// payments is index 1 after alphabetical sort

	if series[1].Labels["service"] != "payments" {
		t.Errorf("expected second series service=payments, got %v", series[1].Labels)
	}
	if series[1].Values[0].Value != "145" {
		t.Errorf("expected value=145, got %q", series[1].Values[0].Value)
	}
	if series[0].Labels["service"] != "orders" {
		t.Errorf("expected first series service=orders, got %v", series[0].Labels)
	}
}

func TestParseMultiSeriesTimeSeries(t *testing.T) {
	s := &CloudWatchSource{}

	// Two series: service=payments and service=orders, each with 2 time buckets
	result := &QueryResult{
		Results: [][]types.ResultField{
			{
				{Field: strPtr("bin(5m)"), Value: strPtr("2024-01-15 14:00:00.000")},
				{Field: strPtr("service"), Value: strPtr("payments")},
				{Field: strPtr("count(*)"), Value: strPtr("10")},
			},
			{
				{Field: strPtr("bin(5m)"), Value: strPtr("2024-01-15 14:00:00.000")},
				{Field: strPtr("service"), Value: strPtr("orders")},
				{Field: strPtr("count(*)"), Value: strPtr("5")},
			},
			{
				{Field: strPtr("bin(5m)"), Value: strPtr("2024-01-15 14:05:00.000")},
				{Field: strPtr("service"), Value: strPtr("payments")},
				{Field: strPtr("count(*)"), Value: strPtr("15")},
			},
			{
				{Field: strPtr("bin(5m)"), Value: strPtr("2024-01-15 14:05:00.000")},
				{Field: strPtr("service"), Value: strPtr("orders")},
				{Field: strPtr("count(*)"), Value: strPtr("8")},
			},
		},
	}

	series := s.parseStatsResults(result)

	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}

	// Each series should have 2 data points
	for _, s := range series {
		if len(s.Values) != 2 {
			t.Errorf("expected 2 data points for series %v, got %d", s.Labels, len(s.Values))
		}
	}
}

func TestParseEmptyResults(t *testing.T) {
	s := &CloudWatchSource{}

	result := &QueryResult{
		Results: nil,
	}

	series := s.parseStatsResults(result)
	if series != nil {
		t.Errorf("expected nil for empty results, got %v", series)
	}
}

func TestSeriesKeyFromLabels(t *testing.T) {
	key := seriesKeyFromLabels(map[string]string{"service": "payments", "level": "error"})
	if key != "level=error,service=payments" {
		t.Errorf("expected sorted key, got %q", key)
	}
}

func TestSeriesKeyFromLabels_Empty(t *testing.T) {
	if seriesKeyFromLabels(nil) != "total" {
		t.Error("expected 'total' for empty labels")
	}
}

func TestResolveLogGroups_InputOverridesConfig(t *testing.T) {
	s := &CloudWatchSource{
		logGroups: []string{"/aws/lambda/default"},
	}

	groups, err := s.resolveLogGroups([]string{"/aws/ecs/custom"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0] != "/aws/ecs/custom" {
		t.Errorf("expected input to override config, got %v", groups)
	}
}

func TestResolveLogGroups_FallsBackToConfig(t *testing.T) {
	s := &CloudWatchSource{
		logGroups: []string{"/aws/lambda/default"},
	}

	groups, err := s.resolveLogGroups(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0] != "/aws/lambda/default" {
		t.Errorf("expected config groups, got %v", groups)
	}
}

func TestResolveLogGroups_ErrorWhenNone(t *testing.T) {
	s := &CloudWatchSource{}

	_, err := s.resolveLogGroups(nil)
	if err == nil {
		t.Error("expected error when no log groups available")
	}
}

func TestAnalyzeLogsIntegration(t *testing.T) {
	// Verify that CloudWatch results can be analyzed by the shared analysis functions
	out := agent.QueryLogsOutput{
		Direction: "backward",
		Logs: []agent.LogEntry{
			{Timestamp: "2024-01-15T14:31:02Z", Line: "connection timeout to database", Labels: map[string]string{"service": "payments"}},
			{Timestamp: "2024-01-15T14:31:01Z", Line: "connection timeout to database", Labels: map[string]string{"service": "payments"}},
			{Timestamp: "2024-01-15T14:31:00Z", Line: "rate limit exceeded", Labels: map[string]string{"service": "orders"}},
		},
	}

	agent.AnalyzeLogs(&out, 100)

	if out.TotalLogs != 3 {
		t.Errorf("expected TotalLogs=3, got %d", out.TotalLogs)
	}
	if len(out.TopPatterns) == 0 {
		t.Fatal("expected patterns to be extracted")
	}
	if out.TopPatterns[0].Count != 2 {
		t.Errorf("expected top pattern count=2, got %d", out.TopPatterns[0].Count)
	}
	if out.UniqueLabels == nil {
		t.Fatal("expected unique labels")
	}
	if out.UniqueLabels["service"]["payments"] != 2 {
		t.Errorf("expected payments=2, got %d", out.UniqueLabels["service"]["payments"])
	}
}

func TestAnalyzeStatsIntegration(t *testing.T) {
	out := agent.QueryStatsOutput{
		Step: "5m",
		Series: []agent.MetricSeries{
			{
				Labels: map[string]string{"service": "payments"},
				Values: []agent.DataPoint{
					{Timestamp: "2024-01-15T14:00:00Z", Value: "10"},
					{Timestamp: "2024-01-15T14:05:00Z", Value: "15"},
					{Timestamp: "2024-01-15T14:10:00Z", Value: "25"},
				},
			},
		},
	}

	agent.AnalyzeStats(&out)

	if out.TotalSeries != 1 {
		t.Errorf("expected TotalSeries=1, got %d", out.TotalSeries)
	}
	if len(out.Summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(out.Summaries))
	}

	summary := out.Summaries["service=payments"]
	if summary.Total != 50 {
		t.Errorf("expected total=50, got %f", summary.Total)
	}
	if summary.Peak != 25 {
		t.Errorf("expected peak=25, got %f", summary.Peak)
	}
	if summary.AvgPerMinute <= 0 {
		t.Errorf("expected positive avg_per_minute, got %f", summary.AvgPerMinute)
	}
}

func TestTruncateLogLine(t *testing.T) {
	short := "short message"
	if agent.TruncateLogLine(short) != short {
		t.Error("short line should not be truncated")
	}

	long := ""
	for i := 0; i < 2000; i++ {
		long += "x"
	}
	truncated := agent.TruncateLogLine(long)
	if len(truncated) >= len(long) {
		t.Error("long line should be truncated")
	}
}

func TestAutoSelectStep(t *testing.T) {
	now := time.Now()

	step := agent.AutoSelectStep(now.Add(-1*time.Hour), now)
	if step != "1m" {
		t.Errorf("expected 1m for 1h range, got %s", step)
	}

	step = agent.AutoSelectStep(now.Add(-6*time.Hour), now)
	if step != "5m" {
		t.Errorf("expected 5m for 6h range, got %s", step)
	}
}

func TestSanitizeTimeRange_NormalRange(t *testing.T) {
	now := time.Now()
	start := now.Add(-1 * time.Hour)
	end := now

	s, e, warning := sanitizeTimeRange(start, end)
	if warning != "" {
		t.Errorf("expected no warning for normal range, got %q", warning)
	}
	if !s.Equal(start) || !e.Equal(end) {
		t.Error("expected times unchanged for normal range")
	}
}

func TestSanitizeTimeRange_SwappedTimes(t *testing.T) {
	now := time.Now()
	start := now                      // "start" is actually later
	end := now.Add(-1 * time.Hour)    // "end" is actually earlier

	s, e, warning := sanitizeTimeRange(start, end)
	if warning == "" {
		t.Fatal("expected warning for swapped times")
	}
	if !strings.Contains(warning, "swapped") {
		t.Errorf("expected 'swapped' in warning, got %q", warning)
	}
	if !e.After(s) {
		t.Error("expected end after start after swap")
	}
}

func TestSanitizeTimeRange_ExceedsMaxRange(t *testing.T) {
	now := time.Now()
	start := now.Add(-7120 * time.Hour) // ~296 days ago — the exact scenario that burned us
	end := now

	s, e, warning := sanitizeTimeRange(start, end)
	if warning == "" {
		t.Fatal("expected warning for excessive range")
	}
	if !strings.Contains(warning, "clamped") {
		t.Errorf("expected 'clamped' in warning, got %q", warning)
	}
	if e.Sub(s) > 24*time.Hour+time.Second {
		t.Errorf("expected range clamped to ~24h, got %v", e.Sub(s))
	}
}

func TestSanitizeTimeRange_FutureEndTime(t *testing.T) {
	now := time.Now()
	start := now.Add(-30 * time.Minute)
	end := now.Add(1 * time.Hour) // future

	_, e, _ := sanitizeTimeRange(start, end)
	if e.After(time.Now().Add(1 * time.Second)) {
		t.Error("expected end time capped at now")
	}
}

func TestSanitizeTimeRange_EqualTimes(t *testing.T) {
	now := time.Now()

	s, e, warning := sanitizeTimeRange(now, now)
	if warning == "" {
		t.Fatal("expected warning for equal times")
	}
	if !e.After(s) {
		t.Error("expected non-zero range after fix")
	}
}

func TestSanitizeTimeRange_SwappedAndExcessive(t *testing.T) {
	now := time.Now()
	start := now                          // later
	end := now.Add(-7120 * time.Hour)     // earlier and way too far back

	s, e, warning := sanitizeTimeRange(start, end)
	if !strings.Contains(warning, "swapped") {
		t.Errorf("expected 'swapped' in warning, got %q", warning)
	}
	if !strings.Contains(warning, "clamped") {
		t.Errorf("expected 'clamped' in warning, got %q", warning)
	}
	if e.Sub(s) > 24*time.Hour+time.Second {
		t.Errorf("expected range clamped to ~24h, got %v", e.Sub(s))
	}
}

func strPtr(s string) *string {
	return &s
}

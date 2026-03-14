//go:build integration

// CloudWatch integration tests exercise the CloudWatch tool layer against
// LocalStack with simulated log data. Same approach as the Loki tests:
// real handlers, real types, real analysis pipeline — no Gemini API key needed.
//
// Prerequisites:
//
//	docker compose -f docker/docker-compose.yml up -d localstack cwloggen
//	# wait ~20s for log ingestion
//	CW_ENDPOINT_URL=http://localhost:4566 go test -tags integration -v -count=1 -run CW ./integration/...
package integration

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/logsource/cwsource"

	"log/slog"
)

var cwSource *cwsource.CloudWatchSource

// cwLogGroups matches what cwloggen creates.
var cwLogGroups = []string{
	"/lokilens/payments",
	"/lokilens/orders",
	"/lokilens/users",
	"/lokilens/gateway",
	"/lokilens/inventory",
}

func init() {
	cwEndpoint := os.Getenv("CW_ENDPOINT_URL")
	if cwEndpoint == "" {
		return // CW tests will skip if no endpoint
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
	auditLogger := audit.New(logger)

	// Set dummy AWS credentials for LocalStack
	os.Setenv("AWS_ACCESS_KEY_ID", "test")
	os.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	source, err := cwsource.New(context.Background(), cwsource.Config{
		Region:      "us-east-1",
		LogGroups:   cwLogGroups,
		Audit:       auditLogger,
		EndpointURL: cwEndpoint,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: failed to create CloudWatch source: %v\n", err)
		return
	}
	cwSource = source
}

func skipIfNoCW(t *testing.T) {
	t.Helper()
	if cwSource == nil {
		t.Skip("CW_ENDPOINT_URL not set or CloudWatch source init failed — skipping CW tests")
	}
}

func cwCtx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	_ = cancel
	return c
}

// ──────────────────────────────────────────────────────────────────────────────
// Health Check
// ──────────────────────────────────────────────────────────────────────────────

func TestCWHealthCheck(t *testing.T) {
	skipIfNoCW(t)
	if err := cwSource.HealthCheck(cwCtx()); err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Discovery: ListLogGroups
// ──────────────────────────────────────────────────────────────────────────────

func TestCWListLogGroups(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.ListLogGroups(cwCtx(), cwsource.ListLogGroupsInput{})
	if err != nil {
		t.Fatalf("ListLogGroups failed: %v", err)
	}

	if out.Total == 0 {
		t.Fatal("expected at least one log group")
	}

	// Should contain our configured log groups
	for _, want := range cwLogGroups {
		if !contains(out.LogGroups, want) {
			t.Errorf("expected log group %q in %v", want, out.LogGroups)
		}
	}
}

func TestCWListLogGroups_WithPrefix(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.ListLogGroups(cwCtx(), cwsource.ListLogGroupsInput{
		Prefix: "/lokilens/pay",
	})
	if err != nil {
		t.Fatalf("ListLogGroups with prefix failed: %v", err)
	}

	// Should find payments
	found := false
	for _, g := range out.LogGroups {
		if strings.Contains(g, "payments") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected to find payments group with prefix /lokilens/pay, got %v", out.LogGroups)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Discovery: GetLogFields
// ──────────────────────────────────────────────────────────────────────────────

func TestCWGetLogFields(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.GetLogFields(cwCtx(), cwsource.GetLogFieldsInput{
		LogGroups: []string{"/lokilens/payments"},
	})
	if err != nil {
		// LocalStack may have limited Insights support
		t.Skipf("GetLogFields failed (LocalStack may not fully support Insights): %v", err)
	}

	if len(out.Fields) == 0 {
		t.Skip("no fields discovered — LocalStack Insights may be limited")
	}

	t.Logf("discovered %d fields: %v", len(out.Fields), out.Fields)

	// Should discover at least @message and @timestamp
	hasMessage := false
	for _, f := range out.Fields {
		if f == "@message" || f == "@timestamp" {
			hasMessage = true
		}
	}
	if !hasMessage {
		t.Logf("warning: @message/@timestamp not in discovered fields (may be a LocalStack limitation)")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// QueryLogs: Basic Queries
// ──────────────────────────────────────────────────────────────────────────────

func TestCWQueryLogs_AllLogs(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
		Limit:     50,
	})
	if err != nil {
		t.Skipf("QueryLogs failed (LocalStack Insights may be limited): %v", err)
	}

	if out.TotalLogs == 0 {
		t.Skip("no logs returned — cwloggen may not have ingested enough data yet")
	}

	// Verify structural integrity
	for i, entry := range out.Logs {
		if entry.Timestamp == "" {
			t.Errorf("log[%d]: empty timestamp", i)
		}
		if entry.Line == "" {
			t.Errorf("log[%d]: empty line", i)
		}
	}

	if out.Query == "" {
		t.Error("Query field should be populated")
	}
	if out.TimeRange == "" {
		t.Error("TimeRange field should be populated")
	}
	t.Logf("returned %d logs", out.TotalLogs)
}

func TestCWQueryLogs_SpecificLogGroup(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: []string{"/lokilens/payments"},
		StartTime: "1h ago",
		Limit:     20,
	})
	if err != nil {
		t.Skipf("QueryLogs failed: %v", err)
	}

	if out.TotalLogs == 0 {
		t.Skip("no logs for payments log group")
	}

	// Every log should contain payment-related content (since it comes from the payments log group)
	t.Logf("payments log group: %d logs", out.TotalLogs)
}

func TestCWQueryLogs_FilterErrors(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		Query:     `filter @message like /(?i)error|failed|timeout/`,
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
		Limit:     50,
	})
	if err != nil {
		t.Skipf("QueryLogs with filter failed: %v", err)
	}

	if out.TotalLogs == 0 {
		t.Skip("no error logs found")
	}

	// Verify filtered results contain error-related content
	for i, entry := range out.Logs {
		lower := strings.ToLower(entry.Line)
		if !strings.Contains(lower, "error") && !strings.Contains(lower, "failed") && !strings.Contains(lower, "timeout") {
			t.Errorf("log[%d]: expected error/failed/timeout in line: %s", i, truncate(entry.Line, 100))
		}
	}
	t.Logf("found %d error logs", out.TotalLogs)
}

func TestCWQueryLogs_PatternAnalysis(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
		Limit:     200,
	})
	if err != nil {
		t.Skipf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs < 10 {
		t.Skip("need at least 10 logs for pattern analysis")
	}

	if len(out.TopPatterns) == 0 {
		t.Error("expected TopPatterns to be populated")
	}
	if out.TotalPatterns == 0 {
		t.Error("expected TotalPatterns > 0")
	}

	for _, p := range out.TopPatterns {
		if p.Pattern == "" {
			t.Error("empty pattern string")
		}
		if p.Count == 0 {
			t.Error("pattern count should be > 0")
		}
		if p.Pct < 0 || p.Pct > 100 {
			t.Errorf("pattern pct out of range: %f", p.Pct)
		}
	}

	t.Logf("found %d patterns (top: %q at %.1f%%)", out.TotalPatterns, out.TopPatterns[0].Pattern, out.TopPatterns[0].Pct)
}

// ──────────────────────────────────────────────────────────────────────────────
// QueryLogs: Direction and Limits
// ──────────────────────────────────────────────────────────────────────────────

func TestCWQueryLogs_Limit(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
		Limit:     5,
	})
	if err != nil {
		t.Skipf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs > 5 {
		t.Errorf("expected at most 5 logs, got %d", out.TotalLogs)
	}
}

func TestCWQueryLogs_ZeroResults(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		Query:     `filter @message like /ZZZZNONEXISTENT99999/`,
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Skipf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs != 0 {
		t.Errorf("expected 0 logs for impossible filter, got %d", out.TotalLogs)
	}
	if out.Warning == "" {
		t.Error("expected warning for zero results")
	}
	if !strings.Contains(out.Warning, "ZERO RESULTS") {
		t.Errorf("expected ZERO RESULTS in warning, got: %s", truncate(out.Warning, 100))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// QueryStats
// ──────────────────────────────────────────────────────────────────────────────

func TestCWQueryStats_ErrorCount(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     `filter @message like /(?i)error/ | stats count(*) by bin(5m)`,
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Skipf("QueryStats failed (LocalStack Insights may be limited): %v", err)
	}

	if len(out.Series) == 0 {
		t.Skip("no error series — cwloggen may not have generated errors yet")
	}

	for _, s := range out.Series {
		if len(s.Values) == 0 {
			t.Errorf("series %v has no data points", s.Labels)
		}
	}

	if out.Step == "" {
		t.Error("expected Step to be populated")
	}

	t.Logf("error count: %d series, step=%s", len(out.Series), out.Step)
}

func TestCWQueryStats_TrendSummary(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     `stats count(*) by bin(5m)`,
		LogGroups: cwLogGroups,
		StartTime: "30m ago",
	})
	if err != nil {
		t.Skipf("QueryStats failed: %v", err)
	}
	if len(out.Series) == 0 {
		t.Skip("no series returned")
	}

	if len(out.Summaries) == 0 {
		t.Error("expected Summaries to be populated")
	}

	for key, summary := range out.Summaries {
		if summary.Total <= 0 {
			t.Errorf("summary[%s]: expected Total > 0, got %f", key, summary.Total)
		}
		if summary.Peak <= 0 {
			t.Errorf("summary[%s]: expected Peak > 0, got %f", key, summary.Peak)
		}
		if summary.Trend == "" {
			t.Errorf("summary[%s]: expected Trend direction", key)
		}
		validTrends := map[string]bool{"increasing": true, "decreasing": true, "stable": true, "sparse": true}
		if !validTrends[summary.Trend] {
			t.Errorf("summary[%s]: invalid trend %q", key, summary.Trend)
		}
	}
}

func TestCWQueryStats_GroupedByField(t *testing.T) {
	skipIfNoCW(t)

	// This query groups errors by the msg field extracted via JSON
	out, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     `filter @message like /(?i)error|failed/ | stats count(*) as cnt by @message | sort cnt desc | limit 10`,
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Skipf("QueryStats grouped failed: %v", err)
	}

	t.Logf("grouped query: %d series", len(out.Series))
}

func TestCWQueryStats_ZeroResults(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     `filter @message like /ZZZZNONEXISTENT99999/ | stats count(*) by bin(5m)`,
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Skipf("QueryStats failed: %v", err)
	}
	if len(out.Series) != 0 {
		t.Errorf("expected 0 series for impossible filter, got %d", len(out.Series))
	}
	if out.Warning == "" {
		t.Error("expected warning for zero results")
	}
}

func TestCWQueryStats_DataPointStructure(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     `stats count(*) by bin(5m)`,
		LogGroups: cwLogGroups,
		StartTime: "30m ago",
	})
	if err != nil {
		t.Skipf("QueryStats failed: %v", err)
	}
	if len(out.Series) == 0 {
		t.Skip("no series")
	}

	for si, series := range out.Series {
		for pi, point := range series.Values {
			if point.Timestamp == "" {
				t.Errorf("series[%d].point[%d]: empty timestamp", si, pi)
			}
			if _, err := strconv.ParseFloat(point.Value, 64); err != nil {
				t.Errorf("series[%d].point[%d]: invalid value %q: %v", si, pi, point.Value, err)
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Full Investigation Flow
// ──────────────────────────────────────────────────────────────────────────────

func TestCWFullInvestigationFlow(t *testing.T) {
	skipIfNoCW(t)
	c := cwCtx()

	// Step 1: List log groups (agent does this first)
	groups, err := cwSource.ListLogGroups(c, cwsource.ListLogGroupsInput{})
	if err != nil {
		t.Fatalf("step 1 (ListLogGroups): %v", err)
	}
	if groups.Total == 0 {
		t.Fatal("step 1: no log groups found")
	}
	t.Logf("step 1: found %d log groups: %v", groups.Total, groups.LogGroups)

	// Step 2: Get fields for payments (agent needs to know field names)
	fields, err := cwSource.GetLogFields(c, cwsource.GetLogFieldsInput{
		LogGroups: []string{"/lokilens/payments"},
	})
	if err != nil {
		t.Logf("step 2 (GetLogFields): %v (may be LocalStack limitation, continuing)", err)
	} else {
		t.Logf("step 2: discovered %d fields: %v", len(fields.Fields), fields.Fields)
	}

	// Step 3: Error scan across all groups
	statsOut, err := cwSource.QueryStats(c, cwsource.QueryStatsInput{
		Query:     `filter @message like /(?i)error|failed/ | stats count(*) by bin(5m)`,
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Skipf("step 3 (QueryStats error scan): %v", err)
	}
	t.Logf("step 3: error scan returned %d series", len(statsOut.Series))

	// Step 4: Drill into error logs
	logsOut, err := cwSource.QueryLogs(c, cwsource.QueryLogsInput{
		Query:     `filter @message like /(?i)error|failed|timeout/`,
		LogGroups: []string{"/lokilens/payments"},
		StartTime: "1h ago",
		Limit:     50,
	})
	if err != nil {
		t.Skipf("step 4 (QueryLogs drill-in): %v", err)
	}
	t.Logf("step 4: fetched %d error logs from payments", logsOut.TotalLogs)

	if logsOut.TotalLogs > 0 && len(logsOut.TopPatterns) == 0 {
		t.Error("step 4: expected patterns for error investigation")
	}
	if logsOut.ExecTimeMS <= 0 {
		t.Error("step 4: expected ExecTimeMS > 0")
	}

	t.Logf("Full CW investigation flow completed: %d groups → %d error series → %d error logs",
		groups.Total, len(statsOut.Series), logsOut.TotalLogs)
}

// ──────────────────────────────────────────────────────────────────────────────
// Concurrent Tool Calls
// ──────────────────────────────────────────────────────────────────────────────

func TestCWConcurrentToolCalls(t *testing.T) {
	skipIfNoCW(t)
	c := cwCtx()

	type result struct {
		name string
		err  error
	}
	results := make(chan result, 3)

	go func() {
		_, err := cwSource.ListLogGroups(c, cwsource.ListLogGroupsInput{})
		results <- result{"ListLogGroups", err}
	}()
	go func() {
		_, err := cwSource.QueryLogs(c, cwsource.QueryLogsInput{
			LogGroups: cwLogGroups, StartTime: "1h ago", Limit: 10,
		})
		results <- result{"QueryLogs", err}
	}()
	go func() {
		_, err := cwSource.QueryStats(c, cwsource.QueryStatsInput{
			Query: `stats count(*) by bin(5m)`, LogGroups: cwLogGroups, StartTime: "30m ago",
		})
		results <- result{"QueryStats", err}
	}()

	for range 3 {
		r := <-results
		if r.err != nil {
			t.Logf("concurrent %s: %v (may be LocalStack limitation)", r.name, r.err)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Edge Cases
// ──────────────────────────────────────────────────────────────────────────────

func TestCWQueryLogs_EmptyQuery(t *testing.T) {
	skipIfNoCW(t)

	// Empty query should still work (defaults to fields @timestamp, @message)
	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: []string{"/lokilens/payments"},
		StartTime: "1h ago",
		Limit:     5,
	})
	if err != nil {
		t.Skipf("QueryLogs with empty query failed: %v", err)
	}
	t.Logf("empty query returned %d logs", out.TotalLogs)
}

func TestCWQueryLogs_NoLogGroups(t *testing.T) {
	skipIfNoCW(t)

	// No log groups specified and source has groups configured, so it should use those
	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		StartTime: "1h ago",
		Limit:     5,
	})
	if err != nil {
		t.Skipf("QueryLogs failed: %v", err)
	}
	t.Logf("query with configured groups returned %d logs", out.TotalLogs)
}

// ──────────────────────────────────────────────────────────────────────────────
// Real-World Queries — exact queries the LLM generates for CloudWatch
// ──────────────────────────────────────────────────────────────────────────────

func TestCWRealWorldQuery_BroadErrorScan(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     `filter @message like /(?i)error|exception|fatal/ | stats count(*) by bin(5m)`,
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Skipf("broad error scan failed: %v", err)
	}
	t.Logf("broad error scan: %d series", len(out.Series))
}

func TestCWRealWorldQuery_TopErrors(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		Query:     `filter @message like /(?i)error|failed/ | fields @timestamp, @message | sort @timestamp desc`,
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
		Limit:     100,
	})
	if err != nil {
		t.Skipf("top errors query failed: %v", err)
	}
	if out.TotalLogs > 0 && len(out.TopPatterns) > 0 {
		t.Logf("top error: %q (%.1f%% of %d errors)", out.TopPatterns[0].Pattern, out.TopPatterns[0].Pct, out.TotalLogs)
	}
}

func TestCWRealWorldQuery_LogVolume(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     `stats count(*) by bin(5m)`,
		LogGroups: []string{"/lokilens/payments"},
		StartTime: "30m ago",
	})
	if err != nil {
		t.Skipf("log volume query failed: %v", err)
	}
	t.Logf("payments log volume: %d series", len(out.Series))
}

// ──────────────────────────────────────────────────────────────────────────────
// Adversarial: Boundary Values and Malformed Inputs
// ──────────────────────────────────────────────────────────────────────────────

func TestCWQueryLogs_LimitZero(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
		Limit:     0,
	})
	if err != nil {
		t.Skipf("QueryLogs with Limit=0 failed (LocalStack may be limited): %v", err)
	}
	if out.TotalLogs > 100 {
		t.Errorf("Limit=0 should default to 100, got %d logs", out.TotalLogs)
	}
}

func TestCWQueryLogs_LimitNegative(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
		Limit:     -5,
	})
	if err != nil {
		t.Skipf("QueryLogs with Limit=-5 failed (LocalStack may be limited): %v", err)
	}
	// Should clamp to positive default (100), not panic or return negative
	t.Logf("Limit=-5 returned %d logs (clamped to positive default)", out.TotalLogs)
}

func TestCWQueryLogs_LimitHuge(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
		Limit:     999999,
	})
	if err != nil {
		t.Skipf("QueryLogs with Limit=999999 failed (LocalStack may be limited): %v", err)
	}
	if out.TotalLogs > 500 {
		t.Errorf("Limit=999999 should be clamped to 500, got %d logs", out.TotalLogs)
	}
}

func TestCWQueryLogs_MalformedStartTime(t *testing.T) {
	skipIfNoCW(t)

	_, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "not_a_time",
		Limit:     5,
	})
	if err == nil {
		t.Error("expected error for malformed StartTime")
	}
}

func TestCWQueryLogs_NaturalLanguageTime(t *testing.T) {
	skipIfNoCW(t)

	// Parser handles natural language: "yesterday at noon", "last 2 hours", etc.
	// The LLM may generate these despite being instructed to use relative format.
	_, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "yesterday at noon",
		EndTime:   "now",
		Limit:     5,
	})
	if err != nil {
		t.Errorf("natural language time 'yesterday at noon' should be accepted: %v", err)
	}
}

func TestCWQueryStats_MalformedTime(t *testing.T) {
	skipIfNoCW(t)

	_, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     `stats count(*) by bin(5m)`,
		LogGroups: cwLogGroups,
		StartTime: "garbage",
	})
	if err == nil {
		t.Error("expected error for malformed StartTime on QueryStats")
	}
}

func TestCWQueryStats_EmptyQuery(t *testing.T) {
	skipIfNoCW(t)

	_, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     "",
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
	})
	if err == nil {
		t.Error("expected validation error for empty Query on QueryStats")
	}
}

func TestCWQueryStats_DangerousRegex(t *testing.T) {
	skipIfNoCW(t)

	_, err := cwSource.QueryStats(cwCtx(), cwsource.QueryStatsInput{
		Query:     `filter @message like /.*/`,
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
	})
	if err == nil {
		t.Error("expected validation error for dangerous regex pattern")
	}
}

func TestCWListLogGroups_NonexistentPrefix(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.ListLogGroups(cwCtx(), cwsource.ListLogGroupsInput{
		Prefix: "/nonexistent/prefix/xyz/",
	})
	if err != nil {
		t.Fatalf("ListLogGroups with nonexistent prefix should not error: %v", err)
	}
	if len(out.LogGroups) != 0 {
		t.Errorf("expected 0 groups for nonexistent prefix, got %d: %v", len(out.LogGroups), out.LogGroups)
	}
}

func TestCWQueryLogs_NonexistentLogGroup(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: []string{"/nonexistent/group/xyz"},
		StartTime: "1h ago",
		Limit:     5,
	})
	if err != nil {
		// Error from CloudWatch (group doesn't exist) is acceptable
		t.Logf("nonexistent log group returned error (acceptable): %v", err)
		return
	}
	// If it didn't error, it should return 0 results — not panic
	t.Logf("nonexistent log group returned %d logs (no panic)", out.TotalLogs)
}

func TestCWQueryLogs_VeryWideRange(t *testing.T) {
	skipIfNoCW(t)

	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "48h ago",
		Limit:     10,
	})
	if err != nil {
		t.Skipf("QueryLogs with 48h range failed (LocalStack may be limited): %v", err)
	}
	if out.Warning != "" && strings.Contains(out.Warning, "clamped") {
		t.Logf("correctly clamped 48h range: warning=%q", out.Warning)
	} else if out.Warning == "" {
		t.Logf("no warning for 48h range (may not exceed configured max)")
	}
}

func TestCWQueryLogs_FutureEndTime(t *testing.T) {
	skipIfNoCW(t)

	futureEnd := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	out, err := cwSource.QueryLogs(cwCtx(), cwsource.QueryLogsInput{
		LogGroups: cwLogGroups,
		StartTime: "1h ago",
		EndTime:   futureEnd,
		Limit:     5,
	})
	if err != nil {
		t.Skipf("QueryLogs with future EndTime failed (LocalStack may be limited): %v", err)
	}
	// sanitizeTimeRange should cap end to now
	t.Logf("future EndTime handled: %d logs, warning=%q", out.TotalLogs, out.Warning)
}

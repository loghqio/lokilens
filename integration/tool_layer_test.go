//go:build integration

// Package integration contains integration tests that run against a real Loki
// instance with simulated log data. These tests exercise the tool layer exactly
// as the LLM agent calls it in production — same handlers, same types, same
// analysis pipeline — but without needing a Gemini API key.
//
// Prerequisites:
//
//	docker compose -f docker/docker-compose.yml up -d loki loggen
//	# wait ~15s for log ingestion
//	go test -tags integration -v -count=1 ./integration/...
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/logsource/lokisource"
	"github.com/lokilens/lokilens/internal/safety"
)

var handlers *lokisource.ToolHandlers

// expectedServices are the services loggen produces.
var expectedServices = []string{"payments", "orders", "users", "gateway", "inventory"}

// expectedLevels are the log levels loggen produces.
var expectedLevels = []string{"info", "warn", "error", "debug"}

// expectedEnvs are the environments loggen produces.
var expectedEnvs = []string{"production", "staging"}

func TestMain(m *testing.M) {
	lokiURL := os.Getenv("LOKI_URL")
	if lokiURL == "" {
		lokiURL = "http://localhost:3100"
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
	auditLogger := audit.New(logger)

	lokiClient := loki.NewHTTPClient(loki.ClientConfig{
		BaseURL:    lokiURL,
		Timeout:    30 * time.Second,
		MaxRetries: 2,
		Logger:     logger,
	})

	validator := safety.NewValidator(24*time.Hour, 500)
	handlers = lokisource.NewToolHandlers(lokiClient, validator, auditLogger)

	os.Exit(m.Run())
}

func ctx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = cancel // Tests are short-lived; GC handles cleanup
	return c
}

// ──────────────────────────────────────────────────────────────────────────────
// Health Check
// ──────────────────────────────────────────────────────────────────────────────

func TestHealthCheck(t *testing.T) {
	lokiURL := os.Getenv("LOKI_URL")
	if lokiURL == "" {
		lokiURL = "http://localhost:3100"
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	auditLogger := audit.New(logger)
	lokiClient := loki.NewHTTPClient(loki.ClientConfig{
		BaseURL: lokiURL, Timeout: 10 * time.Second, Logger: logger,
	})
	validator := safety.NewValidator(24*time.Hour, 500)
	source := lokisource.New(lokiClient, validator, auditLogger)

	if err := source.HealthCheck(ctx()); err != nil {
		t.Fatalf("HealthCheck failed — is Loki running at %s? Error: %v", lokiURL, err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Discovery: GetLabels
// ──────────────────────────────────────────────────────────────────────────────

func TestGetLabels(t *testing.T) {
	out, err := handlers.GetLabels(ctx(), lokisource.GetLabelsInput{})
	if err != nil {
		t.Fatalf("GetLabels failed: %v", err)
	}

	required := []string{"service", "level", "env", "job"}
	for _, want := range required {
		if !contains(out.Labels, want) {
			t.Errorf("expected label %q in %v", want, out.Labels)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Discovery: GetLabelValues
// ──────────────────────────────────────────────────────────────────────────────

func TestGetLabelValues_Service(t *testing.T) {
	out, err := handlers.GetLabelValues(ctx(), lokisource.GetLabelValuesInput{
		LabelName: "service",
	})
	if err != nil {
		t.Fatalf("GetLabelValues failed: %v", err)
	}
	for _, want := range expectedServices {
		if !contains(out.Values, want) {
			t.Errorf("expected service %q in %v", want, out.Values)
		}
	}
}

func TestGetLabelValues_Level(t *testing.T) {
	out, err := handlers.GetLabelValues(ctx(), lokisource.GetLabelValuesInput{
		LabelName: "level",
	})
	if err != nil {
		t.Fatalf("GetLabelValues failed: %v", err)
	}
	for _, want := range expectedLevels {
		if !contains(out.Values, want) {
			t.Errorf("expected level %q in %v", want, out.Values)
		}
	}
}

func TestGetLabelValues_Env(t *testing.T) {
	out, err := handlers.GetLabelValues(ctx(), lokisource.GetLabelValuesInput{
		LabelName: "env",
	})
	if err != nil {
		t.Fatalf("GetLabelValues failed: %v", err)
	}
	for _, want := range expectedEnvs {
		if !contains(out.Values, want) {
			t.Errorf("expected env %q in %v", want, out.Values)
		}
	}
}

func TestGetLabelValues_EmptyLabel(t *testing.T) {
	_, err := handlers.GetLabelValues(ctx(), lokisource.GetLabelValuesInput{
		LabelName: "",
	})
	if err == nil {
		t.Error("expected error for empty label name")
	}
}

func TestGetLabelValues_NonexistentLabel(t *testing.T) {
	out, err := handlers.GetLabelValues(ctx(), lokisource.GetLabelValuesInput{
		LabelName: "nonexistent_label_xyz",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Values) != 0 {
		t.Errorf("expected empty values for nonexistent label, got %v", out.Values)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// QueryLogs: Basic Queries
// ──────────────────────────────────────────────────────────────────────────────

func TestQueryLogs_AllLogs(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs == 0 {
		t.Fatal("expected logs from loggen, got 0")
	}

	// Verify structural integrity of every log entry
	for i, entry := range out.Logs {
		if entry.Timestamp == "" {
			t.Errorf("log[%d]: empty timestamp", i)
		}
		if entry.Line == "" {
			t.Errorf("log[%d]: empty line", i)
		}
		if entry.Labels == nil {
			t.Errorf("log[%d]: nil labels", i)
		}
	}

	// Verify output metadata
	if out.Query == "" {
		t.Error("Query field should be populated")
	}
	if out.TimeRange == "" {
		t.Error("TimeRange field should be populated")
	}
	if out.Direction == "" {
		t.Error("Direction field should be populated")
	}
}

func TestQueryLogs_ErrorsOnly(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{level="error", job="loggen"}`,
		StartTime: "1h ago",
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}

	if out.TotalLogs == 0 {
		t.Skip("no error logs found — possible if loggen just started and hasn't hit the 10% error rate yet")
	}

	// Every returned log should have level=error
	for i, entry := range out.Logs {
		if entry.Labels["level"] != "error" {
			t.Errorf("log[%d]: expected level=error, got level=%s", i, entry.Labels["level"])
		}
	}
}

func TestQueryLogs_SpecificService(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{service="payments", job="loggen"}`,
		StartTime: "1h ago",
		Limit:     20,
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs == 0 {
		t.Fatal("expected logs for payments service")
	}

	for i, entry := range out.Logs {
		if entry.Labels["service"] != "payments" {
			t.Errorf("log[%d]: expected service=payments, got %s", i, entry.Labels["service"])
		}
	}
}

func TestQueryLogs_RegexFilter(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"} |~ "(?i)timeout|connection"`,
		StartTime: "1h ago",
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}

	// Every returned log should contain timeout or connection (case-insensitive)
	for i, entry := range out.Logs {
		lower := strings.ToLower(entry.Line)
		if !strings.Contains(lower, "timeout") && !strings.Contains(lower, "connection") {
			t.Errorf("log[%d]: expected 'timeout' or 'connection' in line, got: %s", i, truncate(entry.Line, 100))
		}
	}
}

func TestQueryLogs_JSONParsing(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs == 0 {
		t.Fatal("expected logs")
	}

	// loggen produces JSON logs — verify they're complete JSON-like strings
	for i, entry := range out.Logs {
		if !strings.Contains(entry.Line, `"msg"`) {
			t.Errorf("log[%d]: expected JSON log with 'msg' field, got: %s", i, truncate(entry.Line, 100))
		}
		if !strings.Contains(entry.Line, `"service"`) {
			t.Errorf("log[%d]: expected JSON log with 'service' field", i)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// QueryLogs: Pattern Analysis
// ──────────────────────────────────────────────────────────────────────────────

func TestQueryLogs_PatternAnalysis(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     200,
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs < 10 {
		t.Skip("need at least 10 logs for meaningful pattern analysis")
	}

	if len(out.TopPatterns) == 0 {
		t.Fatal("expected TopPatterns to be populated")
	}
	if out.TotalPatterns == 0 {
		t.Fatal("expected TotalPatterns > 0")
	}

	// Verify pattern structure
	totalPct := 0.0
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
		if p.Sample == "" {
			t.Error("pattern sample should not be empty")
		}
		totalPct += p.Pct
	}

	// Top patterns should account for a significant portion of logs
	if totalPct < 20 {
		t.Errorf("top patterns account for only %.1f%% of logs — analysis may be broken", totalPct)
	}
}

func TestQueryLogs_LabelDistribution(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     200,
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs < 10 {
		t.Skip("need at least 10 logs for label distribution")
	}

	if out.UniqueLabels == nil {
		t.Fatal("expected UniqueLabels to be populated")
	}

	// Should have service distribution
	serviceDist, ok := out.UniqueLabels["service"]
	if !ok {
		t.Fatal("expected 'service' in UniqueLabels")
	}

	// At least one service should appear
	if len(serviceDist) == 0 {
		t.Error("empty service distribution")
	}

	// Counts should be positive
	for svc, count := range serviceDist {
		if count <= 0 {
			t.Errorf("service %q has invalid count %d", svc, count)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// QueryLogs: Direction and Limits
// ──────────────────────────────────────────────────────────────────────────────

func TestQueryLogs_DirectionBackward(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     20,
		Direction: "backward",
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs < 2 {
		t.Skip("need at least 2 logs to verify ordering")
	}

	if out.Direction != "backward" {
		t.Errorf("expected direction=backward, got %s", out.Direction)
	}

	// Timestamps should be in descending order (newest first)
	for i := 1; i < len(out.Logs); i++ {
		if out.Logs[i].Timestamp > out.Logs[i-1].Timestamp {
			t.Errorf("backward: log[%d] (%s) is after log[%d] (%s)",
				i, out.Logs[i].Timestamp, i-1, out.Logs[i-1].Timestamp)
			break
		}
	}
}

func TestQueryLogs_DirectionForward(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     20,
		Direction: "forward",
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs < 2 {
		t.Skip("need at least 2 logs to verify ordering")
	}

	if out.Direction != "forward" {
		t.Errorf("expected direction=forward, got %s", out.Direction)
	}

	// Timestamps should be in ascending order (oldest first)
	for i := 1; i < len(out.Logs); i++ {
		if out.Logs[i].Timestamp < out.Logs[i-1].Timestamp {
			t.Errorf("forward: log[%d] (%s) is before log[%d] (%s)",
				i, out.Logs[i].Timestamp, i-1, out.Logs[i-1].Timestamp)
			break
		}
	}
}

func TestQueryLogs_Limit(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs > 5 {
		t.Errorf("expected at most 5 logs, got %d", out.TotalLogs)
	}
}

func TestQueryLogs_DefaultLimit(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		// Limit not set — should default to 100
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs > 100 {
		t.Errorf("default limit should cap at 100, got %d", out.TotalLogs)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// QueryLogs: Zero Results and Warnings
// ──────────────────────────────────────────────────────────────────────────────

func TestQueryLogs_ZeroResults(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{service="nonexistent_service_xyz_12345"}`,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Fatalf("QueryLogs failed: %v", err)
	}
	if out.TotalLogs != 0 {
		t.Errorf("expected 0 logs for nonexistent service, got %d", out.TotalLogs)
	}
	if out.Warning == "" {
		t.Error("expected warning for zero results")
	}
	if !strings.Contains(out.Warning, "ZERO RESULTS") {
		t.Errorf("expected ZERO RESULTS in warning, got: %s", truncate(out.Warning, 100))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// QueryStats: Time Series
// ──────────────────────────────────────────────────────────────────────────────

func TestQueryStats_ErrorCount(t *testing.T) {
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `count_over_time({level="error", job="loggen"}[1m])`,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Fatalf("QueryStats failed: %v", err)
	}

	if len(out.Series) == 0 {
		t.Skip("no error series — loggen may not have generated errors yet")
	}

	// Should have data points
	for _, s := range out.Series {
		if len(s.Values) == 0 {
			t.Errorf("series %v has no data points", s.Labels)
		}
	}

	// Verify Step is populated
	if out.Step == "" {
		t.Error("expected Step to be auto-selected")
	}

	if out.Query == "" {
		t.Error("expected Query to be populated")
	}
}

func TestQueryStats_ByService(t *testing.T) {
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum by (service)(count_over_time({job="loggen"}[5m]))`,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Fatalf("QueryStats failed: %v", err)
	}

	if out.TotalSeries == 0 {
		t.Fatal("expected multiple series (one per service)")
	}

	// Should have multiple series corresponding to different services
	seenServices := make(map[string]bool)
	for _, s := range out.Series {
		if svc, ok := s.Labels["service"]; ok {
			seenServices[svc] = true
		}
	}

	if len(seenServices) < 2 {
		t.Errorf("expected multiple services in series, got %d: %v", len(seenServices), seenServices)
	}
}

func TestQueryStats_TrendSummary(t *testing.T) {
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum(count_over_time({job="loggen"}[1m]))`,
		StartTime: "30m ago",
		Step:      "1m",
	})
	if err != nil {
		t.Fatalf("QueryStats failed: %v", err)
	}
	if len(out.Series) == 0 {
		t.Fatal("expected at least one series")
	}

	if len(out.Summaries) == 0 {
		t.Fatal("expected Summaries to be populated")
	}

	// Check summary fields for the first series
	for key, summary := range out.Summaries {
		if summary.Total <= 0 {
			t.Errorf("summary[%s]: expected Total > 0, got %f", key, summary.Total)
		}
		if summary.AvgPerMinute <= 0 {
			t.Errorf("summary[%s]: expected AvgPerMinute > 0, got %f", key, summary.AvgPerMinute)
		}
		if summary.Peak <= 0 {
			t.Errorf("summary[%s]: expected Peak > 0, got %f", key, summary.Peak)
		}
		if summary.PeakTime == "" {
			t.Errorf("summary[%s]: expected PeakTime to be set", key)
		}
		if summary.Trend == "" {
			t.Errorf("summary[%s]: expected Trend direction", key)
		}
		validTrends := map[string]bool{"increasing": true, "decreasing": true, "stable": true, "sparse": true}
		if !validTrends[summary.Trend] {
			t.Errorf("summary[%s]: invalid trend %q", key, summary.Trend)
		}
		if summary.NonZeroPct < 0 || summary.NonZeroPct > 100 {
			t.Errorf("summary[%s]: NonZeroPct out of range: %f", key, summary.NonZeroPct)
		}
	}
}

func TestQueryStats_AutoStep(t *testing.T) {
	// 30 minute range should auto-select "30s" step
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum(count_over_time({job="loggen"}[1m]))`,
		StartTime: "30m ago",
		// Step intentionally empty
	})
	if err != nil {
		t.Fatalf("QueryStats failed: %v", err)
	}
	if out.Step == "" {
		t.Error("expected auto-selected step")
	}
	// For 30m range, step should be "30s" or "1m"
	if out.Step != "30s" && out.Step != "1m" {
		t.Errorf("expected step 30s or 1m for 30m range, got %s", out.Step)
	}
}

func TestQueryStats_ExplicitStep(t *testing.T) {
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum(count_over_time({job="loggen"}[5m]))`,
		StartTime: "1h ago",
		Step:      "5m",
	})
	if err != nil {
		t.Fatalf("QueryStats failed: %v", err)
	}
	if out.Step != "5m" {
		t.Errorf("expected step=5m, got %s", out.Step)
	}
}

func TestQueryStats_ZeroResults(t *testing.T) {
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum(count_over_time({service="nonexistent_xyz_99999"}[1m]))`,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Fatalf("QueryStats failed: %v", err)
	}
	if len(out.Series) != 0 {
		t.Errorf("expected 0 series for nonexistent service, got %d", len(out.Series))
	}
	if out.Warning == "" {
		t.Error("expected warning for zero results")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// QueryStats: Data Point Validation
// ──────────────────────────────────────────────────────────────────────────────

func TestQueryStats_DataPointStructure(t *testing.T) {
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum(count_over_time({job="loggen"}[5m]))`,
		StartTime: "1h ago",
		Step:      "5m",
	})
	if err != nil {
		t.Fatalf("QueryStats failed: %v", err)
	}
	if len(out.Series) == 0 {
		t.Skip("no series returned — log ingestion may still be in progress")
	}

	for si, series := range out.Series {
		for pi, point := range series.Values {
			// Timestamp should be parseable
			if _, err := time.Parse(time.RFC3339, point.Timestamp); err != nil {
				t.Errorf("series[%d].point[%d]: invalid timestamp %q: %v", si, pi, point.Timestamp, err)
			}
			// Value should be a valid number
			if _, err := strconv.ParseFloat(point.Value, 64); err != nil {
				t.Errorf("series[%d].point[%d]: invalid value %q: %v", si, pi, point.Value, err)
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Full Investigation Flow — mimics how the agent calls tools in production
// ──────────────────────────────────────────────────────────────────────────────

func TestFullInvestigationFlow(t *testing.T) {
	c := ctx()

	// Step 1: Discover labels (agent always does this first)
	labels, err := handlers.GetLabels(c, lokisource.GetLabelsInput{})
	if err != nil {
		t.Fatalf("step 1 (GetLabels): %v", err)
	}

	// Find the service and level label names
	hasService := contains(labels.Labels, "service")
	hasLevel := contains(labels.Labels, "level")
	if !hasService {
		t.Fatal("step 1: expected 'service' label")
	}
	if !hasLevel {
		t.Fatal("step 1: expected 'level' label")
	}

	// Step 2: Get service values (agent needs to know what services exist)
	services, err := handlers.GetLabelValues(c, lokisource.GetLabelValuesInput{
		LabelName: "service",
	})
	if err != nil {
		t.Fatalf("step 2 (GetLabelValues service): %v", err)
	}
	if len(services.Values) == 0 {
		t.Fatal("step 2: no services found")
	}
	t.Logf("step 2: found %d services: %v", len(services.Values), services.Values)

	// Step 3: Get level values (agent needs exact level strings)
	levels, err := handlers.GetLabelValues(c, lokisource.GetLabelValuesInput{
		LabelName: "level",
	})
	if err != nil {
		t.Fatalf("step 3 (GetLabelValues level): %v", err)
	}
	if len(levels.Values) == 0 {
		t.Fatal("step 3: no levels found")
	}

	// Determine the error level value (could be "error", "ERROR", etc.)
	errorLevel := ""
	for _, v := range levels.Values {
		if strings.EqualFold(v, "error") {
			errorLevel = v
			break
		}
	}
	if errorLevel == "" {
		t.Fatal("step 3: no error-like level found")
	}

	// Step 4: Multi-service error scan (agent's typical first investigation query)
	statsOut, err := handlers.QueryStats(c, lokisource.QueryStatsInput{
		LogQL:     fmt.Sprintf(`sum by (service)(count_over_time({level="%s"}[5m]))`, errorLevel),
		StartTime: "1h ago",
	})
	if err != nil {
		t.Fatalf("step 4 (QueryStats error scan): %v", err)
	}
	if statsOut.TotalSeries == 0 {
		t.Skip("step 4: no error data — loggen may not have generated errors yet")
	}
	t.Logf("step 4: found %d series across services", statsOut.TotalSeries)

	// Find the noisiest service (highest total errors)
	var noisiest string
	var highestTotal float64
	for key, summary := range statsOut.Summaries {
		if summary.Total > highestTotal {
			highestTotal = summary.Total
			noisiest = key
		}
	}
	t.Logf("step 4: noisiest = %s (total=%.0f, avg/min=%.2f, trend=%s)",
		noisiest, highestTotal,
		statsOut.Summaries[noisiest].AvgPerMinute,
		statsOut.Summaries[noisiest].Trend)

	// Extract service name from the series key (e.g. "service=payments")
	targetService := ""
	for _, s := range statsOut.Series {
		if svc, ok := s.Labels["service"]; ok {
			found := false
			for k, summary := range statsOut.Summaries {
				if strings.Contains(k, svc) && summary.Total == highestTotal {
					found = true
					break
				}
			}
			if found {
				targetService = svc
				break
			}
		}
	}
	if targetService == "" && len(services.Values) > 0 {
		targetService = services.Values[0] // fallback
	}

	// Step 5: Drill into the noisiest service (agent fetches actual error logs)
	logsOut, err := handlers.QueryLogs(c, lokisource.QueryLogsInput{
		LogQL:     fmt.Sprintf(`{service="%s", level="%s"}`, targetService, errorLevel),
		StartTime: "1h ago",
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("step 5 (QueryLogs drill-in): %v", err)
	}
	t.Logf("step 5: fetched %d error logs from %s", logsOut.TotalLogs, targetService)

	if logsOut.TotalLogs == 0 {
		t.Fatalf("step 5: expected error logs for service=%s, got 0", targetService)
	}

	// Verify the drill-in returned proper analysis
	if len(logsOut.TopPatterns) == 0 {
		t.Error("step 5: expected patterns for error investigation")
	}
	if logsOut.ExecTimeMS <= 0 {
		t.Error("step 5: expected ExecTimeMS > 0")
	}

	// Step 6: Verify all returned logs are for the target service and error level
	for i, entry := range logsOut.Logs {
		if entry.Labels["service"] != targetService {
			t.Errorf("step 6: log[%d] service=%s, expected %s", i, entry.Labels["service"], targetService)
		}
		if entry.Labels["level"] != errorLevel {
			t.Errorf("step 6: log[%d] level=%s, expected %s", i, entry.Labels["level"], errorLevel)
		}
	}

	t.Logf("Full investigation flow completed successfully: %d labels → %d services → %d error series → %d error logs with %d patterns",
		len(labels.Labels), len(services.Values), statsOut.TotalSeries, logsOut.TotalLogs, len(logsOut.TopPatterns))
}

// ──────────────────────────────────────────────────────────────────────────────
// Concurrent Tool Calls — the agent often fires multiple tools in parallel
// ──────────────────────────────────────────────────────────────────────────────

func TestConcurrentToolCalls(t *testing.T) {
	c := ctx()

	type result struct {
		name string
		err  error
	}
	results := make(chan result, 4)

	// Fire 4 tool calls in parallel (agent does this for service health checks)
	go func() {
		_, err := handlers.GetLabels(c, lokisource.GetLabelsInput{})
		results <- result{"GetLabels", err}
	}()
	go func() {
		_, err := handlers.GetLabelValues(c, lokisource.GetLabelValuesInput{LabelName: "service"})
		results <- result{"GetLabelValues(service)", err}
	}()
	go func() {
		_, err := handlers.QueryLogs(c, lokisource.QueryLogsInput{
			LogQL: `{job="loggen"}`, StartTime: "1h ago", Limit: 10,
		})
		results <- result{"QueryLogs", err}
	}()
	go func() {
		_, err := handlers.QueryStats(c, lokisource.QueryStatsInput{
			LogQL: `sum(count_over_time({job="loggen"}[1m]))`, StartTime: "30m ago",
		})
		results <- result{"QueryStats", err}
	}()

	for range 4 {
		r := <-results
		if r.err != nil {
			t.Errorf("concurrent %s failed: %v", r.name, r.err)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Edge Cases
// ──────────────────────────────────────────────────────────────────────────────

func TestQueryLogs_InvalidLogQL(t *testing.T) {
	_, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `invalid query syntax {{{`,
		StartTime: "1h ago",
	})
	if err == nil {
		t.Error("expected error for invalid LogQL")
	}
}

func TestQueryLogs_EmptyQuery(t *testing.T) {
	_, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     "",
		StartTime: "1h ago",
	})
	if err == nil {
		t.Error("expected error for empty query")
	}
}

func TestQueryStats_InvalidLogQL(t *testing.T) {
	_, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `not a valid query`,
		StartTime: "1h ago",
	})
	if err == nil {
		t.Error("expected error for invalid LogQL")
	}
}

func TestQueryLogs_SwappedTimeRange(t *testing.T) {
	// Handler should handle swapped times gracefully (clamp + warn)
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "now",
		EndTime:   "1h ago",
		Limit:     10,
	})
	if err != nil {
		// Validator may reject swapped times — that's also acceptable
		t.Logf("swapped time range returned error (acceptable): %v", err)
		return
	}
	// If it succeeded, it should have swapped and warned
	if out.Warning != "" {
		t.Logf("swapped time range handled with warning: %s", out.Warning)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Real-World Queries — the exact queries the LLM generates
// ──────────────────────────────────────────────────────────────────────────────

func TestRealWorldQuery_BroadErrorScan(t *testing.T) {
	// "Any issues?" → agent generates this
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum by (service)(count_over_time({level="error"}[5m]))`,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Fatalf("broad error scan failed: %v", err)
	}
	t.Logf("broad error scan: %d series, %d summaries", out.TotalSeries, len(out.Summaries))
}

func TestRealWorldQuery_ServiceHealth(t *testing.T) {
	// "Is payments running?" → agent generates log volume query
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum(count_over_time({service="payments"}[5m]))`,
		StartTime: "30m ago",
		Step:      "5m",
	})
	if err != nil {
		t.Fatalf("service health query failed: %v", err)
	}
	if len(out.Series) == 0 {
		t.Error("expected log volume data for payments")
	}
}

func TestRealWorldQuery_TopErrors(t *testing.T) {
	// "What are the top errors?" → agent fetches error logs with large limit
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{level="error", job="loggen"}`,
		StartTime: "1h ago",
		Limit:     200,
	})
	if err != nil {
		t.Fatalf("top errors query failed: %v", err)
	}
	if out.TotalLogs == 0 {
		t.Skip("no errors yet")
	}
	if len(out.TopPatterns) == 0 {
		t.Error("expected TopPatterns for error investigation")
	}
	// The dominant pattern should have a percentage
	if out.TopPatterns[0].Pct <= 0 {
		t.Error("expected top pattern to have a percentage > 0")
	}
	t.Logf("top error: %q (%.1f%% of %d errors)", out.TopPatterns[0].Pattern, out.TopPatterns[0].Pct, out.TotalLogs)
}

func TestRealWorldQuery_TopErrorServices(t *testing.T) {
	// "Which service has the most errors?" → topk query
	out, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `topk(5, sum by (service)(count_over_time({level="error"}[1h])))`,
		StartTime: "1h ago",
	})
	if err != nil {
		t.Fatalf("top error services query failed: %v", err)
	}
	t.Logf("top error services: %d series", out.TotalSeries)
}

func TestRealWorldQuery_ErrorRateComparison(t *testing.T) {
	// "Is it getting worse?" → agent compares two time periods
	recent, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum(count_over_time({level="error"}[1m]))`,
		StartTime: "15m ago",
		Step:      "1m",
	})
	if err != nil {
		t.Fatalf("recent error rate failed: %v", err)
	}

	older, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum(count_over_time({level="error"}[1m]))`,
		StartTime: "30m ago",
		EndTime:   "15m ago",
		Step:      "1m",
	})
	if err != nil {
		t.Fatalf("older error rate failed: %v", err)
	}

	// Both queries should succeed (may have 0 data if loggen just started)
	t.Logf("comparison: recent=%d series, older=%d series", recent.TotalSeries, older.TotalSeries)
}

func TestRealWorldQuery_MultiServiceErrorsWithLabels(t *testing.T) {
	// Complex multi-label query agent generates for root cause analysis
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{level="error", job="loggen"} |~ "(?i)timeout|refused|503"`,
		StartTime: "1h ago",
		Limit:     100,
	})
	if err != nil {
		t.Fatalf("multi-service error query failed: %v", err)
	}
	t.Logf("multi-service timeout/refused/503 errors: %d logs, %d patterns", out.TotalLogs, out.TotalPatterns)
}

// ──────────────────────────────────────────────────────────────────────────────
// Adversarial: Boundary Values and Malformed Inputs
// ──────────────────────────────────────────────────────────────────────────────

func TestQueryLogs_LimitZero(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     0,
	})
	if err != nil {
		t.Fatalf("QueryLogs with Limit=0 failed: %v", err)
	}
	if out.TotalLogs > 100 {
		t.Errorf("Limit=0 should default to 100, got %d logs", out.TotalLogs)
	}
	if out.TotalLogs == 0 {
		t.Error("expected logs from loggen (Limit=0 should default to 100, not 0)")
	}
}

func TestQueryLogs_LimitNegative(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     -1,
	})
	if err != nil {
		t.Fatalf("QueryLogs with Limit=-1 failed: %v", err)
	}
	if out.TotalLogs > 1 {
		t.Errorf("Limit=-1 should be clamped to 1, got %d logs", out.TotalLogs)
	}
}

func TestQueryLogs_LimitHuge(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Limit:     999999,
	})
	if err != nil {
		t.Fatalf("QueryLogs with Limit=999999 should not error: %v", err)
	}
	if out.TotalLogs > 500 {
		t.Errorf("Limit=999999 should be clamped to MaxResults (500), got %d logs", out.TotalLogs)
	}
}

func TestQueryLogs_InvalidDirection(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Direction: "NONSENSE",
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("QueryLogs with Direction=NONSENSE failed: %v", err)
	}
	if out.Direction != "backward" {
		t.Errorf("Direction=NONSENSE should default to backward, got %q", out.Direction)
	}
}

func TestQueryLogs_UppercaseDirection(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "1h ago",
		Direction: "BACKWARD",
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("QueryLogs with Direction=BACKWARD failed: %v", err)
	}
	if out.Direction != "backward" {
		t.Errorf("Direction=BACKWARD should be treated as backward, got %q", out.Direction)
	}
}

func TestQueryLogs_MalformedStartTime(t *testing.T) {
	_, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "not a real time",
		Limit:     5,
	})
	if err == nil {
		t.Error("expected error for malformed StartTime")
	}
}

func TestQueryLogs_NaturalLanguageTime(t *testing.T) {
	// Parser handles natural language: "yesterday", "last 2 hours", "yesterday at noon", etc.
	// The LLM may generate these despite being instructed to use relative format.
	_, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "yesterday",
		EndTime:   "now",
		Limit:     5,
	})
	if err != nil {
		t.Errorf("natural language time 'yesterday' should be accepted: %v", err)
	}
}

func TestQueryStats_MalformedStartTime(t *testing.T) {
	_, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     `sum(count_over_time({job="loggen"}[1m]))`,
		StartTime: "garbage",
	})
	if err == nil {
		t.Error("expected error for malformed StartTime on QueryStats")
	}
}

func TestQueryLogs_FutureTimeRange(t *testing.T) {
	futureEnd := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "now",
		EndTime:   futureEnd,
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("QueryLogs with future EndTime should not error: %v", err)
	}
	// clampTimeRange caps end to now, then defaults to 1h range
	t.Logf("future time range handled: %d logs, warning=%q", out.TotalLogs, out.Warning)
}

func TestQueryLogs_VeryWideRange(t *testing.T) {
	out, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
		LogQL:     `{job="loggen"}`,
		StartTime: "48h ago",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("QueryLogs with 48h range should not error: %v", err)
	}
	if out.Warning != "" && strings.Contains(out.Warning, "clamped") {
		t.Logf("correctly clamped 48h range: warning=%q", out.Warning)
	} else if out.Warning == "" {
		t.Logf("no warning for 48h range (may not exceed configured max)")
	}
}

func TestQueryStats_EmptyLogQL(t *testing.T) {
	_, err := handlers.QueryStats(ctx(), lokisource.QueryStatsInput{
		LogQL:     "",
		StartTime: "1h ago",
	})
	if err == nil {
		t.Error("expected validation error for empty LogQL on QueryStats")
	}
}

func TestGetLabelValues_SpecialChars(t *testing.T) {
	// Should not panic — just return empty or error gracefully
	out, err := handlers.GetLabelValues(ctx(), lokisource.GetLabelValuesInput{
		LabelName: "label/with/slashes",
	})
	if err != nil {
		t.Logf("special chars label returned error (acceptable): %v", err)
		return
	}
	if len(out.Values) != 0 {
		t.Errorf("expected empty values for label with special chars, got %v", out.Values)
	}
}

func TestConcurrentHeavyLoad(t *testing.T) {
	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, err := handlers.QueryLogs(ctx(), lokisource.QueryLogsInput{
				LogQL:     `{job="loggen"}`,
				StartTime: "1h ago",
				Limit:     10,
			})
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", id, err)
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent heavy load failure: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// sortedKeys returns sorted keys of a string map for deterministic output.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

package lokisource

import (
	"fmt"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/safety"
)

// LokiSource implements logsource.LogSource for Grafana Loki.
type LokiSource struct {
	handlers *ToolHandlers
}

// New creates a LokiSource with the given dependencies.
func New(lokiClient loki.Client, validator *safety.Validator, auditLogger *audit.Logger) *LokiSource {
	return &LokiSource{
		handlers: NewToolHandlers(lokiClient, validator, auditLogger),
	}
}

func (s *LokiSource) Name() string        { return "Loki" }
func (s *LokiSource) Instruction() string  { return systemInstruction }
func (s *LokiSource) Description() string {
	return "Log analysis assistant that queries Grafana Loki via natural language"
}

func (s *LokiSource) Tools() ([]tool.Tool, error) {
	queryLogsTool, err := functiontool.New(functiontool.Config{
		Name:        "query_logs",
		Description: "Fetch raw log lines from Loki with automatic pattern analysis. Returns individual log entries plus top_patterns (grouped similar lines with counts and pct — percentage of total), total_patterns (count of all distinct patterns — report this when present: '47 distinct error types found'), unique_labels (nested label distribution, e.g. {\"service\": {\"payments\": 45, \"orders\": 12}}), and direction ('backward'=newest first, 'forward'=oldest first — use this to correctly describe timeline order). Use this when you need actual log messages, error details, or stack traces. The top_patterns field lets you immediately identify the dominant error type without manually reading every line — use the pct field to say e.g. '78% of errors are timeouts'. unique_labels shows which services/levels dominate the results. NOT for counting or aggregation over time — use query_stats for that.",
	}, func(ctx tool.Context, input QueryLogsInput) (agent.QueryLogsOutput, error) {
		return s.handlers.QueryLogs(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating query_logs tool: %w", err)
	}

	getLabelsTool, err := functiontool.New(functiontool.Config{
		Name:        "get_labels",
		Description: "List all label names in Loki (e.g. service, level, namespace, env). Call this FIRST in any new conversation before building LogQL queries — the returned list tells you which labels exist so you can identify the service label, the log level label, and the environment label. This is a single lightweight Loki API call. Use the results to pick the right label names, then call get_label_values to see the actual values.",
	}, func(ctx tool.Context, input GetLabelsInput) (GetLabelsOutput, error) {
		return s.handlers.GetLabels(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating get_labels tool: %w", err)
	}

	getLabelValuesTool, err := functiontool.New(functiontool.Config{
		Name:        "get_label_values",
		Description: "Get all values for a specific label (e.g. all service names, all environments, all log levels). After calling get_labels to learn label names, call this to discover: (1) all service names — pass the service label, (2) all log level values — pass the level label to see exact strings like 'error' or 'ERROR', (3) all environments — pass the environment label. Call this in parallel for multiple labels to save time. Essential for building correct LogQL queries — using the wrong label value returns 0 results.",
	}, func(ctx tool.Context, input GetLabelValuesInput) (GetLabelValuesOutput, error) {
		return s.handlers.GetLabelValues(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating get_label_values tool: %w", err)
	}

	queryStatsTool, err := functiontool.New(functiontool.Config{
		Name:        "query_stats",
		Description: "Run aggregation queries to get counts, rates, and trends over time with automatic trend analysis. Returns time-series data plus total_series (number of series — 0 means no matching data), step (the time granularity, e.g. '1m' or '5m'), and a summaries map with pre-computed total, avg (average count per step interval), avg_per_minute (already normalized to per-minute — use this directly for user-facing rates, no math needed), latest value, peak, peak_time (when the peak occurred), trend direction (increasing/decreasing/stable/sparse), and non_zero_pct for each series. Use this for 'how many errors?', 'error rate trend', 'compare periods', 'which service has the most errors?', 'is there a spike?', 'is it getting worse?'. The summaries field gives you instant verdicts without parsing individual data points — use avg_per_minute for 'what's the error rate', peak+peak_time for 'when was the worst point'. If total_series is 0, the query found no matching data. NOT for raw logs — use query_logs for actual log lines.",
	}, func(ctx tool.Context, input QueryStatsInput) (agent.QueryStatsOutput, error) {
		return s.handlers.QueryStats(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating query_stats tool: %w", err)
	}

	return []tool.Tool{queryLogsTool, getLabelsTool, getLabelValuesTool, queryStatsTool}, nil
}

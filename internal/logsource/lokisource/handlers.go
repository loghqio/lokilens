package lokisource

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/safety"
)

// Loki-specific tool input types

type QueryLogsInput struct {
	LogQL     string `json:"logql" jsonschema_description:"LogQL query string with stream selector and optional filters"`
	StartTime string `json:"start_time" jsonschema_description:"Start time as relative (e.g. 2h ago or 30m ago) or RFC3339. Defaults to 1h ago"`
	EndTime   string `json:"end_time,omitempty" jsonschema_description:"End time as relative or RFC3339. Defaults to now"`
	Limit     int    `json:"limit,omitempty" jsonschema_description:"Max log lines to return. Default 100 and max 500"`
	Direction string `json:"direction,omitempty" jsonschema_description:"Sort order: backward (newest first) or forward (oldest first). Default backward"`
}

type GetLabelsInput struct {
	StartTime string `json:"start_time,omitempty" jsonschema_description:"Start time. Defaults to 6h ago"`
	EndTime   string `json:"end_time,omitempty" jsonschema_description:"End time. Defaults to now"`
}

type GetLabelsOutput struct {
	Labels []string `json:"labels"`
}

type GetLabelValuesInput struct {
	LabelName string `json:"label_name" jsonschema_description:"The label to get values for (e.g. service or level)"`
	StartTime string `json:"start_time,omitempty" jsonschema_description:"Start time. Defaults to 6h ago"`
	EndTime   string `json:"end_time,omitempty" jsonschema_description:"End time. Defaults to now"`
}

type GetLabelValuesOutput struct {
	LabelName string   `json:"label_name"`
	Values    []string `json:"values"`
}

type QueryStatsInput struct {
	LogQL     string `json:"logql" jsonschema_description:"LogQL metric query for aggregated statistics"`
	StartTime string `json:"start_time" jsonschema_description:"Start time as relative or RFC3339"`
	EndTime   string `json:"end_time,omitempty" jsonschema_description:"End time. Defaults to now"`
	Step      string `json:"step,omitempty" jsonschema_description:"Query resolution step (e.g. 1m or 5m). Leave empty to auto-select based on time range (30s for ≤30m, 1m for ≤2h, 5m for ≤6h, 15m for ≤12h, 1h for 24h)"`
}

// ToolHandlers holds the tool handler functions bound to a Loki client and validator.
type ToolHandlers struct {
	lokiClient loki.Client
	validator  *safety.Validator
	audit      *audit.Logger
}

// NewToolHandlers creates tool handlers with the given dependencies.
func NewToolHandlers(lokiClient loki.Client, validator *safety.Validator, auditLogger *audit.Logger) *ToolHandlers {
	return &ToolHandlers{
		lokiClient: lokiClient,
		validator:  validator,
		audit:      auditLogger,
	}
}

func (h *ToolHandlers) QueryLogs(ctx context.Context, input QueryLogsInput) (agent.QueryLogsOutput, error) {
	start := time.Now()

	if err := h.validator.ValidateQuery(input.LogQL, input.StartTime, input.EndTime); err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return agent.QueryLogsOutput{}, fmt.Errorf("validation failed: %w", err)
	}

	startTime, err := agent.ParseTimeOrDefault(input.StartTime, 1*time.Hour)
	if err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return agent.QueryLogsOutput{}, err
	}
	endTime, err := agent.ParseTimeOrDefault(input.EndTime, 0)
	if err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return agent.QueryLogsOutput{}, err
	}

	var warning string
	startTime, endTime, warning = h.clampTimeRange(startTime, endTime)

	limit := clamp(input.Limit, 1, h.validator.MaxResults())
	if input.Limit == 0 {
		limit = 100
	}

	direction := input.Direction
	if direction == "" {
		direction = "backward"
	}
	if direction != "forward" && direction != "backward" {
		direction = "backward"
	}

	resp, err := h.lokiClient.QueryRange(ctx, loki.QueryRangeRequest{
		Query:     input.LogQL,
		Start:     startTime,
		End:       endTime,
		Limit:     limit,
		Direction: direction,
	})
	if err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return agent.QueryLogsOutput{}, fmt.Errorf("loki query failed: %w", err)
	}

	out, err := buildQueryLogsOutput(resp, input.LogQL, startTime, endTime, limit, direction)
	if err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return out, err
	}

	if warning != "" {
		if out.Warning != "" {
			out.Warning = warning + " | " + out.Warning
		} else {
			out.Warning = warning
		}
	}
	out.ExecTimeMS = int(time.Since(start).Milliseconds())

	h.audit.ToolInvoked("query_logs", time.Since(start).Milliseconds(),
		slog.String("query", input.LogQL),
		slog.Int("result_count", out.TotalLogs),
	)
	return out, nil
}

func (h *ToolHandlers) GetLabels(ctx context.Context, input GetLabelsInput) (GetLabelsOutput, error) {
	start := time.Now()

	startTime, err := agent.ParseTimeOrDefault(input.StartTime, 6*time.Hour)
	if err != nil {
		h.audit.ToolFailed("get_labels", time.Since(start).Milliseconds(), err)
		return GetLabelsOutput{}, err
	}
	endTime, err := agent.ParseTimeOrDefault(input.EndTime, 0)
	if err != nil {
		h.audit.ToolFailed("get_labels", time.Since(start).Milliseconds(), err)
		return GetLabelsOutput{}, err
	}

	resp, err := h.lokiClient.Labels(ctx, loki.LabelsRequest{
		Start: startTime,
		End:   endTime,
	})
	if err != nil {
		h.audit.ToolFailed("get_labels", time.Since(start).Milliseconds(), err)
		return GetLabelsOutput{}, fmt.Errorf("loki labels failed: %w", err)
	}

	h.audit.ToolInvoked("get_labels", time.Since(start).Milliseconds(),
		slog.Int("result_count", len(resp.Data)),
	)
	return GetLabelsOutput{Labels: resp.Data}, nil
}

func (h *ToolHandlers) GetLabelValues(ctx context.Context, input GetLabelValuesInput) (GetLabelValuesOutput, error) {
	start := time.Now()

	if input.LabelName == "" {
		err := fmt.Errorf("label_name is required")
		h.audit.ToolFailed("get_label_values", time.Since(start).Milliseconds(), err)
		return GetLabelValuesOutput{}, err
	}

	startTime, err := agent.ParseTimeOrDefault(input.StartTime, 6*time.Hour)
	if err != nil {
		h.audit.ToolFailed("get_label_values", time.Since(start).Milliseconds(), err)
		return GetLabelValuesOutput{}, err
	}
	endTime, err := agent.ParseTimeOrDefault(input.EndTime, 0)
	if err != nil {
		h.audit.ToolFailed("get_label_values", time.Since(start).Milliseconds(), err)
		return GetLabelValuesOutput{}, err
	}

	resp, err := h.lokiClient.LabelValues(ctx, loki.LabelValuesRequest{
		LabelName: input.LabelName,
		Start:     startTime,
		End:       endTime,
	})
	if err != nil {
		h.audit.ToolFailed("get_label_values", time.Since(start).Milliseconds(), err)
		return GetLabelValuesOutput{}, fmt.Errorf("loki label_values failed: %w", err)
	}

	h.audit.ToolInvoked("get_label_values", time.Since(start).Milliseconds(),
		slog.String("label", input.LabelName),
		slog.Int("result_count", len(resp.Data)),
	)
	return GetLabelValuesOutput{
		LabelName: input.LabelName,
		Values:    resp.Data,
	}, nil
}

func (h *ToolHandlers) QueryStats(ctx context.Context, input QueryStatsInput) (agent.QueryStatsOutput, error) {
	start := time.Now()

	if err := h.validator.ValidateQuery(input.LogQL, input.StartTime, input.EndTime); err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return agent.QueryStatsOutput{}, fmt.Errorf("validation failed: %w", err)
	}

	startTime, err := agent.ParseTimeOrDefault(input.StartTime, 1*time.Hour)
	if err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return agent.QueryStatsOutput{}, err
	}
	endTime, err := agent.ParseTimeOrDefault(input.EndTime, 0)
	if err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return agent.QueryStatsOutput{}, err
	}

	var warning string
	startTime, endTime, warning = h.clampTimeRange(startTime, endTime)

	step := input.Step
	if step == "" {
		step = agent.AutoSelectStep(startTime, endTime)
	}

	resp, err := h.lokiClient.QueryRange(ctx, loki.QueryRangeRequest{
		Query: input.LogQL,
		Start: startTime,
		End:   endTime,
		Step:  step,
	})
	if err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return agent.QueryStatsOutput{}, fmt.Errorf("loki metric query failed: %w", err)
	}

	out, err := buildQueryStatsOutput(resp, input.LogQL, startTime, endTime, step)
	if err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return out, err
	}

	if warning != "" {
		if out.Warning != "" {
			out.Warning = warning + " | " + out.Warning
		} else {
			out.Warning = warning
		}
	}
	out.ExecTimeMS = int(time.Since(start).Milliseconds())

	h.audit.ToolInvoked("query_stats", time.Since(start).Milliseconds(),
		slog.String("query", input.LogQL),
		slog.Int("series_count", len(out.Series)),
	)
	return out, nil
}

func (h *ToolHandlers) clampTimeRange(start, end time.Time) (time.Time, time.Time, string) {
	maxRange := h.validator.MaxTimeRange()
	var warnings []string

	// Swap if reversed
	if end.Before(start) {
		start, end = end, start
		warnings = append(warnings, "start/end times were swapped")
	}

	// Cap future end times
	now := time.Now()
	if end.After(now) {
		end = now
	}

	// Clamp to max range
	if end.Sub(start) > maxRange {
		start = end.Add(-maxRange)
		warnings = append(warnings, fmt.Sprintf("time range clamped to %s (maximum allowed)", maxRange))
	}

	// Ensure non-zero range
	if !end.After(start) {
		start = end.Add(-1 * time.Hour)
		warnings = append(warnings, "defaulted to 1h time range")
	}

	return start, end, strings.Join(warnings, "; ")
}

func clamp(val, min, max int) int {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

func buildQueryLogsOutput(resp *loki.QueryResponse, query string, start, end time.Time, limit int, direction string) (agent.QueryLogsOutput, error) {
	out := agent.QueryLogsOutput{
		Query:     query,
		Direction: direction,
		Logs:      []agent.LogEntry{},
		TimeRange: fmt.Sprintf("%s to %s", start.Format(time.RFC3339), end.Format(time.RFC3339)),
	}

	if resp.Data.ResultType != "streams" {
		return out, nil
	}

	var streams []loki.Stream
	if err := json.Unmarshal(resp.Data.Result, &streams); err != nil {
		return out, fmt.Errorf("parsing streams: %w", err)
	}

	for _, s := range streams {
		for _, v := range s.Values {
			if len(v) < 2 {
				continue
			}
			ts, err := loki.ParseNanoTimestamp(v[0])
			if err != nil {
				continue
			}
			out.Logs = append(out.Logs, agent.LogEntry{
				Timestamp: ts.Format("2006-01-02T15:04:05.000Z07:00"),
				Line:      agent.TruncateLogLine(v[1]),
				Labels:    s.Labels,
			})
		}
	}

	// Use shared analysis for sorting, patterns, and label distribution
	agent.AnalyzeLogs(&out, limit)

	// When zero results, guide the model to investigate instead of giving up
	if len(out.Logs) == 0 {
		out.Warning = "ZERO RESULTS — do NOT tell the user there are no logs without investigating first. " +
			"Try: (1) widen time range to 6h or 24h, (2) remove filters (use a bare selector like {service=~\".+\"}), " +
			"(3) call get_labels to verify label names/values exist, (4) check for typos in label values."
	}

	return out, nil
}

func buildQueryStatsOutput(resp *loki.QueryResponse, query string, start, end time.Time, step string) (agent.QueryStatsOutput, error) {
	out := agent.QueryStatsOutput{
		Query:     query,
		Step:      step,
		Series:    []agent.MetricSeries{},
		TimeRange: fmt.Sprintf("%s to %s", start.Format(time.RFC3339), end.Format(time.RFC3339)),
	}

	switch resp.Data.ResultType {
	case "matrix":
		var series []loki.MatrixSeries
		if err := json.Unmarshal(resp.Data.Result, &series); err != nil {
			return out, fmt.Errorf("parsing matrix: %w", err)
		}
		for _, s := range series {
			ms := agent.MetricSeries{Labels: s.Metric}
			for _, v := range s.Values {
				ts := time.Unix(int64(v.Timestamp), 0)
				ms.Values = append(ms.Values, agent.DataPoint{
					Timestamp: ts.Format(time.RFC3339),
					Value:     v.Value,
				})
			}
			out.Series = append(out.Series, ms)
		}
	case "vector":
		var samples []loki.VectorSample
		if err := json.Unmarshal(resp.Data.Result, &samples); err != nil {
			return out, fmt.Errorf("parsing vector: %w", err)
		}
		for _, s := range samples {
			ms := agent.MetricSeries{Labels: s.Metric}
			ts := time.Unix(int64(s.Value.Timestamp), 0)
			ms.Values = append(ms.Values, agent.DataPoint{
				Timestamp: ts.Format(time.RFC3339),
				Value:     s.Value.Value,
			})
			out.Series = append(out.Series, ms)
		}
	}

	// Use shared analysis for trend summaries and downsampling
	agent.AnalyzeStats(&out)

	// When zero results, guide the model to investigate instead of giving up
	if len(out.Series) == 0 {
		out.Warning = "ZERO RESULTS — do NOT tell the user there are no logs without investigating first. " +
			"Try: (1) widen time range to 6h or 24h, (2) simplify the query, " +
			"(3) call get_labels to verify label names/values exist."
	}

	return out, nil
}

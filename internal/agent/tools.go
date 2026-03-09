package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/safety"
)

// Tool input/output types

type QueryLogsInput struct {
	LogQL     string `json:"logql" jsonschema_description:"LogQL query string with stream selector and optional filters"`
	StartTime string `json:"start_time" jsonschema_description:"Start time as relative (e.g. 2h ago or 30m ago) or RFC3339. Defaults to 1h ago"`
	EndTime   string `json:"end_time,omitempty" jsonschema_description:"End time as relative or RFC3339. Defaults to now"`
	Limit     int    `json:"limit,omitempty" jsonschema_description:"Max log lines to return. Default 100 and max 500"`
	Direction string `json:"direction,omitempty" jsonschema_description:"Sort order: backward (newest first) or forward (oldest first). Default backward"`
}

type LogEntry struct {
	Timestamp string            `json:"timestamp"`
	Line      string            `json:"line"`
	Labels    map[string]string `json:"labels"`
}

type QueryLogsOutput struct {
	Logs      []LogEntry `json:"logs"`
	TotalLogs int        `json:"total_logs"`
	Truncated bool       `json:"truncated"`
	Query     string     `json:"query_executed"`
	TimeRange string     `json:"time_range"`
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
	Step      string `json:"step,omitempty" jsonschema_description:"Query resolution step (e.g. 1m or 5m). Default 1m"`
}

type DataPoint struct {
	Timestamp string `json:"timestamp"`
	Value     string `json:"value"`
}

type MetricSeries struct {
	Labels map[string]string `json:"labels"`
	Values []DataPoint       `json:"values"`
}

type QueryStatsOutput struct {
	Series []MetricSeries `json:"series"`
	Query  string         `json:"query_executed"`
}

type DiscoverServicesInput struct{}

type DiscoverServicesOutput struct {
	Services     []string `json:"services"`
	Environments []string `json:"environments"`
	LogLevels    []string `json:"log_levels"`
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

func (h *ToolHandlers) QueryLogs(ctx context.Context, input QueryLogsInput) (QueryLogsOutput, error) {
	start := time.Now()

	if err := h.validator.ValidateQuery(input.LogQL, input.StartTime, input.EndTime); err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return QueryLogsOutput{}, fmt.Errorf("validation failed: %w", err)
	}

	startTime, err := parseTimeOrDefault(input.StartTime, 1*time.Hour)
	if err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return QueryLogsOutput{}, err
	}
	endTime, err := parseTimeOrDefault(input.EndTime, 0)
	if err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return QueryLogsOutput{}, err
	}

	limit := loki.Clamp(input.Limit, 1, h.validator.MaxResults())
	if input.Limit == 0 {
		limit = 100
	}

	direction := input.Direction
	if direction == "" {
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
		return QueryLogsOutput{}, fmt.Errorf("loki query failed: %w", err)
	}

	out, err := buildQueryLogsOutput(resp, input.LogQL, startTime, endTime, limit)
	if err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return out, err
	}

	h.audit.ToolInvoked("query_logs", time.Since(start).Milliseconds(),
		slog.String("query", input.LogQL),
		slog.Int("result_count", out.TotalLogs),
	)
	return out, nil
}

func (h *ToolHandlers) GetLabels(ctx context.Context, input GetLabelsInput) (GetLabelsOutput, error) {
	start := time.Now()

	startTime, err := parseTimeOrDefault(input.StartTime, 6*time.Hour)
	if err != nil {
		h.audit.ToolFailed("get_labels", time.Since(start).Milliseconds(), err)
		return GetLabelsOutput{}, err
	}
	endTime, err := parseTimeOrDefault(input.EndTime, 0)
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

	startTime, err := parseTimeOrDefault(input.StartTime, 6*time.Hour)
	if err != nil {
		h.audit.ToolFailed("get_label_values", time.Since(start).Milliseconds(), err)
		return GetLabelValuesOutput{}, err
	}
	endTime, err := parseTimeOrDefault(input.EndTime, 0)
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

func (h *ToolHandlers) QueryStats(ctx context.Context, input QueryStatsInput) (QueryStatsOutput, error) {
	start := time.Now()

	if err := h.validator.ValidateQuery(input.LogQL, input.StartTime, input.EndTime); err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return QueryStatsOutput{}, fmt.Errorf("validation failed: %w", err)
	}

	startTime, err := parseTimeOrDefault(input.StartTime, 1*time.Hour)
	if err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return QueryStatsOutput{}, err
	}
	endTime, err := parseTimeOrDefault(input.EndTime, 0)
	if err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return QueryStatsOutput{}, err
	}

	step := input.Step
	if step == "" {
		step = "1m"
	}

	resp, err := h.lokiClient.QueryRange(ctx, loki.QueryRangeRequest{
		Query: input.LogQL,
		Start: startTime,
		End:   endTime,
		Step:  step,
	})
	if err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return QueryStatsOutput{}, fmt.Errorf("loki metric query failed: %w", err)
	}

	out, err := buildQueryStatsOutput(resp, input.LogQL)
	if err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return out, err
	}

	h.audit.ToolInvoked("query_stats", time.Since(start).Milliseconds(),
		slog.String("query", input.LogQL),
		slog.Int("series_count", len(out.Series)),
	)
	return out, nil
}

func (h *ToolHandlers) DiscoverServices(ctx context.Context, _ DiscoverServicesInput) (DiscoverServicesOutput, error) {
	start := time.Now()

	now := time.Now()
	sixHoursAgo := now.Add(-6 * time.Hour)
	req := loki.LabelValuesRequest{
		Start: sixHoursAgo,
		End:   now,
	}

	out := DiscoverServicesOutput{}

	// Try common service label names
	for _, labelName := range []string{"service", "app", "service_name", "job"} {
		req.LabelName = labelName
		resp, err := h.lokiClient.LabelValues(ctx, req)
		if err == nil && len(resp.Data) > 0 {
			out.Services = resp.Data
			break
		}
	}

	// Get environments
	for _, labelName := range []string{"env", "environment", "namespace"} {
		req.LabelName = labelName
		resp, err := h.lokiClient.LabelValues(ctx, req)
		if err == nil && len(resp.Data) > 0 {
			out.Environments = resp.Data
			break
		}
	}

	// Get log levels
	req.LabelName = "level"
	resp, err := h.lokiClient.LabelValues(ctx, req)
	if err == nil {
		out.LogLevels = resp.Data
	}

	h.audit.ToolInvoked("discover_services", time.Since(start).Milliseconds(),
		slog.Int("services_count", len(out.Services)),
		slog.Int("environments_count", len(out.Environments)),
		slog.Int("levels_count", len(out.LogLevels)),
	)
	return out, nil
}

// Helpers

func parseTimeOrDefault(input string, defaultAgo time.Duration) (time.Time, error) {
	if input == "" {
		if defaultAgo == 0 {
			return time.Now(), nil
		}
		return time.Now().Add(-defaultAgo), nil
	}
	return loki.ParseRelativeTime(input)
}

func buildQueryLogsOutput(resp *loki.QueryResponse, query string, start, end time.Time, limit int) (QueryLogsOutput, error) {
	out := QueryLogsOutput{
		Query:     query,
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
			out.Logs = append(out.Logs, LogEntry{
				Timestamp: ts.Format(time.RFC3339),
				Line:      v[1],
				Labels:    s.Labels,
			})
		}
	}

	out.TotalLogs = len(out.Logs)
	out.Truncated = out.TotalLogs >= limit

	return out, nil
}

func buildQueryStatsOutput(resp *loki.QueryResponse, query string) (QueryStatsOutput, error) {
	out := QueryStatsOutput{Query: query}

	switch resp.Data.ResultType {
	case "matrix":
		var series []loki.MatrixSeries
		if err := json.Unmarshal(resp.Data.Result, &series); err != nil {
			return out, fmt.Errorf("parsing matrix: %w", err)
		}
		for _, s := range series {
			ms := MetricSeries{Labels: s.Metric}
			for _, v := range s.Values {
				ts := time.Unix(int64(v.Timestamp), 0)
				ms.Values = append(ms.Values, DataPoint{
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
			ms := MetricSeries{Labels: s.Metric}
			ts := time.Unix(int64(s.Value.Timestamp), 0)
			ms.Values = append(ms.Values, DataPoint{
				Timestamp: ts.Format(time.RFC3339),
				Value:     s.Value.Value,
			})
			out.Series = append(out.Series, ms)
		}
	}

	return out, nil
}

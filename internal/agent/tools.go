package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

// ErrorPattern groups similar log lines so the LLM can reason about patterns
// rather than mentally parsing hundreds of individual lines.
type ErrorPattern struct {
	Pattern string  `json:"pattern"`
	Count   int     `json:"count"`
	Pct     float64 `json:"pct"`
	Sample  string  `json:"sample"`
}

type QueryLogsOutput struct {
	Logs          []LogEntry                `json:"logs"`
	TotalLogs     int                       `json:"total_logs"`
	Truncated     bool                      `json:"truncated"`
	Direction     string                    `json:"direction"`
	Query         string                    `json:"query_executed"`
	TimeRange     string                    `json:"time_range"`
	ExecTimeMS    int                       `json:"exec_time_ms,omitempty"`
	TopPatterns   []ErrorPattern            `json:"top_patterns,omitempty"`
	TotalPatterns int                       `json:"total_patterns,omitempty"`
	UniqueLabels  map[string]map[string]int `json:"unique_labels,omitempty"`
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

type DataPoint struct {
	Timestamp string `json:"timestamp"`
	Value     string `json:"value"`
}

type MetricSeries struct {
	Labels map[string]string `json:"labels"`
	Values []DataPoint       `json:"values"`
}

// TrendSummary gives the LLM pre-computed analysis so it can assess
// severity and direction without manually parsing time-series data points.
type TrendSummary struct {
	Total        float64 `json:"total"`
	Avg          float64 `json:"avg"`
	AvgPerMinute float64 `json:"avg_per_minute"`
	Latest       float64 `json:"latest"`
	Peak         float64 `json:"peak"`
	PeakTime     string  `json:"peak_time,omitempty"`
	Trend        string  `json:"trend"` // "increasing", "decreasing", "stable", "sparse"
	NonZeroPct   float64 `json:"non_zero_pct"`
}

type QueryStatsOutput struct {
	Series      []MetricSeries          `json:"series"`
	TotalSeries int                     `json:"total_series"`
	Step        string                  `json:"step,omitempty"`
	Query       string                  `json:"query_executed"`
	TimeRange   string                  `json:"time_range,omitempty"`
	ExecTimeMS  int                     `json:"exec_time_ms,omitempty"`
	Summaries   map[string]TrendSummary `json:"summaries,omitempty"`
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
		return QueryLogsOutput{}, fmt.Errorf("loki query failed: %w", err)
	}

	out, err := buildQueryLogsOutput(resp, input.LogQL, startTime, endTime, limit, direction)
	if err != nil {
		h.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return out, err
	}

	// Override with wall-clock time so the model sees the full round-trip cost
	// (network + Loki execution + parsing), not just Loki's internal exec time.
	out.ExecTimeMS = int(time.Since(start).Milliseconds())

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
		step = autoSelectStep(startTime, endTime)
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

	out, err := buildQueryStatsOutput(resp, input.LogQL, startTime, endTime, step)
	if err != nil {
		h.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return out, err
	}

	out.ExecTimeMS = int(time.Since(start).Milliseconds())

	h.audit.ToolInvoked("query_stats", time.Since(start).Milliseconds(),
		slog.String("query", input.LogQL),
		slog.Int("series_count", len(out.Series)),
	)
	return out, nil
}


// Helpers

// stepToMinutes converts a step string (e.g. "30s", "1m", "5m", "1h") to minutes.
// Returns 0 if the step can't be parsed. Used to pre-compute avg_per_minute
// so the LLM doesn't need to normalize rates manually.
func stepToMinutes(step string) float64 {
	d, err := time.ParseDuration(step)
	if err != nil {
		return 0
	}
	return d.Minutes()
}

// autoSelectStep picks a reasonable query step based on the time range.
// This keeps the number of data points manageable (60-120 points) for readable output.
func autoSelectStep(start, end time.Time) string {
	dur := end.Sub(start)
	switch {
	case dur <= 30*time.Minute:
		return "30s"
	case dur <= 2*time.Hour:
		return "1m"
	case dur <= 6*time.Hour:
		return "5m"
	case dur <= 12*time.Hour:
		return "15m"
	default:
		return "1h"
	}
}

func parseTimeOrDefault(input string, defaultAgo time.Duration) (time.Time, error) {
	if input == "" {
		if defaultAgo == 0 {
			return time.Now(), nil
		}
		return time.Now().Add(-defaultAgo), nil
	}
	return loki.ParseRelativeTime(input)
}

func buildQueryLogsOutput(resp *loki.QueryResponse, query string, start, end time.Time, limit int, direction string) (QueryLogsOutput, error) {
	out := QueryLogsOutput{
		Query:     query,
		Direction: direction,
		Logs:      []LogEntry{}, // never nil — cleaner for LLM JSON interpretation
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
			line := v[1]
			if len(line) > maxLogLineLength {
				line = truncateRuneSafe(line, maxLogLineLength) + "…[truncated]"
			}
			out.Logs = append(out.Logs, LogEntry{
				Timestamp: ts.Format("2006-01-02T15:04:05.000Z07:00"),
				Line:      line,
				Labels:    s.Labels,
			})
		}
	}

	// Sort logs by timestamp across all streams — direction must match
	// what the user/model requested for accurate timeline analysis.
	if direction == "forward" {
		sort.Slice(out.Logs, func(i, j int) bool {
			return out.Logs[i].Timestamp < out.Logs[j].Timestamp
		})
	} else {
		sort.Slice(out.Logs, func(i, j int) bool {
			return out.Logs[i].Timestamp > out.Logs[j].Timestamp
		})
	}

	out.TotalLogs = len(out.Logs)
	out.Truncated = out.TotalLogs >= limit
	if resp.Data.Stats.Summary.ExecTime > 0 {
		out.ExecTimeMS = int(resp.Data.Stats.Summary.ExecTime * 1000)
	}

	// Pre-compute pattern grouping so the LLM reasons from structured
	// analysis rather than parsing raw log lines.
	if out.TotalLogs >= 3 {
		var totalPatterns int
		out.TopPatterns, totalPatterns = extractPatterns(out.Logs, 10)
		if totalPatterns > len(out.TopPatterns) {
			out.TotalPatterns = totalPatterns
		}
	}

	// Show label distribution when results span multiple label values.
	// Dynamically detect all labels from the results rather than hardcoding
	// names — this works regardless of the Loki instance's label scheme.
	// Nested structure (e.g. {"service": {"payments": 45, "orders": 12}})
	// is much easier for the LLM to interpret than flat keys.
	out.UniqueLabels = make(map[string]map[string]int)
	labelNames := collectLabelNames(out.Logs)
	for _, label := range labelNames {
		if dist := extractLabelDistribution(out.Logs, label); dist != nil {
			out.UniqueLabels[label] = dist
		}
	}
	if len(out.UniqueLabels) == 0 {
		out.UniqueLabels = nil
	}

	return out, nil
}

// maxLogLineLength caps individual log lines sent to the LLM. Production
// systems emit structured JSON lines that can be 2-10KB each. With 100 entries,
// unbounded lines blow up to 200KB+ of LLM tokens — most of it noise fields the
// model will never use. The top_patterns analysis already extracts the message;
// raw lines are for evidence/context, so 1500 chars preserves the useful parts.
const maxLogLineLength = 1500

// maxDataPointsPerSeries caps the raw data points sent to the LLM per series.
// Summaries already contain the key insights (total, avg, peak, trend); the raw
// points are only useful for the LLM to reference specific timestamps. Capping
// these prevents sending thousands of tokens of data the LLM will never read.
const maxDataPointsPerSeries = 24

func buildQueryStatsOutput(resp *loki.QueryResponse, query string, start, end time.Time, step string) (QueryStatsOutput, error) {
	out := QueryStatsOutput{
		Query:     query,
		Step:      step,
		Series:    []MetricSeries{}, // never nil — cleaner for LLM JSON interpretation
		TimeRange: fmt.Sprintf("%s to %s", start.Format(time.RFC3339), end.Format(time.RFC3339)),
	}
	if resp.Data.Stats.Summary.ExecTime > 0 {
		out.ExecTimeMS = int(resp.Data.Stats.Summary.ExecTime * 1000)
	}

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

	out.TotalSeries = len(out.Series)

	// Compute trend summaries BEFORE downsampling — summaries use all data points
	// for accuracy, then we cap the raw points to save LLM tokens.
	if len(out.Series) > 0 {
		stepMinutes := stepToMinutes(step)
		out.Summaries = make(map[string]TrendSummary, len(out.Series))
		for i, s := range out.Series {
			key := seriesKey(s.Labels)
			summary := computeTrend(s.Values)
			// Pre-compute avg_per_minute so the LLM doesn't have to parse the
			// step and do division — eliminates a common source of math errors.
			if stepMinutes > 0 {
				summary.AvgPerMinute = math.Round(summary.Avg/stepMinutes*100) / 100
			}
			out.Summaries[key] = summary
			out.Series[i].Values = downsampleDataPoints(s.Values, maxDataPointsPerSeries)
		}
	}

	return out, nil
}

// downsampleDataPoints reduces a series to at most maxPoints by evenly sampling.
// Always preserves the first and last data points to maintain the time boundary.
func downsampleDataPoints(values []DataPoint, maxPoints int) []DataPoint {
	if len(values) <= maxPoints {
		return values
	}
	result := make([]DataPoint, 0, maxPoints)
	result = append(result, values[0])

	// Evenly sample the middle
	step := float64(len(values)-1) / float64(maxPoints-1)
	for i := 1; i < maxPoints-1; i++ {
		idx := int(math.Round(float64(i) * step))
		result = append(result, values[idx])
	}

	result = append(result, values[len(values)-1])
	return result
}

// patternNormalizer replaces variable parts of log lines (IDs, timestamps,
// numbers, IPs) with placeholders so similar errors group together.
var patternNormalizer = regexp.MustCompile(
	`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b` + // UUIDs
		`|\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?\b` + // IPs
		`|\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[^\s]*\b` + // timestamps
		`|\b0x[0-9a-f]+\b` + // hex
		`|\b\d{5,}\b`, // long numbers (IDs, ports, etc.)
)

// extractPatterns groups log lines by normalized signature and returns the
// top N patterns with counts and a representative sample line.
func extractPatterns(logs []LogEntry, topN int) ([]ErrorPattern, int) {
	if len(logs) == 0 {
		return nil, 0
	}

	type patternData struct {
		count  int
		sample string
	}
	groups := make(map[string]*patternData)

	for _, entry := range logs {
		line := extractLogMessage(entry.Line)
		sig := patternNormalizer.ReplaceAllString(line, "<*>")
		// Collapse repeated placeholders
		sig = strings.ReplaceAll(sig, "<*>:<*>", "<*>")
		// Truncate long signatures at a valid UTF-8 rune boundary
		if len(sig) > 200 {
			sig = truncateRuneSafe(sig, 200) + "..."
		}
		if g, ok := groups[sig]; ok {
			g.count++
		} else {
			// Store the extracted message as sample, not raw JSON —
			// this is what the LLM shows users as evidence.
			groups[sig] = &patternData{count: 1, sample: line}
		}
	}

	total := len(logs)
	patterns := make([]ErrorPattern, 0, len(groups))
	for sig, g := range groups {
		sample := g.sample
		if len(sample) > 200 {
			sample = truncateRuneSafe(sample, 200) + "..."
		}
		patterns = append(patterns, ErrorPattern{
			Pattern: sig,
			Count:   g.count,
			Pct:     math.Round(float64(g.count)/float64(total)*1000) / 10,
			Sample:  sample,
		})
	}

	sort.Slice(patterns, func(i, j int) bool {
		return patterns[i].Count > patterns[j].Count
	})

	totalPatterns := len(patterns)
	if len(patterns) > topN {
		patterns = patterns[:topN]
	}
	return patterns, totalPatterns
}

// extractLabelDistribution counts how many logs each label value appears in,
// helping the LLM understand which services or levels dominate the results.
func extractLabelDistribution(logs []LogEntry, label string) map[string]int {
	if len(logs) == 0 {
		return nil
	}
	dist := make(map[string]int)
	for _, entry := range logs {
		if v, ok := entry.Labels[label]; ok {
			dist[v]++
		}
	}
	if len(dist) <= 1 {
		return nil // not interesting if all the same
	}
	return dist
}

// safeParseFloat parses a string as float64, returning 0 and false for
// unparseable values, NaN, and Inf. Loki can return NaN from division by
// zero (e.g. error_rate / total_rate when total is 0) and Inf from certain
// aggregations. Without this guard, math.NaN() poisons all arithmetic and
// json.Marshal fails with "unsupported value: NaN", crashing the tool response.
func safeParseFloat(s string) (float64, bool) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// computeTrend analyzes a time series and returns a summary with
// total, latest, peak, and trend direction.
func computeTrend(values []DataPoint) TrendSummary {
	if len(values) == 0 {
		return TrendSummary{Trend: "sparse"}
	}

	var total, peak, latest float64
	var peakTime string
	nonZero := 0
	validCount := 0

	for _, dp := range values {
		v, ok := safeParseFloat(dp.Value)
		if !ok {
			continue
		}
		validCount++
		total += v
		latest = v // continuously updated — last valid value is the most recent
		if v > peak {
			peak = v
			peakTime = dp.Timestamp
		}
		if v > 0 {
			nonZero++
		}
	}

	if validCount == 0 {
		return TrendSummary{Trend: "sparse"}
	}

	nonZeroPct := float64(nonZero) / float64(validCount) * 100
	avg := total / float64(validCount)

	trend := "stable"
	if validCount < 3 {
		if total == 0 {
			trend = "sparse"
		}
	} else {
		// Compare first third vs last third to determine trend
		third := len(values) / 3
		var firstSum, lastSum float64
		for i := 0; i < third; i++ {
			if v, ok := safeParseFloat(values[i].Value); ok {
				firstSum += v
			}
		}
		for i := len(values) - third; i < len(values); i++ {
			if v, ok := safeParseFloat(values[i].Value); ok {
				lastSum += v
			}
		}
		if firstSum == 0 && lastSum == 0 {
			trend = "sparse"
		} else if firstSum == 0 {
			// Went from nothing to something — clearly increasing, but only
			// flag it if the activity is meaningful (not a single blip).
			if lastSum > 0 {
				trend = "increasing"
			}
		} else if lastSum > firstSum*1.3 {
			trend = "increasing"
		} else if lastSum < firstSum*0.7 {
			trend = "decreasing"
		}
	}

	return TrendSummary{
		Total:      math.Round(total*100) / 100,
		Avg:        math.Round(avg*100) / 100,
		Latest:     math.Round(latest*100) / 100,
		Peak:       math.Round(peak*100) / 100,
		PeakTime:   peakTime,
		Trend:      trend,
		NonZeroPct: math.Round(nonZeroPct*10) / 10,
	}
}

// extractLogMessage extracts the human-readable message from a log line.
// Handles JSON ({"msg": "..."}) and logfmt (msg="..." or msg=...) formats,
// returning just the message instead of the full structured line — producing
// much cleaner patterns for the LLM to reason about.
func extractLogMessage(line string) string {
	line = strings.TrimSpace(line)
	if len(line) == 0 {
		return line
	}

	// JSON extraction
	if line[0] == '{' {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			return line
		}
		for _, key := range []string{"msg", "message", "error", "err", "log", "error_message", "reason", "detail"} {
			if v, ok := parsed[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					// Docker/K8s logging drivers append "\n" to every log line
					// in the "log" JSON key. Strip trailing whitespace so patterns
					// group correctly ("timeout\n" and "timeout" should be the same).
					return strings.TrimRight(s, "\n\r")
				}
			}
		}
		// Check for nested error objects: {"error": {"message": "timeout"}}
		// Common in Go structured logging where errors are serialized as objects.
		for _, key := range []string{"error", "err"} {
			if v, ok := parsed[key]; ok {
				if m, ok := v.(map[string]any); ok {
					for _, msgKey := range []string{"message", "msg"} {
						if inner, ok := m[msgKey]; ok {
							if s, ok := inner.(string); ok && s != "" {
								return s
							}
						}
					}
				}
			}
		}
		return line
	}

	// Logfmt extraction: many Go apps emit key=value pairs like
	// level=error msg="DB connection refused" service=payments trace_id=abc123
	for _, key := range []string{"msg=", "message=", "error=", "err=", "error_message=", "reason=", "detail="} {
		if msg := extractLogfmtValue(line, key); msg != "" {
			return msg
		}
	}

	return line
}

// extractLogfmtValue extracts the value for a key= token from a logfmt line.
// Handles both quoted (msg="connection refused") and unquoted (msg=timeout) values.
// Returns "" if the key is not found or is not at a word boundary.
// Searches all occurrences — if "msg=" appears as a substring of another key
// (e.g. "customer_msg="), it skips that match and keeps looking.
func extractLogfmtValue(line, key string) string {
	searchFrom := 0
	for searchFrom < len(line) {
		idx := strings.Index(line[searchFrom:], key)
		if idx == -1 {
			return ""
		}
		idx += searchFrom
		// Ensure key is at a word boundary (start of line or preceded by whitespace)
		if idx > 0 && line[idx-1] != ' ' && line[idx-1] != '\t' {
			searchFrom = idx + len(key)
			continue
		}
		val := line[idx+len(key):]
		if len(val) == 0 {
			return ""
		}
		if val[0] == '"' {
			// Quoted value: msg="connection refused"
			end := strings.IndexByte(val[1:], '"')
			if end >= 0 {
				return val[1 : end+1]
			}
			return val[1:] // unclosed quote — take the rest
		}
		// Unquoted value: msg=timeout — ends at next space
		end := strings.IndexByte(val, ' ')
		if end == -1 {
			return val
		}
		return val[:end]
	}
	return ""
}

// collectLabelNames returns all distinct label names present in the log entries.
// This lets us dynamically detect which labels exist (service, app, job, env, etc.)
// rather than hardcoding a fixed list.
func collectLabelNames(logs []LogEntry) []string {
	seen := make(map[string]struct{})
	for _, entry := range logs {
		for k := range entry.Labels {
			// Skip Loki internal labels (e.g. __stream_shard__, __name__) —
			// they waste LLM tokens and can't be used in queries.
			if strings.HasPrefix(k, "__") {
				continue
			}
			seen[k] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for k := range seen {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// truncateRuneSafe truncates a string to at most maxBytes, backing up to the
// nearest valid UTF-8 rune boundary. Prevents slicing multibyte characters
// (¥, €, CJK) in half, which would produce garbled text in pattern analysis.
func truncateRuneSafe(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// seriesKey builds a human-readable key from metric labels for the summaries map.
func seriesKey(labels map[string]string) string {
	if len(labels) == 0 {
		return "total"
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

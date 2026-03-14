package cwsource

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/audit"
)

// CloudWatchSource implements logsource.LogSource for AWS CloudWatch Logs.
type CloudWatchSource struct {
	client    *Client
	logGroups []string // scoped log groups for this tenant (empty = discover all)
	audit     *audit.Logger
}

// Config holds configuration for creating a CloudWatchSource.
type Config struct {
	Region    string
	LogGroups []string // pre-configured log groups (empty = discover via API)
	Audit     *audit.Logger
}

// New creates a CloudWatchSource.
func New(ctx context.Context, cfg Config) (*CloudWatchSource, error) {
	client, err := NewClient(ctx, ClientConfig{
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("creating cloudwatch client: %w", err)
	}
	return &CloudWatchSource{
		client:    client,
		logGroups: cfg.LogGroups,
		audit:     cfg.Audit,
	}, nil
}

func (s *CloudWatchSource) Name() string { return "CloudWatch" }
func (s *CloudWatchSource) Description() string {
	return "Log analysis assistant that queries AWS CloudWatch Logs via natural language"
}
func (s *CloudWatchSource) Instruction() string { return systemInstruction }

func (s *CloudWatchSource) HealthCheck(ctx context.Context) error {
	// Verify we can reach CloudWatch at all
	groups, err := s.client.ListLogGroups(ctx, "")
	if err != nil {
		return fmt.Errorf("cloudwatch unreachable: %w", err)
	}

	if len(s.logGroups) == 0 {
		// No log groups configured — queries will fail
		if len(groups) == 0 {
			return fmt.Errorf("no log groups found and CW_LOG_GROUPS not set — set CW_LOG_GROUPS or check IAM permissions")
		}
		return fmt.Errorf("CW_LOG_GROUPS not set — set it to a comma-separated list of log groups (found %d groups in account, e.g. %s)", len(groups), groups[0])
	}

	// Verify configured log groups actually exist
	existing := make(map[string]bool, len(groups))
	for _, g := range groups {
		existing[g] = true
	}
	var missing []string
	for _, g := range s.logGroups {
		if !existing[g] {
			missing = append(missing, g)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("configured log groups not found: %v — check names and IAM permissions", missing)
	}

	return nil
}

// Tool input types

type QueryLogsInput struct {
	Query      string   `json:"query" jsonschema_description:"CloudWatch Insights query string (e.g. filter @message like /error/ | fields @timestamp, @message)"`
	LogGroups  []string `json:"log_groups" jsonschema_description:"Log group names to query. Use list_log_groups to discover available groups."`
	StartTime  string   `json:"start_time" jsonschema_description:"Start time as relative (e.g. 2h ago or 30m ago) or RFC3339. Defaults to 1h ago"`
	EndTime    string   `json:"end_time,omitempty" jsonschema_description:"End time as relative or RFC3339. Defaults to now"`
	Limit      int      `json:"limit,omitempty" jsonschema_description:"Max log lines to return. Default 100 and max 500"`
	Direction  string   `json:"direction,omitempty" jsonschema_description:"Sort order: backward (newest first) or forward (oldest first). Default backward"`
}

type ListLogGroupsInput struct {
	Prefix string `json:"prefix,omitempty" jsonschema_description:"Optional prefix filter for log group names"`
}

type ListLogGroupsOutput struct {
	LogGroups []string `json:"log_groups"`
	Total     int      `json:"total"`
}

type GetLogFieldsInput struct {
	LogGroups []string `json:"log_groups" jsonschema_description:"Log group names to discover fields for"`
}

type GetLogFieldsOutput struct {
	LogGroups []string `json:"log_groups"`
	Fields    []string `json:"fields"`
}

type QueryStatsInput struct {
	Query     string   `json:"query" jsonschema_description:"CloudWatch Insights query with stats aggregation (e.g. filter @message like /error/ | stats count(*) by bin(5m))"`
	LogGroups []string `json:"log_groups" jsonschema_description:"Log group names to query"`
	StartTime string   `json:"start_time" jsonschema_description:"Start time as relative or RFC3339"`
	EndTime   string   `json:"end_time,omitempty" jsonschema_description:"End time. Defaults to now"`
	Step      string   `json:"step,omitempty" jsonschema_description:"Time bin size (e.g. 5m, 1h). If your query already has bin(), this is ignored."`
}

func (s *CloudWatchSource) Tools() ([]tool.Tool, error) {
	queryLogsTool, err := functiontool.New(functiontool.Config{
		Name:        "query_logs",
		Description: "Fetch raw log lines from CloudWatch Logs with automatic pattern analysis. Runs a CloudWatch Insights query and returns individual log entries plus top_patterns (grouped similar lines with counts and pct), total_patterns, unique_labels (nested label distribution), and direction. Use this when you need actual log messages, error details, or stack traces. The top_patterns field lets you immediately identify the dominant error type — use the pct field to say e.g. '78% of errors are timeouts'. NOT for counting or aggregation over time — use query_stats for that.",
	}, func(ctx tool.Context, input QueryLogsInput) (agent.QueryLogsOutput, error) {
		return s.queryLogs(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating query_logs tool: %w", err)
	}

	listLogGroupsTool, err := functiontool.New(functiontool.Config{
		Name:        "list_log_groups",
		Description: "List available CloudWatch log groups. Call this FIRST in any new conversation to discover which log groups exist. Each log group typically corresponds to a service, application, or AWS resource. Use the results to pick the right log groups for queries. Optionally filter by prefix (e.g. '/aws/lambda/' or '/ecs/').",
	}, func(ctx tool.Context, input ListLogGroupsInput) (ListLogGroupsOutput, error) {
		return s.listLogGroups(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating list_log_groups tool: %w", err)
	}

	getLogFieldsTool, err := functiontool.New(functiontool.Config{
		Name:        "get_log_fields",
		Description: "Discover the fields available in one or more log groups. After calling list_log_groups, call this to learn which fields exist (e.g. level, service, statusCode, @duration, @message). Essential for building correct Insights queries — using a field that doesn't exist returns 0 results.",
	}, func(ctx tool.Context, input GetLogFieldsInput) (GetLogFieldsOutput, error) {
		return s.getLogFields(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating get_log_fields tool: %w", err)
	}

	queryStatsTool, err := functiontool.New(functiontool.Config{
		Name:        "query_stats",
		Description: "Run aggregation queries on CloudWatch Logs to get counts, rates, and trends over time with automatic trend analysis. Returns time-series data plus summaries with pre-computed total, avg, avg_per_minute (already normalized — use directly for user-facing rates), latest, peak, peak_time, trend direction, and non_zero_pct. Use this for 'how many errors?', 'error rate trend', 'compare periods', 'which service has the most errors?'. The summaries field gives instant verdicts without parsing data points. NOT for raw logs — use query_logs for actual log lines.",
	}, func(ctx tool.Context, input QueryStatsInput) (agent.QueryStatsOutput, error) {
		return s.queryStats(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating query_stats tool: %w", err)
	}

	return []tool.Tool{queryLogsTool, listLogGroupsTool, getLogFieldsTool, queryStatsTool}, nil
}

// resolveLogGroups returns the log groups to query: input overrides, then workspace config, then error.
func (s *CloudWatchSource) resolveLogGroups(input []string) ([]string, error) {
	if len(input) > 0 {
		return input, nil
	}
	if len(s.logGroups) > 0 {
		return s.logGroups, nil
	}
	return nil, fmt.Errorf("no log groups specified — call list_log_groups first to discover available groups")
}

func (s *CloudWatchSource) queryLogs(ctx context.Context, input QueryLogsInput) (agent.QueryLogsOutput, error) {
	start := time.Now()

	if input.Query != "" {
		if err := validateInsightsQuery(input.Query); err != nil {
			s.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
			return agent.QueryLogsOutput{}, fmt.Errorf("validation failed: %w", err)
		}
	}

	logGroups, err := s.resolveLogGroups(input.LogGroups)
	if err != nil {
		s.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return agent.QueryLogsOutput{}, err
	}

	startTime, err := agent.ParseTimeOrDefault(input.StartTime, 1*time.Hour)
	if err != nil {
		s.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return agent.QueryLogsOutput{}, err
	}
	endTime, err := agent.ParseTimeOrDefault(input.EndTime, 0)
	if err != nil {
		s.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return agent.QueryLogsOutput{}, err
	}

	// Defensive time handling: swap if reversed, clamp to 24h max
	startTime, endTime, warning := sanitizeTimeRange(startTime, endTime)

	limit := input.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	direction := input.Direction
	if direction != "forward" {
		direction = "backward"
	}

	// Build query — ensure it has a fields clause for structured output
	query := input.Query
	if query == "" {
		query = "fields @timestamp, @message"
	}
	// Add sort if not present
	if direction == "forward" && !strings.Contains(strings.ToLower(query), "sort") {
		query += " | sort @timestamp asc"
	} else if !strings.Contains(strings.ToLower(query), "sort") {
		query += " | sort @timestamp desc"
	}
	// Add limit if not present
	if !strings.Contains(strings.ToLower(query), "limit") {
		query += fmt.Sprintf(" | limit %d", limit)
	}

	result, err := s.client.RunInsightsQuery(ctx, logGroups, query, startTime, endTime, int32(limit))
	if err != nil {
		s.audit.ToolFailed("query_logs", time.Since(start).Milliseconds(), err)
		return agent.QueryLogsOutput{}, fmt.Errorf("cloudwatch query failed: %w", err)
	}

	// Convert CloudWatch results to LogEntry format
	out := agent.QueryLogsOutput{
		Query:     query,
		Direction: direction,
		Logs:      make([]agent.LogEntry, 0, len(result.Results)),
		TimeRange: fmt.Sprintf("%s to %s", startTime.Format(time.RFC3339), endTime.Format(time.RFC3339)),
		Warning:   warning,
	}

	for _, row := range result.Results {
		entry := agent.LogEntry{
			Labels: make(map[string]string),
		}
		for _, field := range row {
			if field.Field == nil || field.Value == nil {
				continue
			}
			name := *field.Field
			value := *field.Value

			switch name {
			case "@timestamp":
				entry.Timestamp = value
			case "@message":
				entry.Line = agent.TruncateLogLine(value)
			case "@ptr":
				// Skip internal pointer field
			default:
				entry.Labels[name] = value
			}
		}

		// Use @message or concatenate all fields if no message
		if entry.Line == "" {
			var parts []string
			for _, field := range row {
				if field.Field != nil && field.Value != nil && *field.Field != "@ptr" {
					parts = append(parts, fmt.Sprintf("%s=%s", *field.Field, *field.Value))
				}
			}
			entry.Line = agent.TruncateLogLine(strings.Join(parts, " "))
		}

		// Default timestamp to now if not present
		if entry.Timestamp == "" {
			entry.Timestamp = time.Now().Format(time.RFC3339)
		}

		out.Logs = append(out.Logs, entry)
	}

	// Use shared analysis: patterns, label distribution, sorting
	agent.AnalyzeLogs(&out, limit)

	// When zero results, add diagnostic hints so the model investigates instead of giving up
	if len(out.Logs) == 0 {
		hints := "ZERO RESULTS — do NOT tell the user there are no logs without investigating first. "
		hints += "Try: (1) widen time range to 6h or 24h, (2) remove filters to get raw logs (fields @timestamp, @message), "
		hints += "(3) call list_log_groups to verify the log group name exists exactly as specified. "
		if len(s.logGroups) > 0 {
			hints += fmt.Sprintf("Configured log groups: %v", s.logGroups)
		}
		if out.Warning != "" {
			out.Warning += "; " + hints
		} else {
			out.Warning = hints
		}
	}

	out.ExecTimeMS = int(time.Since(start).Milliseconds())

	s.audit.ToolInvoked("query_logs", time.Since(start).Milliseconds())
	return out, nil
}

func (s *CloudWatchSource) listLogGroups(ctx context.Context, input ListLogGroupsInput) (ListLogGroupsOutput, error) {
	start := time.Now()

	// If workspace has scoped log groups, return those
	if len(s.logGroups) > 0 && input.Prefix == "" {
		s.audit.ToolInvoked("list_log_groups", time.Since(start).Milliseconds())
		return ListLogGroupsOutput{
			LogGroups: s.logGroups,
			Total:     len(s.logGroups),
		}, nil
	}

	groups, err := s.client.ListLogGroups(ctx, input.Prefix)
	if err != nil {
		s.audit.ToolFailed("list_log_groups", time.Since(start).Milliseconds(), err)
		return ListLogGroupsOutput{}, fmt.Errorf("listing log groups failed: %w", err)
	}

	// If workspace has scoped groups, filter to only those
	if len(s.logGroups) > 0 {
		allowed := make(map[string]struct{}, len(s.logGroups))
		for _, g := range s.logGroups {
			allowed[g] = struct{}{}
		}
		filtered := make([]string, 0)
		for _, g := range groups {
			if _, ok := allowed[g]; ok {
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}

	sort.Strings(groups)

	s.audit.ToolInvoked("list_log_groups", time.Since(start).Milliseconds())
	return ListLogGroupsOutput{
		LogGroups: groups,
		Total:     len(groups),
	}, nil
}

func (s *CloudWatchSource) getLogFields(ctx context.Context, input GetLogFieldsInput) (GetLogFieldsOutput, error) {
	start := time.Now()

	logGroups, err := s.resolveLogGroups(input.LogGroups)
	if err != nil {
		s.audit.ToolFailed("get_log_fields", time.Since(start).Milliseconds(), err)
		return GetLogFieldsOutput{}, err
	}

	fields, err := s.client.DiscoverFields(ctx, logGroups)
	if err != nil {
		s.audit.ToolFailed("get_log_fields", time.Since(start).Milliseconds(), err)
		return GetLogFieldsOutput{}, fmt.Errorf("discovering fields failed: %w", err)
	}

	sort.Strings(fields)

	s.audit.ToolInvoked("get_log_fields", time.Since(start).Milliseconds())
	return GetLogFieldsOutput{
		LogGroups: logGroups,
		Fields:    fields,
	}, nil
}

func (s *CloudWatchSource) queryStats(ctx context.Context, input QueryStatsInput) (agent.QueryStatsOutput, error) {
	start := time.Now()

	if err := validateInsightsQuery(input.Query); err != nil {
		s.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return agent.QueryStatsOutput{}, fmt.Errorf("validation failed: %w", err)
	}

	logGroups, err := s.resolveLogGroups(input.LogGroups)
	if err != nil {
		s.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return agent.QueryStatsOutput{}, err
	}

	startTime, err := agent.ParseTimeOrDefault(input.StartTime, 1*time.Hour)
	if err != nil {
		s.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return agent.QueryStatsOutput{}, err
	}
	endTime, err := agent.ParseTimeOrDefault(input.EndTime, 0)
	if err != nil {
		s.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return agent.QueryStatsOutput{}, err
	}

	// Defensive time handling: swap if reversed, clamp to 24h max
	startTime, endTime, warning := sanitizeTimeRange(startTime, endTime)

	step := input.Step
	if step == "" {
		step = agent.AutoSelectStep(startTime, endTime)
	}

	query := input.Query

	result, err := s.client.RunInsightsQuery(ctx, logGroups, query, startTime, endTime, 10000)
	if err != nil {
		s.audit.ToolFailed("query_stats", time.Since(start).Milliseconds(), err)
		return agent.QueryStatsOutput{}, fmt.Errorf("cloudwatch stats query failed: %w", err)
	}

	out := agent.QueryStatsOutput{
		Query:     query,
		Step:      step,
		Series:    make([]agent.MetricSeries, 0),
		TimeRange: fmt.Sprintf("%s to %s", startTime.Format(time.RFC3339), endTime.Format(time.RFC3339)),
		Warning:   warning,
	}

	// Parse CloudWatch Insights stats results into MetricSeries.
	// Results can be time-bucketed (bin column) or grouped by field.
	out.Series = s.parseStatsResults(result)

	// Use shared analysis: trend summaries and downsampling
	agent.AnalyzeStats(&out)

	// When zero results, add diagnostic hints
	if len(out.Series) == 0 {
		hints := "ZERO RESULTS — do NOT tell the user there are no logs without investigating first. "
		hints += "Try: (1) widen time range to 6h or 24h, (2) simplify the query, "
		hints += "(3) call list_log_groups to verify the log group name exists exactly as specified."
		if out.Warning != "" {
			out.Warning += "; " + hints
		} else {
			out.Warning = hints
		}
	}

	out.ExecTimeMS = int(time.Since(start).Milliseconds())

	s.audit.ToolInvoked("query_stats", time.Since(start).Milliseconds())
	return out, nil
}

// parseStatsResults converts CloudWatch Insights stats output into MetricSeries.
func (s *CloudWatchSource) parseStatsResults(result *QueryResult) []agent.MetricSeries {
	if len(result.Results) == 0 {
		return nil
	}

	// Detect if results have a time bin column
	hasBin := false
	binField := ""
	for _, field := range result.Results[0] {
		if field.Field != nil {
			name := *field.Field
			if strings.HasPrefix(name, "bin(") {
				hasBin = true
				binField = name
				break
			}
		}
	}

	if hasBin {
		return s.parseTimeSeriesResults(result, binField)
	}
	return s.parseGroupedResults(result)
}

// parseTimeSeriesResults handles results with a time bin column → single or multi-series.
func (s *CloudWatchSource) parseTimeSeriesResults(result *QueryResult, binField string) []agent.MetricSeries {
	// Group by non-bin, non-count fields (series keys)
	type seriesData struct {
		labels map[string]string
		points []agent.DataPoint
	}
	seriesMap := make(map[string]*seriesData)

	for _, row := range result.Results {
		var ts, value string
		labels := make(map[string]string)

		for _, field := range row {
			if field.Field == nil || field.Value == nil {
				continue
			}
			name := *field.Field
			val := *field.Value

			switch {
			case name == binField:
				ts = val
			case name == "count(*)" || name == "count" || name == "cnt" ||
				name == "sum" || name == "avg" || name == "max" || name == "min":
				value = val
			case name == "@ptr":
				// skip
			default:
				labels[name] = val
			}
		}

		// If no explicit count field, default to "1" (presence = 1 event)
		if value == "" {
			value = "1"
		}

		key := seriesKeyFromLabels(labels)
		sd, ok := seriesMap[key]
		if !ok {
			sd = &seriesData{labels: labels}
			seriesMap[key] = sd
		}
		sd.points = append(sd.points, agent.DataPoint{
			Timestamp: ts,
			Value:     value,
		})
	}

	series := make([]agent.MetricSeries, 0, len(seriesMap))
	for _, sd := range seriesMap {
		series = append(series, agent.MetricSeries{
			Labels: sd.labels,
			Values: sd.points,
		})
	}
	return series
}

// parseGroupedResults handles non-time-series results (e.g. stats count by field).
func (s *CloudWatchSource) parseGroupedResults(result *QueryResult) []agent.MetricSeries {
	var series []agent.MetricSeries

	for _, row := range result.Results {
		labels := make(map[string]string)
		var value string

		for _, field := range row {
			if field.Field == nil || field.Value == nil {
				continue
			}
			name := *field.Field
			val := *field.Value

			switch {
			case name == "count(*)" || name == "count" || name == "cnt" ||
				name == "sum" || name == "avg" || name == "max" || name == "min":
				value = val
			case name == "@ptr":
				// skip
			default:
				labels[name] = val
			}
		}

		if value == "" {
			value = "0"
		}

		series = append(series, agent.MetricSeries{
			Labels: labels,
			Values: []agent.DataPoint{
				{
					Timestamp: time.Now().Format(time.RFC3339),
					Value:     value,
				},
			},
		})
	}

	return series
}

// sanitizeTimeRange swaps start/end if reversed, clamps to 24h max, and ensures
// times are not in the future. Returns corrected times and a warning if anything changed.
func sanitizeTimeRange(start, end time.Time) (time.Time, time.Time, string) {
	const maxRange = 24 * time.Hour
	var warnings []string

	// Swap if reversed
	if end.Before(start) {
		start, end = end, start
		warnings = append(warnings, "start/end times were swapped")
	}

	// Ensure end is not in the future (CloudWatch has no future logs)
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

	warning := strings.Join(warnings, "; ")
	return start, end, warning
}

func seriesKeyFromLabels(labels map[string]string) string {
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


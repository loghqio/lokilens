package cwsource

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// Client wraps the AWS CloudWatch Logs API.
type Client struct {
	cw     *cloudwatchlogs.Client
	logger *slog.Logger
}

// ClientConfig holds configuration for creating a CloudWatch client.
type ClientConfig struct {
	Region string
	Logger *slog.Logger
}

// NewClient creates a CloudWatch Logs client using the standard AWS credential chain.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return &Client{
		cw:     cloudwatchlogs.NewFromConfig(awsCfg),
		logger: cfg.Logger,
	}, nil
}

// QueryResult holds the results of a CloudWatch Insights query.
type QueryResult struct {
	Results [][]types.ResultField
	Stats   *types.QueryStatistics
}

// RunInsightsQuery executes a CloudWatch Insights query and polls until complete.
func (c *Client) RunInsightsQuery(ctx context.Context, logGroups []string, query string, start, end time.Time, limit int32) (*QueryResult, error) {
	// Guard against swapped times — the LLM can generate bad parameters
	if end.Before(start) {
		start, end = end, start
	}
	// Default to last 1 hour if times are equal
	if start.Equal(end) {
		start = end.Add(-1 * time.Hour)
	}

	c.logger.Debug("starting insights query",
		"log_groups", logGroups,
		"query", query,
		"start", start.Format(time.RFC3339),
		"end", end.Format(time.RFC3339),
	)

	input := &cloudwatchlogs.StartQueryInput{
		LogGroupNames: logGroups,
		QueryString:   aws.String(query),
		StartTime:     aws.Int64(start.Unix()),
		EndTime:       aws.Int64(end.Unix()),
		Limit:         aws.Int32(limit),
	}

	startResp, err := c.cw.StartQuery(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("starting query: %w", err)
	}

	return c.pollQueryResults(ctx, *startResp.QueryId)
}

// pollQueryResults polls GetQueryResults until the query completes or fails.
func (c *Client) pollQueryResults(ctx context.Context, queryID string) (*QueryResult, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// Cap polling at 60 seconds to avoid hanging on expensive queries.
	deadline := time.After(60 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("query %s timed out after 60s", queryID)
		case <-ticker.C:
			resp, err := c.cw.GetQueryResults(ctx, &cloudwatchlogs.GetQueryResultsInput{
				QueryId: aws.String(queryID),
			})
			if err != nil {
				return nil, fmt.Errorf("getting query results: %w", err)
			}

			switch resp.Status {
			case types.QueryStatusComplete:
				return &QueryResult{
					Results: resp.Results,
					Stats:   resp.Statistics,
				}, nil
			case types.QueryStatusFailed:
				return nil, fmt.Errorf("query failed")
			case types.QueryStatusCancelled:
				return nil, fmt.Errorf("query was cancelled")
			case types.QueryStatusTimeout:
				return nil, fmt.Errorf("query timed out on CloudWatch side")
			case types.QueryStatusRunning, types.QueryStatusScheduled:
				continue
			default:
				continue
			}
		}
	}
}

// ListLogGroups returns all log group names, optionally filtered by prefix.
func (c *Client) ListLogGroups(ctx context.Context, prefix string) ([]string, error) {
	var groups []string
	input := &cloudwatchlogs.DescribeLogGroupsInput{}
	if prefix != "" {
		input.LogGroupNamePrefix = aws.String(prefix)
	}

	paginator := cloudwatchlogs.NewDescribeLogGroupsPaginator(c.cw, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing log groups: %w", err)
		}
		for _, g := range page.LogGroups {
			if g.LogGroupName != nil {
				groups = append(groups, *g.LogGroupName)
			}
		}
	}

	return groups, nil
}

// DiscoverFields returns the fields discovered in the given log groups.
// Samples 20 recent log entries to collect all field names present.
func (c *Client) DiscoverFields(ctx context.Context, logGroups []string) ([]string, error) {
	// Use "fields *" to get all fields, sample 20 entries for broad coverage
	query := "fields * | limit 20"
	now := time.Now()
	start := now.Add(-1 * time.Hour)

	result, err := c.RunInsightsQuery(ctx, logGroups, query, start, now, 20)
	if err != nil {
		return nil, fmt.Errorf("discovering fields: %w", err)
	}

	seen := make(map[string]struct{})
	for _, row := range result.Results {
		for _, field := range row {
			if field.Field != nil {
				name := *field.Field
				// Skip the internal @ptr field
				if name != "@ptr" {
					seen[name] = struct{}{}
				}
			}
		}
	}

	fields := make([]string, 0, len(seen))
	for f := range seen {
		fields = append(fields, f)
	}
	return fields, nil
}


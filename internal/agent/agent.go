package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/config"
	"github.com/lokilens/lokilens/internal/errs"
	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/safety"
)

const AppName = "lokilens"

// Agent wraps the ADK runner and session service for use by the bot.
type Agent struct {
	Runner         *runner.Runner
	SessionService session.Service
	audit          *audit.Logger
	logger         *slog.Logger
}

// New creates a new LokiLens agent with the given configuration and Loki client.
func New(ctx context.Context, cfg *config.Config, lokiClient loki.Client, auditLogger *audit.Logger, logger *slog.Logger) (*Agent, error) {
	model, err := gemini.NewModel(ctx, cfg.GeminiModel, &genai.ClientConfig{
		APIKey:  cfg.GeminiAPIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("creating gemini model: %w", err)
	}

	validator := safety.NewValidator(cfg.MaxTimeRange, cfg.MaxResults)
	tools, err := buildTools(lokiClient, validator, auditLogger)
	if err != nil {
		return nil, fmt.Errorf("building tools: %w", err)
	}

	temp := float32(0.1)
	lokiAgent, err := llmagent.New(llmagent.Config{
		Name:        AppName,
		Description: "Log analysis assistant that queries Grafana Loki via natural language",
		Model:       model,
		Instruction: SystemInstruction,
		Tools:       tools,
		GenerateContentConfig: &genai.GenerateContentConfig{
			Temperature: &temp,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating llm agent: %w", err)
	}

	sessionSvc := session.InMemoryService()

	r, err := runner.New(runner.Config{
		AppName:        AppName,
		Agent:          lokiAgent,
		SessionService: sessionSvc,
	})
	if err != nil {
		return nil, fmt.Errorf("creating runner: %w", err)
	}

	a := &Agent{
		Runner:         r,
		SessionService: sessionSvc,
		audit:          auditLogger,
		logger:         logger,
	}

	// Start session cleanup goroutine
	go a.cleanupSessions(ctx)

	return a, nil
}

// EnsureSession creates a session if it doesn't exist, or returns silently if it does.
func (a *Agent) EnsureSession(ctx context.Context, userID, sessionID string) error {
	_, err := a.SessionService.Get(ctx, &session.GetRequest{
		AppName:   AppName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err == nil {
		return nil // session exists
	}

	_, err = a.SessionService.Create(ctx, &session.CreateRequest{
		AppName:   AppName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	a.audit.SessionCreated(userID, sessionID)
	return nil
}

// FileInput represents a file (image, video, etc.) to include in the agent request.
type FileInput struct {
	MimeType string
	Data     []byte
	Name     string
}

// Run executes the agent for a user message and returns the final text response.
func (a *Agent) Run(ctx context.Context, userID, sessionID, text string, files []FileInput) (string, error) {
	if err := a.EnsureSession(ctx, userID, sessionID); err != nil {
		return "", err
	}

	// Build multimodal content: text + any attached files
	var parts []*genai.Part
	if text != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}
	for _, f := range files {
		parts = append(parts, genai.NewPartFromBytes(f.Data, f.MimeType))
	}
	if len(parts) == 0 {
		parts = append(parts, genai.NewPartFromText(""))
	}
	msg := genai.NewContentFromParts(parts, genai.RoleUser)

	var finalText strings.Builder
	for event, err := range a.Runner.Run(ctx, userID, sessionID, msg, agent.RunConfig{}) {
		if err != nil {
			a.logger.Error("agent event error", "error", err)
			return "", errs.NewLLM("agent execution failed", err)
		}
		if event == nil {
			continue
		}
		if event.IsFinalResponse() && event.Content != nil {
			for _, part := range event.Content.Parts {
				if part.Text != "" {
					finalText.WriteString(part.Text)
				}
			}
		}
	}

	result := finalText.String()
	if result == "" {
		return "I wasn't able to generate a response. Try a more specific query — for example:\n• _\"Show me errors from payments in the last hour\"_\n• _\"What's the error rate for gateway?\"_\n• _\"Are there any issues right now?\"_", nil
	}
	return result, nil
}

// cleanupSessions periodically removes stale sessions to prevent memory leaks.
func (a *Agent) cleanupSessions(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := a.SessionService.List(ctx, &session.ListRequest{AppName: AppName})
			if err != nil {
				a.logger.Warn("failed to list sessions for cleanup", "error", err)
				continue
			}
			cutoff := time.Now().Add(-6 * time.Hour)
			for _, s := range resp.Sessions {
				if s.LastUpdateTime().Before(cutoff) {
					if err := a.SessionService.Delete(ctx, &session.DeleteRequest{
						AppName:   AppName,
						UserID:    s.UserID(),
						SessionID: s.ID(),
					}); err != nil {
						a.logger.Warn("failed to delete stale session", "session", s.ID(), "error", err)
					}
				}
			}
		}
	}
}

func buildTools(lokiClient loki.Client, validator *safety.Validator, auditLogger *audit.Logger) ([]tool.Tool, error) {
	handlers := NewToolHandlers(lokiClient, validator, auditLogger)

	queryLogsTool, err := functiontool.New(functiontool.Config{
		Name:        "query_logs",
		Description: "Fetch raw log lines from Loki with automatic pattern analysis. Returns individual log entries plus top_patterns (grouped similar lines with counts and pct — percentage of total), total_patterns (count of all distinct patterns — report this when present: '47 distinct error types found'), unique_labels (nested label distribution, e.g. {\"service\": {\"payments\": 45, \"orders\": 12}}), and direction ('backward'=newest first, 'forward'=oldest first — use this to correctly describe timeline order). Use this when you need actual log messages, error details, or stack traces. The top_patterns field lets you immediately identify the dominant error type without manually reading every line — use the pct field to say e.g. '78% of errors are timeouts'. unique_labels shows which services/levels dominate the results. NOT for counting or aggregation over time — use query_stats for that.",
	}, func(ctx tool.Context, input QueryLogsInput) (QueryLogsOutput, error) {
		return handlers.QueryLogs(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating query_logs tool: %w", err)
	}

	getLabelsTool, err := functiontool.New(functiontool.Config{
		Name:        "get_labels",
		Description: "List all label names in Loki (e.g. service, level, namespace, env). Call this FIRST in any new conversation before building LogQL queries — the returned list tells you which labels exist so you can identify the service label, the log level label, and the environment label. This is a single lightweight Loki API call. Use the results to pick the right label names, then call get_label_values to see the actual values.",
	}, func(ctx tool.Context, input GetLabelsInput) (GetLabelsOutput, error) {
		return handlers.GetLabels(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating get_labels tool: %w", err)
	}

	getLabelValuesTool, err := functiontool.New(functiontool.Config{
		Name:        "get_label_values",
		Description: "Get all values for a specific label (e.g. all service names, all environments, all log levels). After calling get_labels to learn label names, call this to discover: (1) all service names — pass the service label, (2) all log level values — pass the level label to see exact strings like 'error' or 'ERROR', (3) all environments — pass the environment label. Call this in parallel for multiple labels to save time. Essential for building correct LogQL queries — using the wrong label value returns 0 results.",
	}, func(ctx tool.Context, input GetLabelValuesInput) (GetLabelValuesOutput, error) {
		return handlers.GetLabelValues(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating get_label_values tool: %w", err)
	}

	queryStatsTool, err := functiontool.New(functiontool.Config{
		Name:        "query_stats",
		Description: "Run aggregation queries to get counts, rates, and trends over time with automatic trend analysis. Returns time-series data plus total_series (number of series — 0 means no matching data), step (the time granularity, e.g. '1m' or '5m'), and a summaries map with pre-computed total, avg (average count per step interval), avg_per_minute (already normalized to per-minute — use this directly for user-facing rates, no math needed), latest value, peak, peak_time (when the peak occurred), trend direction (increasing/decreasing/stable/sparse), and non_zero_pct for each series. Use this for 'how many errors?', 'error rate trend', 'compare periods', 'which service has the most errors?', 'is there a spike?', 'is it getting worse?'. The summaries field gives you instant verdicts without parsing individual data points — use avg_per_minute for 'what's the error rate', peak+peak_time for 'when was the worst point'. If total_series is 0, the query found no matching data. NOT for raw logs — use query_logs for actual log lines.",
	}, func(ctx tool.Context, input QueryStatsInput) (QueryStatsOutput, error) {
		return handlers.QueryStats(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating query_stats tool: %w", err)
	}

	return []tool.Tool{queryLogsTool, getLabelsTool, getLabelValuesTool, queryStatsTool}, nil
}

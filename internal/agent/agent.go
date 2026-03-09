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

// Run executes the agent for a user message and returns the final text response.
func (a *Agent) Run(ctx context.Context, userID, sessionID, text string) (string, error) {
	if err := a.EnsureSession(ctx, userID, sessionID); err != nil {
		return "", err
	}

	msg := genai.NewContentFromText(text, genai.RoleUser)

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
		return "I wasn't able to generate a response. Please try rephrasing your question.", nil
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
			cutoff := time.Now().Add(-2 * time.Hour)
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
		Description: "Query log lines from Loki using LogQL. Use this to search for specific log entries matching patterns, error messages, or service names within a time range.",
	}, func(ctx tool.Context, input QueryLogsInput) (QueryLogsOutput, error) {
		return handlers.QueryLogs(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating query_logs tool: %w", err)
	}

	getLabelsTool, err := functiontool.New(functiontool.Config{
		Name:        "get_labels",
		Description: "List all available label names in Loki. Use this to discover what labels exist (e.g., service, level, namespace) before constructing queries.",
	}, func(ctx tool.Context, input GetLabelsInput) (GetLabelsOutput, error) {
		return handlers.GetLabels(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating get_labels tool: %w", err)
	}

	getLabelValuesTool, err := functiontool.New(functiontool.Config{
		Name:        "get_label_values",
		Description: "Get all values for a specific label. Use this to discover exact service names, log levels, or environment names before querying logs.",
	}, func(ctx tool.Context, input GetLabelValuesInput) (GetLabelValuesOutput, error) {
		return handlers.GetLabelValues(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating get_label_values tool: %w", err)
	}

	queryStatsTool, err := functiontool.New(functiontool.Config{
		Name:        "query_stats",
		Description: "Run LogQL metric queries for aggregated statistics like error counts, log rates, or volume trends over time. Use count_over_time, rate, sum, topk, etc.",
	}, func(ctx tool.Context, input QueryStatsInput) (QueryStatsOutput, error) {
		return handlers.QueryStats(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating query_stats tool: %w", err)
	}

	discoverServicesTool, err := functiontool.New(functiontool.Config{
		Name:        "discover_services",
		Description: "Discover available services, environments, and log levels. Use this first when the user asks a broad question or when you need to know what services exist.",
	}, func(ctx tool.Context, input DiscoverServicesInput) (DiscoverServicesOutput, error) {
		return handlers.DiscoverServices(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("creating discover_services tool: %w", err)
	}

	return []tool.Tool{queryLogsTool, getLabelsTool, getLabelValuesTool, queryStatsTool, discoverServicesTool}, nil
}

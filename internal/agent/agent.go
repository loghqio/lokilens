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

	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/config"
	"github.com/lokilens/lokilens/internal/errs"
	"github.com/lokilens/lokilens/internal/logsource"
)

const AppName = "lokilens"

// Agent wraps the ADK runner and session service for use by the bot.
type Agent struct {
	Runner         *runner.Runner
	SessionService session.Service
	audit          *audit.Logger
	logger         *slog.Logger
}

// New creates a new LokiLens agent with the given configuration and log source.
func New(ctx context.Context, cfg *config.Config, source logsource.LogSource, auditLogger *audit.Logger, logger *slog.Logger) (*Agent, error) {
	clientConfig := &genai.ClientConfig{}
	if cfg.UseVertexAI() {
		clientConfig.Backend = genai.BackendVertexAI
		clientConfig.Project = cfg.GCPProject
		clientConfig.Location = cfg.GCPLocation
	} else {
		clientConfig.Backend = genai.BackendGeminiAPI
		clientConfig.APIKey = cfg.GeminiAPIKey
	}
	model, err := gemini.NewModel(ctx, cfg.GeminiModel, clientConfig)
	if err != nil {
		return nil, fmt.Errorf("creating gemini model: %w", err)
	}

	tools, err := source.Tools()
	if err != nil {
		return nil, fmt.Errorf("building tools: %w", err)
	}

	temp := float32(0.1)
	llmAgent, err := llmagent.New(llmagent.Config{
		Name:        AppName,
		Description: source.Description(),
		Model:       model,
		Instruction: source.Instruction(),
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
		Agent:          llmAgent,
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


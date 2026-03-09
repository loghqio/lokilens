package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	agentpkg "github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/bot"
	"github.com/lokilens/lokilens/internal/config"
	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/safety"
	"github.com/lokilens/lokilens/internal/store"
)

// BotManager manages multiple workspace bot instances.
type BotManager struct {
	store           store.WorkspaceStore
	sharedGeminiKey string
	appToken        string // single SLACK_APP_TOKEN (app-level, shared across workspaces)
	geminiModel     string
	auditLogger     *audit.Logger
	logger          *slog.Logger

	mu      sync.RWMutex
	bundles map[string]*WorkspaceBundle
}

// ManagerConfig holds configuration for creating a BotManager.
type ManagerConfig struct {
	Store           store.WorkspaceStore
	SharedGeminiKey string
	AppToken        string
	GeminiModel     string
	AuditLogger     *audit.Logger
	Logger          *slog.Logger
}

// New creates a new BotManager.
func New(cfg ManagerConfig) *BotManager {
	return &BotManager{
		store:           cfg.Store,
		sharedGeminiKey: cfg.SharedGeminiKey,
		appToken:        cfg.AppToken,
		geminiModel:     cfg.GeminiModel,
		auditLogger:     cfg.AuditLogger,
		logger:          cfg.Logger,
		bundles:         make(map[string]*WorkspaceBundle),
	}
}

// Start loads all active and pending workspaces and starts their bots.
// It also starts background cleanup of old usage data.
// Blocks until the context is cancelled.
func (m *BotManager) Start(ctx context.Context) error {
	// Load all non-suspended workspaces
	active, err := m.store.List(ctx, store.StatusActive)
	if err != nil {
		return fmt.Errorf("listing active workspaces: %w", err)
	}
	pending, err := m.store.List(ctx, store.StatusPendingSetup)
	if err != nil {
		return fmt.Errorf("listing pending workspaces: %w", err)
	}

	all := append(active, pending...)
	m.logger.Info("starting bot manager", "active_workspaces", len(active), "pending_workspaces", len(pending))

	for _, ws := range all {
		if err := m.startBundle(ctx, ws); err != nil {
			m.logger.Error("failed to start workspace bot", "workspace", ws.WorkspaceID, "error", err)
			continue
		}
	}

	// Background usage cleanup
	go m.cleanupUsageLoop(ctx)

	// Block until cancelled
	<-ctx.Done()

	// Stop all bots
	m.mu.Lock()
	for id, bundle := range m.bundles {
		bundle.Cancel()
		delete(m.bundles, id)
	}
	m.mu.Unlock()

	return nil
}

// AddWorkspace creates and starts a bot for a workspace.
func (m *BotManager) AddWorkspace(ctx context.Context, workspaceID string) error {
	ws, err := m.store.Get(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("getting workspace: %w", err)
	}
	return m.startBundle(ctx, ws)
}

// RemoveWorkspace stops and removes a workspace's bot.
func (m *BotManager) RemoveWorkspace(workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if bundle, ok := m.bundles[workspaceID]; ok {
		bundle.Cancel()
		delete(m.bundles, workspaceID)
		m.logger.Info("removed workspace bot", "workspace", workspaceID)
	}
}

// ReloadWorkspace stops the existing bot and starts a new one with fresh config.
func (m *BotManager) ReloadWorkspace(ctx context.Context, workspaceID string) error {
	m.RemoveWorkspace(workspaceID)
	return m.AddWorkspace(ctx, workspaceID)
}

// GetBundle returns the workspace bundle, or nil if not found.
func (m *BotManager) GetBundle(workspaceID string) *WorkspaceBundle {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bundles[workspaceID]
}

func (m *BotManager) startBundle(ctx context.Context, ws *store.Workspace) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop existing bundle if any
	if existing, ok := m.bundles[ws.WorkspaceID]; ok {
		existing.Cancel()
	}

	wsLogger := m.logger.With("workspace", ws.WorkspaceID)

	// Loki client
	lokiClient := loki.NewHTTPClient(loki.ClientConfig{
		BaseURL:    ws.LokiURL,
		APIKey:     ws.LokiAPIKey,
		Timeout:    30 * time.Second,
		MaxRetries: 2,
		Logger:     wsLogger,
	})

	// Determine Gemini key
	geminiKey := ws.GeminiAPIKey
	if geminiKey == "" {
		geminiKey = m.sharedGeminiKey
	}

	// Agent
	agentCfg := &config.Config{
		GeminiAPIKey: geminiKey,
		GeminiModel:  m.geminiModel,
		MaxTimeRange: ws.MaxTimeRange,
		MaxResults:   ws.MaxResults,
	}

	agentCtx, agentCancel := context.WithCancel(ctx)

	agent, err := agentpkg.New(agentCtx, agentCfg, lokiClient, m.auditLogger, wsLogger)
	if err != nil {
		agentCancel()
		return fmt.Errorf("creating agent for workspace %s: %w", ws.WorkspaceID, err)
	}

	// Safety components
	rl := safety.NewRateLimiter(ws.RateLimitPerUser, ws.RateLimitBurst)
	cb := safety.NewCircuitBreaker(5, 30*time.Second)

	// Usage checker (only for shared key workspaces)
	var usageChecker bot.UsageChecker
	if ws.UsesSharedKey() && m.sharedGeminiKey != "" {
		usageChecker = NewUsageChecker(m.store)
	}

	// Workspace status
	wsStatus := string(ws.Status)
	if wsStatus == "" {
		wsStatus = "active"
	}

	// Bot
	slackBot, err := bot.New(bot.Config{
		BotToken:        ws.BotToken,
		AppToken:        m.appToken,
		Agent:           agent,
		RateLimiter:     rl,
		PIIFilter:       safety.NewPIIFilter(),
		PromptGuard:     safety.NewPromptGuard(),
		CircuitBreaker:  cb,
		AuditLogger:     m.auditLogger,
		Logger:          wsLogger,
		WorkspaceID:     ws.WorkspaceID,
		WorkspaceStatus: wsStatus,
		UsageChecker:    usageChecker,
	})
	if err != nil {
		agentCancel()
		return fmt.Errorf("creating bot for workspace %s: %w", ws.WorkspaceID, err)
	}

	bundle := &WorkspaceBundle{
		Workspace:      ws,
		Bot:            slackBot,
		Agent:          agent,
		LokiClient:    lokiClient,
		RateLimiter:    rl,
		CircuitBreaker: cb,
		Cancel:         agentCancel,
	}

	m.bundles[ws.WorkspaceID] = bundle

	// Run bot in background
	go func() {
		wsLogger.Info("starting workspace bot")
		if err := slackBot.Run(agentCtx); err != nil && !errors.Is(err, context.Canceled) {
			wsLogger.Error("workspace bot exited with error", "error", err)
		}
	}()

	return nil
}

func (m *BotManager) cleanupUsageLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.store.DeleteOldUsage(ctx, 7); err != nil {
				m.logger.Error("failed to clean up old usage data", "error", err)
			}
		}
	}
}

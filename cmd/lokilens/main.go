package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/slack-go/slack"

	agentpkg "github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/bot"
	"github.com/lokilens/lokilens/internal/config"
	"github.com/lokilens/lokilens/internal/health"
	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/logsource"
	"github.com/lokilens/lokilens/internal/logsource/cwsource"
	"github.com/lokilens/lokilens/internal/logsource/lokisource"
	"github.com/lokilens/lokilens/internal/manager"
	"github.com/lokilens/lokilens/internal/oauth"
	"github.com/lokilens/lokilens/internal/safety"
	"github.com/lokilens/lokilens/internal/setup"
	"github.com/lokilens/lokilens/internal/store"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	}))
	slog.SetDefault(logger)

	if cfg.MultiTenant() {
		runMultiTenant(ctx, cfg, logger)
	} else {
		runSingleTenant(ctx, cfg, logger)
	}

	logger.Info("LokiLens shut down gracefully")
}

// runSingleTenant is the original startup path — one workspace, all config from env vars.
func runSingleTenant(ctx context.Context, cfg *config.Config, logger *slog.Logger) {
	logger.Info("LokiLens starting (single-tenant)",
		"gemini_model", cfg.GeminiModel,
		"log_backend", cfg.LogBackend,
		"vertex_ai", cfg.UseVertexAI(),
	)

	// Audit logger
	auditLogger := audit.New(logger)

	// Build log source based on backend
	var source logsource.LogSource

	if cfg.IsCloudWatch() {
		var logGroups []string
		if cfg.CWLogGroups != "" {
			for _, g := range strings.Split(cfg.CWLogGroups, ",") {
				g = strings.TrimSpace(g)
				if g != "" {
					logGroups = append(logGroups, g)
				}
			}
		}
		cwSource, err := cwsource.New(ctx, cwsource.Config{
			Region:    cfg.AWSRegion,
			LogGroups: logGroups,
			Audit:     auditLogger,
		})
		if err != nil {
			logger.Error("failed to create cloudwatch source", "error", err)
			os.Exit(1)
		}
		source = cwSource
	} else {
		lokiClient := loki.NewHTTPClient(loki.ClientConfig{
			BaseURL:    cfg.LokiBaseURL,
			APIKey:     cfg.LokiAPIKey,
			Timeout:    cfg.LokiTimeout,
			MaxRetries: cfg.LokiMaxRetries,
			Logger:     logger,
		})
		validator := safety.NewValidator(cfg.MaxTimeRange, cfg.MaxResults)
		source = lokisource.New(lokiClient, validator, auditLogger)
	}

	// Validate backend connectivity before starting
	checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := source.HealthCheck(checkCtx); err != nil {
		checkCancel()
		logger.Error("log backend health check failed", "backend", source.Name(), "error", err)
		os.Exit(1)
	}
	checkCancel()
	logger.Info("log backend connected", "backend", source.Name())

	// ADK agent
	agent, err := agentpkg.New(ctx, cfg, source, auditLogger, logger)
	if err != nil {
		logger.Error("failed to create agent", "error", err)
		os.Exit(1)
	}

	// Health server for Kubernetes probes
	healthSrv := health.New(health.Config{
		Addr:   cfg.HealthAddr,
		Source: source,
		Logger: logger,
	})
	go healthSrv.Run(ctx)

	// Slack bot
	slackBot, err := bot.New(bot.Config{
		BotToken:       cfg.SlackBotToken,
		AppToken:       cfg.SlackAppToken,
		Agent:          agent,
		PIIFilter:      safety.NewPIIFilter(),
		PromptGuard:    safety.NewPromptGuard(),
		CircuitBreaker: safety.NewCircuitBreaker(5, 30*time.Second),
		AuditLogger:    auditLogger,
		Logger:         logger,
	})
	if err != nil {
		logger.Error("failed to create slack bot", "error", err)
		os.Exit(1)
	}

	// Run the bot (blocks until context cancelled)
	if err := slackBot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("bot exited with error", "error", err)
		os.Exit(1)
	}
}

// runMultiTenant starts the BotManager, HTTP server (OAuth + health), and manages
// multiple workspace bots from the database.
func runMultiTenant(ctx context.Context, cfg *config.Config, logger *slog.Logger) {
	logger.Info("LokiLens starting (multi-tenant)",
		"gemini_model", cfg.GeminiModel,
		"base_url", cfg.BaseURL,
	)

	// Encryption cipher for secrets at rest
	cipher, err := store.NewCipher(cfg.EncryptionKey)
	if err != nil {
		logger.Error("failed to create cipher", "error", err)
		os.Exit(1)
	}

	// Database
	db, err := store.NewPostgresStore(cfg.DatabaseURL, cipher)
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Run migrations
	if err := db.Migrate(ctx); err != nil {
		logger.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	// Seed Grey's workspace from legacy env vars if present
	seedWorkspaceFromEnv(ctx, cfg, db, logger)

	// Audit logger
	auditLogger := audit.New(logger)

	// Determine shared Gemini key
	sharedKey := cfg.GeminiSharedKey
	if sharedKey == "" {
		sharedKey = cfg.GeminiAPIKey // fallback to legacy env var
	}

	// Bot manager
	mgr := manager.New(manager.ManagerConfig{
		Store:           db,
		SharedGeminiKey: sharedKey,
		AppToken:        cfg.SlackAppToken,
		GeminiModel:     cfg.GeminiModel,
		GCPProject:      cfg.GCPProject,
		GCPLocation:     cfg.GCPLocation,
		AuditLogger:     auditLogger,
		Logger:          logger,
	})

	// Setup wizard
	wizard := setup.New(db, mgr, logger)

	// OAuth handler
	oauthHandler := oauth.NewHandler(oauth.Config{
		ClientID:      cfg.SlackClientID,
		ClientSecret:  cfg.SlackClientSecret,
		SigningSecret: cfg.SlackSigningSecret,
		AppToken:      cfg.SlackAppToken,
		BaseURL:       cfg.BaseURL,
		Store:         db,
		Wizard:        wizard,
		Manager:       mgr,
		Logger:        logger,
	})

	// Wire up the Slack API lookup so OAuth/interactions can find workspace bots
	oauthHandler.SlackAPIForWorkspace = func(workspaceID string) *slack.Client {
		bundle := mgr.GetBundle(workspaceID)
		if bundle != nil {
			return bundle.Bot.API()
		}
		return nil
	}

	// HTTP server (health + OAuth routes)
	mux := http.NewServeMux()

	// Health endpoints — use a no-op checker since multi-tenant checks DB instead
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.DB().PingContext(r.Context()); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"db_unhealthy"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	oauthHandler.RegisterRoutes(mux)

	httpServer := &http.Server{
		Addr:         cfg.HealthAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Start HTTP server
	go func() {
		logger.Info("HTTP server starting", "addr", cfg.HealthAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server failed", "error", err)
		}
	}()

	// Start bot manager (blocks until context cancelled)
	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("bot manager exited with error", "error", err)
	}

	// Shut down HTTP server
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	httpServer.Shutdown(shutdownCtx)
}

// seedWorkspaceFromEnv creates or updates Grey's workspace from legacy env vars.
// This ensures backwards compatibility — set the old env vars plus DATABASE_URL
// and Grey's workspace auto-seeds into the database.
func seedWorkspaceFromEnv(ctx context.Context, cfg *config.Config, db store.WorkspaceStore, logger *slog.Logger) {
	if cfg.SlackBotToken == "" || cfg.LokiBaseURL == "" {
		return // no legacy config to seed
	}

	// Check if already seeded — we need a workspace ID.
	// For seeding, use a well-known ID or derive from the bot token.
	// We'll use "seed" as a sentinel; the real ID will come from Slack.
	// Instead, check if there are any active workspaces — if so, skip seeding.
	existing, err := db.List(ctx, store.StatusActive)
	if err == nil && len(existing) > 0 {
		logger.Info("workspaces already exist, skipping env seed")
		return
	}

	// Determine Gemini key for the seed workspace
	geminiKey := cfg.GeminiAPIKey

	ws := &store.Workspace{
		WorkspaceID:      "seed-workspace",
		TeamName:         "Grey",
		BotToken:         cfg.SlackBotToken,
		LokiURL:          cfg.LokiBaseURL,
		LokiAPIKey:       cfg.LokiAPIKey,
		GeminiAPIKey:     geminiKey,
		DailyQueryLimit:  100,
		MaxTimeRange:     cfg.MaxTimeRange,
		MaxResults:       cfg.MaxResults,
		InstalledBy:      "env-seed",
		Status:           store.StatusActive,
	}

	if err := db.Create(ctx, ws); err != nil {
		// Might already exist — try update
		if err2 := db.Update(ctx, ws); err2 != nil {
			logger.Warn("failed to seed workspace from env", "error", err, "update_error", err2)
			return
		}
	}

	logger.Info("seeded Grey workspace from environment variables")
}

func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

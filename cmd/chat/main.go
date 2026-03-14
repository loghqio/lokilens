package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	agentpkg "github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/config"
	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/logsource"
	"github.com/lokilens/lokilens/internal/logsource/cwsource"
	"github.com/lokilens/lokilens/internal/logsource/lokisource"
	"github.com/lokilens/lokilens/internal/safety"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.LoadAgent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	auditLogger := audit.New(logger)

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
			fmt.Fprintf(os.Stderr, "cloudwatch init error: %v\n", err)
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

	agent, err := agentpkg.New(ctx, cfg, source, auditLogger, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent init error: %v\n", err)
		os.Exit(1)
	}

	const userID = "cli-user"
	sessionNum := 1
	sessionID := "cli-session-1"

	fmt.Printf("LokiLens Chat (backend: %s)\n", source.Name())
	fmt.Printf("Model: %s\n", cfg.GeminiModel)
	fmt.Println("Commands: new (fresh session), exit (quit)")
	fmt.Println(strings.Repeat("─", 60))

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nyou> ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if text == "exit" || text == "quit" {
			break
		}
		if text == "new" {
			sessionNum++
			sessionID = "cli-session-" + strconv.Itoa(sessionNum)
			fmt.Printf("(new session: %s)\n", sessionID)
			continue
		}

		fmt.Println()
		response, err := agent.Run(ctx, userID, sessionID, text, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			continue
		}

		fmt.Println("lokilens>")
		fmt.Println(response)
		fmt.Println(strings.Repeat("─", 60))
	}

	fmt.Println("\nBye.")
}

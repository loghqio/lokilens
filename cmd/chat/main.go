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
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.LoadAgent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		fmt.Fprintf(os.Stderr, "Required: GEMINI_API_KEY, LOKI_BASE_URL\n")
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	lokiClient := loki.NewHTTPClient(loki.ClientConfig{
		BaseURL:    cfg.LokiBaseURL,
		APIKey:     cfg.LokiAPIKey,
		Timeout:    cfg.LokiTimeout,
		MaxRetries: cfg.LokiMaxRetries,
		Logger:     logger,
	})

	auditLogger := audit.New(logger)

	agent, err := agentpkg.New(ctx, cfg, lokiClient, auditLogger, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent init error: %v\n", err)
		os.Exit(1)
	}

	const userID = "cli-user"
	sessionNum := 1
	sessionID := "cli-session-1"

	fmt.Println("LokiLens Chat (real Gemini + real Loki)")
	fmt.Printf("Model: %s | Loki: %s\n", cfg.GeminiModel, cfg.LokiBaseURL)
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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	agentpkg "github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/config"
	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/logsource"
	"github.com/lokilens/lokilens/internal/logsource/cwsource"
	"github.com/lokilens/lokilens/internal/logsource/lokisource"
	"github.com/lokilens/lokilens/internal/safety"
)

type Scenario struct {
	Name     string   `json:"name"`
	Query    string   `json:"query"`
	Expect   []string `json:"expect"`
	Reject   []string `json:"reject"`
	MaxSecs  int      `json:"max_secs"`
}

type SuiteFile struct {
	Scenarios []Scenario `json:"scenarios"`
}

type Result struct {
	Name     string
	Query    string
	Passed   bool
	Duration time.Duration
	Response string
	Failures []string
}

func main() {
	suiteFile := "e2e/scenarios.json"
	if len(os.Args) > 1 {
		suiteFile = os.Args[1]
	}

	data, err := os.ReadFile(suiteFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read scenarios file %s: %v\n", suiteFile, err)
		os.Exit(1)
	}

	var suite SuiteFile
	if err := json.Unmarshal(data, &suite); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse scenarios: %v\n", err)
		os.Exit(1)
	}

	if len(suite.Scenarios) == 0 {
		fmt.Fprintf(os.Stderr, "no scenarios found in %s\n", suiteFile)
		os.Exit(1)
	}

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

	fmt.Printf("LokiLens E2E — %d scenarios from %s\n", len(suite.Scenarios), suiteFile)
	fmt.Printf("Model: %s | Backend: %s\n", cfg.GeminiModel, source.Name())
	fmt.Println(strings.Repeat("=", 70))

	var results []Result
	passed, failed := 0, 0

	for i, sc := range suite.Scenarios {
		fmt.Printf("\n[%d/%d] %s\n", i+1, len(suite.Scenarios), sc.Name)
		fmt.Printf("  Query: %s\n", sc.Query)

		maxDur := 60 * time.Second
		if sc.MaxSecs > 0 {
			maxDur = time.Duration(sc.MaxSecs) * time.Second
		}

		scCtx, scCancel := context.WithTimeout(ctx, maxDur)

		sessionID := fmt.Sprintf("e2e-%d", i)
		start := time.Now()
		response, err := agent.Run(scCtx, "e2e-user", sessionID, sc.Query, nil)
		duration := time.Since(start)
		scCancel()

		r := Result{
			Name:     sc.Name,
			Query:    sc.Query,
			Duration: duration,
		}

		if err != nil {
			r.Failures = append(r.Failures, fmt.Sprintf("agent error: %v", err))
			r.Response = ""
		} else {
			r.Response = response
			lower := strings.ToLower(response)

			for _, exp := range sc.Expect {
				if !strings.Contains(lower, strings.ToLower(exp)) {
					r.Failures = append(r.Failures, fmt.Sprintf("expected %q not found in response", exp))
				}
			}

			for _, rej := range sc.Reject {
				if strings.Contains(lower, strings.ToLower(rej)) {
					r.Failures = append(r.Failures, fmt.Sprintf("rejected %q found in response", rej))
				}
			}
		}

		r.Passed = len(r.Failures) == 0
		results = append(results, r)

		if r.Passed {
			passed++
			fmt.Printf("  PASS (%s)\n", duration.Round(time.Millisecond))
		} else {
			failed++
			fmt.Printf("  FAIL (%s)\n", duration.Round(time.Millisecond))
			for _, f := range r.Failures {
				fmt.Printf("    - %s\n", f)
			}
		}

		// Show a preview of the response
		preview := r.Response
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		if preview != "" {
			fmt.Printf("  Response: %s\n", preview)
		}
	}

	// Summary
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("Results: %d passed, %d failed, %d total\n", passed, failed, len(results))

	// Write full results to file
	reportFile := "e2e/results.json"
	type jsonResult struct {
		Name     string   `json:"name"`
		Query    string   `json:"query"`
		Passed   bool     `json:"passed"`
		Duration string   `json:"duration"`
		Response string   `json:"response"`
		Failures []string `json:"failures,omitempty"`
	}

	var report []jsonResult
	for _, r := range results {
		report = append(report, jsonResult{
			Name:     r.Name,
			Query:    r.Query,
			Passed:   r.Passed,
			Duration: r.Duration.Round(time.Millisecond).String(),
			Response: r.Response,
			Failures: r.Failures,
		})
	}

	reportData, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(reportFile, reportData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write results to %s: %v\n", reportFile, err)
	} else {
		fmt.Printf("Full results written to %s\n", reportFile)
	}

	if failed > 0 {
		os.Exit(1)
	}
}

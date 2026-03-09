package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"
)

var (
	services = []string{"payments", "orders", "users", "gateway", "inventory"}
	levels   = []string{"info", "warn", "error", "debug"}
	envs     = []string{"production", "staging"}

	infoMessages = []string{
		"request completed successfully",
		"health check passed",
		"connection pool initialized",
		"cache hit for key",
		"processing batch of records",
		"scheduled task completed",
		"metrics exported",
		"configuration reloaded",
	}

	warnMessages = []string{
		"response time exceeded threshold",
		"connection pool nearing capacity",
		"retry attempt for failed request",
		"deprecated API endpoint called",
		"cache miss rate above threshold",
		"disk usage at 80 percent",
	}

	errorMessages = []string{
		"failed to process payment: connection refused",
		"database query timeout after 30s",
		"authentication failed for user",
		"upstream service returned 503",
		"out of memory: cannot allocate",
		"TLS handshake failed",
		"rate limit exceeded for client",
		"failed to serialize response",
		"null pointer exception in handler",
		"circuit breaker open for dependency",
	}

	endpoints = []string{
		"POST /api/v1/payments/charge",
		"GET /api/v1/orders",
		"POST /api/v1/users/login",
		"GET /api/v1/inventory/check",
		"POST /api/v1/gateway/proxy",
		"GET /api/v1/health",
		"PUT /api/v1/orders/status",
		"DELETE /api/v1/users/session",
	}
)

type lokiPushRequest struct {
	Streams []lokiStream `json:"streams"`
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

func main() {
	lokiURL := os.Getenv("LOKI_URL")
	if lokiURL == "" {
		lokiURL = "http://localhost:3100"
	}

	pushURL := lokiURL + "/loki/api/v1/push"
	log.Printf("Log generator starting, pushing to %s", pushURL)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	errorSpikeTicker := time.NewTicker(10 * time.Minute)
	defer errorSpikeTicker.Stop()

	var errorSpikeService string
	var errorSpikeUntil time.Time

	for {
		select {
		case <-errorSpikeTicker.C:
			// Trigger an error spike for a random service
			errorSpikeService = services[rand.Intn(len(services))]
			errorSpikeUntil = time.Now().Add(2 * time.Minute)
			log.Printf("Error spike started for service=%s", errorSpikeService)

		case <-ticker.C:
			batch := lokiPushRequest{}

			// Generate 3-8 log entries per tick
			count := rand.Intn(6) + 3
			for range count {
				service := services[rand.Intn(len(services))]
				env := envs[rand.Intn(len(envs))]
				level := pickLevel(service, errorSpikeService, errorSpikeUntil)
				msg := pickMessage(level)
				traceID := fmt.Sprintf("%016x", rand.Int63())
				endpoint := endpoints[rand.Intn(len(endpoints))]
				duration := rand.Intn(2000) + 5
				statusCode := pickStatusCode(level)

				logLine := fmt.Sprintf(
					`{"timestamp":"%s","level":"%s","service":"%s","msg":"%s","trace_id":"%s","endpoint":"%s","duration_ms":%d,"status_code":%d,"env":"%s"}`,
					time.Now().Format(time.RFC3339Nano),
					level,
					service,
					msg,
					traceID,
					endpoint,
					duration,
					statusCode,
					env,
				)

				stream := lokiStream{
					Stream: map[string]string{
						"service": service,
						"level":   level,
						"env":     env,
						"job":     "loggen",
					},
					Values: [][]string{
						{fmt.Sprintf("%d", time.Now().UnixNano()), logLine},
					},
				}
				batch.Streams = append(batch.Streams, stream)
			}

			if err := pushToLoki(pushURL, batch); err != nil {
				log.Printf("Failed to push logs: %v", err)
			}
		}
	}
}

func pickLevel(service, errorSpikeService string, spikeUntil time.Time) string {
	// During error spike, the affected service generates mostly errors
	if service == errorSpikeService && time.Now().Before(spikeUntil) {
		roll := rand.Intn(100)
		if roll < 60 {
			return "error"
		}
		if roll < 80 {
			return "warn"
		}
		return "info"
	}

	// Normal distribution
	roll := rand.Intn(100)
	switch {
	case roll < 60:
		return "info"
	case roll < 75:
		return "debug"
	case roll < 90:
		return "warn"
	default:
		return "error"
	}
}

func pickMessage(level string) string {
	switch level {
	case "error":
		return errorMessages[rand.Intn(len(errorMessages))]
	case "warn":
		return warnMessages[rand.Intn(len(warnMessages))]
	default:
		return infoMessages[rand.Intn(len(infoMessages))]
	}
}

func pickStatusCode(level string) int {
	switch level {
	case "error":
		codes := []int{500, 502, 503, 504, 408, 429}
		return codes[rand.Intn(len(codes))]
	case "warn":
		codes := []int{200, 201, 429, 408}
		return codes[rand.Intn(len(codes))]
	default:
		return 200
	}
}

func pushToLoki(url string, batch lokiPushRequest) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("marshaling: %w", err)
	}

	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("posting: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("loki returned %d", resp.StatusCode)
	}
	return nil
}

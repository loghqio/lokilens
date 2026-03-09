package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Slack
	SlackBotToken string
	SlackAppToken string

	// Gemini / ADK
	GeminiAPIKey string
	GeminiModel  string

	// Loki
	LokiBaseURL    string
	LokiAPIKey     string
	LokiTimeout    time.Duration
	LokiMaxRetries int

	// Safety
	MaxTimeRange     time.Duration
	MaxResults       int
	RateLimitPerUser int
	RateLimitBurst   int

	// Server
	HealthAddr string

	// Logging
	LogLevel string

	// License
	LicenseKey string

	// Multi-tenant (all optional — when DatabaseURL is set, workspaces are loaded from DB)
	DatabaseURL        string // DATABASE_URL (Neon PostgreSQL connection string)
	EncryptionKey      string // ENCRYPTION_KEY (32-byte hex for AES-256-GCM)
	GeminiSharedKey    string // GEMINI_SHARED_KEY (shared Gemini key for free tier)
	SlackClientID      string // SLACK_CLIENT_ID (OAuth)
	SlackClientSecret  string // SLACK_CLIENT_SECRET (OAuth)
	SlackSigningSecret string // SLACK_SIGNING_SECRET (request verification)
	BaseURL            string // BASE_URL (public URL for OAuth callbacks)
}

func Load() (*Config, error) {
	loadDotEnv()

	cfg := &Config{
		SlackBotToken:  os.Getenv("SLACK_BOT_TOKEN"),
		SlackAppToken:  os.Getenv("SLACK_APP_TOKEN"),
		GeminiAPIKey:   os.Getenv("GEMINI_API_KEY"),
		GeminiModel:    envOrDefault("GEMINI_MODEL", "gemini-2.5-flash"),
		LokiBaseURL:    os.Getenv("LOKI_BASE_URL"),
		LokiAPIKey:     os.Getenv("LOKI_API_KEY"),
		LokiTimeout:    envDuration("LOKI_TIMEOUT", 30*time.Second),
		LokiMaxRetries: envInt("LOKI_MAX_RETRIES", 2),
		MaxTimeRange:   envDuration("MAX_TIME_RANGE", 24*time.Hour),
		MaxResults:     envInt("MAX_RESULTS", 500),
		RateLimitPerUser: envInt("RATE_LIMIT_PER_USER", 20),
		RateLimitBurst:   envInt("RATE_LIMIT_BURST", 5),
		HealthAddr:       envOrDefault("HEALTH_ADDR", ":8080"),
		LogLevel:         envOrDefault("LOG_LEVEL", "info"),
		LicenseKey:       os.Getenv("LICENSE_KEY"),

		// Multi-tenant
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		EncryptionKey:      os.Getenv("ENCRYPTION_KEY"),
		GeminiSharedKey:    os.Getenv("GEMINI_SHARED_KEY"),
		SlackClientID:      os.Getenv("SLACK_CLIENT_ID"),
		SlackClientSecret:  os.Getenv("SLACK_CLIENT_SECRET"),
		SlackSigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		BaseURL:            os.Getenv("BASE_URL"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// LoadAgent loads config requiring only Gemini and Loki settings (no Slack).
func LoadAgent() (*Config, error) {
	loadDotEnv()

	cfg := &Config{
		GeminiAPIKey:   os.Getenv("GEMINI_API_KEY"),
		GeminiModel:    envOrDefault("GEMINI_MODEL", "gemini-2.5-flash"),
		LokiBaseURL:    os.Getenv("LOKI_BASE_URL"),
		LokiAPIKey:     os.Getenv("LOKI_API_KEY"),
		LokiTimeout:    envDuration("LOKI_TIMEOUT", 30*time.Second),
		LokiMaxRetries: envInt("LOKI_MAX_RETRIES", 2),
		MaxTimeRange:   envDuration("MAX_TIME_RANGE", 24*time.Hour),
		MaxResults:     envInt("MAX_RESULTS", 500),
		LogLevel:       envOrDefault("LOG_LEVEL", "info"),
	}

	var missing []string
	if cfg.GeminiAPIKey == "" {
		missing = append(missing, "GEMINI_API_KEY")
	}
	if cfg.LokiBaseURL == "" {
		missing = append(missing, "LOKI_BASE_URL")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

// MultiTenant returns true if the app is configured for multi-tenant mode.
func (c *Config) MultiTenant() bool {
	return c.DatabaseURL != ""
}

func (c *Config) validate() error {
	if c.MultiTenant() {
		return c.validateMultiTenant()
	}
	return c.validateSingleTenant()
}

func (c *Config) validateSingleTenant() error {
	var missing []string
	if c.SlackBotToken == "" {
		missing = append(missing, "SLACK_BOT_TOKEN")
	}
	if c.SlackAppToken == "" {
		missing = append(missing, "SLACK_APP_TOKEN")
	}
	if c.GeminiAPIKey == "" {
		missing = append(missing, "GEMINI_API_KEY")
	}
	if c.LokiBaseURL == "" {
		missing = append(missing, "LOKI_BASE_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	if !strings.HasPrefix(c.SlackBotToken, "xoxb-") {
		return fmt.Errorf("SLACK_BOT_TOKEN must start with xoxb-")
	}
	if !strings.HasPrefix(c.SlackAppToken, "xapp-") {
		return fmt.Errorf("SLACK_APP_TOKEN must start with xapp-")
	}

	return nil
}

func (c *Config) validateMultiTenant() error {
	var missing []string
	if c.EncryptionKey == "" {
		missing = append(missing, "ENCRYPTION_KEY")
	}
	if c.SlackAppToken == "" {
		missing = append(missing, "SLACK_APP_TOKEN")
	}
	if c.SlackClientID == "" {
		missing = append(missing, "SLACK_CLIENT_ID")
	}
	if c.SlackClientSecret == "" {
		missing = append(missing, "SLACK_CLIENT_SECRET")
	}
	if c.SlackSigningSecret == "" {
		missing = append(missing, "SLACK_SIGNING_SECRET")
	}
	if c.BaseURL == "" {
		missing = append(missing, "BASE_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("multi-tenant mode requires: %s", strings.Join(missing, ", "))
	}

	if !strings.HasPrefix(c.SlackAppToken, "xapp-") {
		return fmt.Errorf("SLACK_APP_TOKEN must start with xapp-")
	}
	if len(c.EncryptionKey) != 64 {
		return fmt.Errorf("ENCRYPTION_KEY must be 64 hex characters (32 bytes)")
	}

	return nil
}

func loadDotEnv() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}

func envInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

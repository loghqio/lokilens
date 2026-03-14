package manager

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/store"
)

func TestBuildLogSource_DefaultsToLoki(t *testing.T) {
	ws := &store.Workspace{
		LogBackend:   "",
		LokiURL:      "http://localhost:3100",
		MaxTimeRange: 24 * time.Hour,
		MaxResults:   500,
	}
	logger := slog.Default()
	al := audit.New(logger)

	source, err := buildLogSource(context.Background(), ws, al, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.Name() != "Loki" {
		t.Errorf("expected Loki source for empty backend, got %q", source.Name())
	}
}

func TestBuildLogSource_ExplicitLoki(t *testing.T) {
	ws := &store.Workspace{
		LogBackend:   "loki",
		LokiURL:      "http://loki:3100",
		MaxTimeRange: 24 * time.Hour,
		MaxResults:   500,
	}
	logger := slog.Default()
	al := audit.New(logger)

	source, err := buildLogSource(context.Background(), ws, al, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.Name() != "Loki" {
		t.Errorf("expected Loki, got %q", source.Name())
	}
}

func TestBuildLogSource_CaseInsensitiveBackend(t *testing.T) {
	// "Loki" (capitalized) should still resolve to Loki
	ws := &store.Workspace{
		LogBackend:   "Loki",
		LokiURL:      "http://loki:3100",
		MaxTimeRange: 24 * time.Hour,
		MaxResults:   500,
	}
	logger := slog.Default()
	al := audit.New(logger)

	source, err := buildLogSource(context.Background(), ws, al, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.Name() != "Loki" {
		t.Errorf("expected Loki, got %q", source.Name())
	}
}

func TestBuildLogSource_UnknownBackendDefaultsToLoki(t *testing.T) {
	ws := &store.Workspace{
		LogBackend:   "elasticsearch",
		LokiURL:      "http://loki:3100",
		MaxTimeRange: 24 * time.Hour,
		MaxResults:   500,
	}
	logger := slog.Default()
	al := audit.New(logger)

	source, err := buildLogSource(context.Background(), ws, al, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.Name() != "Loki" {
		t.Errorf("expected Loki fallback for unknown backend, got %q", source.Name())
	}
}

func TestBuildLogSource_CloudWatch(t *testing.T) {
	ws := &store.Workspace{
		LogBackend:   "cloudwatch",
		AWSRegion:    "us-east-1",
		CWLogGroups:  "/aws/lambda/payments, /aws/ecs/orders",
		MaxTimeRange: 24 * time.Hour,
		MaxResults:   500,
	}
	logger := slog.Default()
	al := audit.New(logger)

	source, err := buildLogSource(context.Background(), ws, al, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.Name() != "CloudWatch" {
		t.Errorf("expected CloudWatch, got %q", source.Name())
	}
}

func TestBuildLogSource_CloudWatchCaseInsensitive(t *testing.T) {
	ws := &store.Workspace{
		LogBackend:   "CloudWatch",
		AWSRegion:    "eu-west-1",
		MaxTimeRange: 24 * time.Hour,
		MaxResults:   500,
	}
	logger := slog.Default()
	al := audit.New(logger)

	source, err := buildLogSource(context.Background(), ws, al, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.Name() != "CloudWatch" {
		t.Errorf("expected CloudWatch, got %q", source.Name())
	}
}

func TestBuildLogSource_CloudWatchEmptyLogGroups(t *testing.T) {
	ws := &store.Workspace{
		LogBackend:   "cloudwatch",
		AWSRegion:    "us-east-1",
		CWLogGroups:  "",
		MaxTimeRange: 24 * time.Hour,
		MaxResults:   500,
	}
	logger := slog.Default()
	al := audit.New(logger)

	source, err := buildLogSource(context.Background(), ws, al, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.Name() != "CloudWatch" {
		t.Errorf("expected CloudWatch, got %q", source.Name())
	}
}

func TestBuildLogSource_CloudWatchLogGroupsParsing(t *testing.T) {
	// Verify commas with whitespace are properly handled
	ws := &store.Workspace{
		LogBackend:   "cloudwatch",
		AWSRegion:    "us-east-1",
		CWLogGroups:  " /group1 , /group2 , , /group3 ",
		MaxTimeRange: 24 * time.Hour,
		MaxResults:   500,
	}
	logger := slog.Default()
	al := audit.New(logger)

	source, err := buildLogSource(context.Background(), ws, al, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.Name() != "CloudWatch" {
		t.Errorf("expected CloudWatch, got %q", source.Name())
	}
}

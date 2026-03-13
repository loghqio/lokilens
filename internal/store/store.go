package store

import (
	"context"
	"time"
)

// WorkspaceStatus represents the lifecycle state of a workspace.
type WorkspaceStatus string

const (
	StatusPendingSetup WorkspaceStatus = "pending_setup"
	StatusActive       WorkspaceStatus = "active"
	StatusSuspended    WorkspaceStatus = "suspended"
)

// Workspace holds decrypted workspace configuration.
type Workspace struct {
	WorkspaceID      string
	TeamName         string
	BotToken         string // decrypted
	LokiURL          string
	LokiAPIKey       string          // decrypted, "" = no auth
	GeminiAPIKey     string          // decrypted, "" = use shared key
	DailyQueryLimit  int
	MaxTimeRange time.Duration
	MaxResults       int
	InstalledBy      string
	Status           WorkspaceStatus
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// UsesSharedKey returns true if the workspace uses the shared Gemini API key.
func (w *Workspace) UsesSharedKey() bool {
	return w.GeminiAPIKey == ""
}

// WorkspaceStore defines CRUD and usage operations for workspace configs.
type WorkspaceStore interface {
	Get(ctx context.Context, workspaceID string) (*Workspace, error)
	List(ctx context.Context, status WorkspaceStatus) ([]*Workspace, error)
	Create(ctx context.Context, w *Workspace) error
	Update(ctx context.Context, w *Workspace) error
	Delete(ctx context.Context, workspaceID string) error

	// IncrementUsage atomically increments the daily query count and returns
	// the current count and the workspace's daily limit.
	IncrementUsage(ctx context.Context, workspaceID string) (count int, limit int, err error)

	// DeleteOldUsage removes usage rows older than the given number of days.
	DeleteOldUsage(ctx context.Context, daysToKeep int) error

	// Close releases database resources.
	Close() error
}

package manager

import (
	"context"

	"github.com/lokilens/lokilens/internal/store"
)

// UsageChecker tracks and enforces daily query limits.
// Implements bot.UsageChecker.
type UsageChecker struct {
	store store.WorkspaceStore
}

// NewUsageChecker creates a UsageChecker backed by the workspace store.
func NewUsageChecker(s store.WorkspaceStore) *UsageChecker {
	return &UsageChecker{store: s}
}

// Check increments the daily usage and returns the current count and limit.
func (u *UsageChecker) Check(ctx context.Context, workspaceID string) (int, int, error) {
	return u.store.IncrementUsage(ctx, workspaceID)
}

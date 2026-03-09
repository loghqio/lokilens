package license

import (
	"os"
	"strconv"
	"time"
)

const (
	graceDuration = 7 * 24 * time.Hour
	graceFile     = "/tmp/.lokilens-grace"
)

// GraceTracker persists the first-seen expiry timestamp so the
// 7-day grace period survives container restarts.
type GraceTracker struct {
	path string
}

// NewGraceTracker creates a grace tracker using the default path.
func NewGraceTracker() *GraceTracker {
	return &GraceTracker{path: graceFile}
}

// RecordExpiry writes the current timestamp if no grace file exists yet.
// If the file already exists, this is a no-op (preserves the original deadline).
func (g *GraceTracker) RecordExpiry() error {
	if _, err := os.Stat(g.path); err == nil {
		return nil // already recorded
	}
	return os.WriteFile(g.path, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
}

// Deadline returns when the grace period ends (first-seen + 7 days).
func (g *GraceTracker) Deadline() (time.Time, error) {
	data, err := os.ReadFile(g.path)
	if err != nil {
		return time.Time{}, err
	}
	unix, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(unix, 0).Add(graceDuration), nil
}

// IsWithinGrace returns true if the grace period has been recorded
// and the current time is before the deadline.
func (g *GraceTracker) IsWithinGrace() bool {
	deadline, err := g.Deadline()
	if err != nil {
		return false
	}
	return time.Now().Before(deadline)
}

// Clear removes the grace file. Call when a license becomes valid again.
func (g *GraceTracker) Clear() error {
	if err := os.Remove(g.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

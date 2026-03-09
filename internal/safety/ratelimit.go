package safety

import (
	"fmt"
	"sync"

	"golang.org/x/time/rate"
)

const maxTrackedUsers = 10000

// RateLimiter enforces per-user request rate limits.
type RateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	perSec   rate.Limit
	burst    int
}

// NewRateLimiter creates a rate limiter.
// perMinute is the sustained rate (requests per minute).
// burst is the maximum burst size.
func NewRateLimiter(perMinute, burst int) *RateLimiter {
	return &RateLimiter{
		limiters: make(map[string]*rate.Limiter),
		perSec:   rate.Limit(float64(perMinute) / 60.0),
		burst:    burst,
	}
}

// Allow checks if a request from the given user is allowed.
func (rl *RateLimiter) Allow(userID string) error {
	rl.mu.Lock()
	limiter, exists := rl.limiters[userID]
	if !exists {
		// Evict all entries if map grows too large to prevent memory leak
		if len(rl.limiters) >= maxTrackedUsers {
			clear(rl.limiters)
		}
		limiter = rate.NewLimiter(rl.perSec, rl.burst)
		rl.limiters[userID] = limiter
	}
	rl.mu.Unlock()

	if !limiter.Allow() {
		return fmt.Errorf("rate limit exceeded — please wait before sending more requests")
	}
	return nil
}

package safety

import (
	"sync"
	"time"

	"github.com/lokilens/lokilens/internal/errs"
)

type circuitState int

const (
	stateClosed   circuitState = iota // normal operation
	stateOpen                         // failing fast, rejecting calls
	stateHalfOpen                     // allowing one probe request
)

// CircuitBreaker implements the circuit breaker pattern to prevent cascading
// failures when the LLM backend is unhealthy.
type CircuitBreaker struct {
	mu sync.Mutex

	state            circuitState
	consecutiveFails int
	lastFailTime     time.Time

	// Configuration
	failThreshold int           // consecutive failures to trip open
	resetTimeout  time.Duration // how long to wait before half-open probe
}

// NewCircuitBreaker creates a circuit breaker that opens after failThreshold
// consecutive failures and probes again after resetTimeout.
func NewCircuitBreaker(failThreshold int, resetTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:         stateClosed,
		failThreshold: failThreshold,
		resetTimeout:  resetTimeout,
	}
}

// Allow checks whether a request should be permitted. Returns nil if allowed,
// or CircuitOpenError if the circuit is open and the caller should fail fast.
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case stateClosed:
		return nil
	case stateHalfOpen:
		// Already have a probe in flight — reject additional requests
		return &errs.CircuitOpenError{}
	case stateOpen:
		if time.Since(cb.lastFailTime) >= cb.resetTimeout {
			// Enough time has passed — allow one probe request
			cb.state = stateHalfOpen
			return nil
		}
		return &errs.CircuitOpenError{}
	}
	return nil
}

// RecordSuccess signals that a request succeeded.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveFails = 0
	cb.state = stateClosed
}

// RecordFailure signals that a request failed.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveFails++
	cb.lastFailTime = time.Now()

	if cb.consecutiveFails >= cb.failThreshold {
		cb.state = stateOpen
	}
}

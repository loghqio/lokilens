package safety

import (
	"testing"
	"time"
)

func TestCircuitBreaker_ClosedAllows(t *testing.T) {
	cb := NewCircuitBreaker(3, 1*time.Second)
	if err := cb.Allow(); err != nil {
		t.Errorf("closed circuit should allow: %v", err)
	}
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := NewCircuitBreaker(3, 1*time.Second)
	cb.RecordFailure()
	cb.RecordFailure()
	cb.RecordFailure()
	if err := cb.Allow(); err == nil {
		t.Error("circuit should be open after 3 failures")
	}
}

func TestCircuitBreaker_SuccessResets(t *testing.T) {
	cb := NewCircuitBreaker(3, 1*time.Second)
	cb.RecordFailure()
	cb.RecordFailure()
	cb.RecordSuccess()
	// Counter should be reset
	cb.RecordFailure()
	cb.RecordFailure()
	if err := cb.Allow(); err != nil {
		t.Error("circuit should still be closed after success reset")
	}
}

func TestCircuitBreaker_HalfOpenAfterTimeout(t *testing.T) {
	cb := NewCircuitBreaker(1, 50*time.Millisecond)
	cb.RecordFailure() // trips open
	if err := cb.Allow(); err == nil {
		t.Error("should be open")
	}
	time.Sleep(60 * time.Millisecond)
	// Should transition to half-open
	if err := cb.Allow(); err != nil {
		t.Errorf("should allow probe: %v", err)
	}
	// Second request in half-open should be rejected
	if err := cb.Allow(); err == nil {
		t.Error("should reject during half-open")
	}
}

func TestCircuitBreaker_ReopensOnProbeFailure(t *testing.T) {
	cb := NewCircuitBreaker(1, 50*time.Millisecond)
	cb.RecordFailure() // trips open
	time.Sleep(60 * time.Millisecond)
	cb.Allow()         // half-open probe allowed
	cb.RecordFailure() // probe failed — should go back to open
	// Immediately after probe failure, circuit should be open again
	if err := cb.Allow(); err == nil {
		t.Error("should be open after probe failure")
	}
	// After another timeout, should allow another probe
	time.Sleep(60 * time.Millisecond)
	if err := cb.Allow(); err != nil {
		t.Errorf("should allow probe after second timeout: %v", err)
	}
}

func TestCircuitBreaker_ClosesOnProbeSuccess(t *testing.T) {
	cb := NewCircuitBreaker(1, 50*time.Millisecond)
	cb.RecordFailure()
	time.Sleep(60 * time.Millisecond)
	cb.Allow()          // half-open probe
	cb.RecordSuccess()  // should close
	if err := cb.Allow(); err != nil {
		t.Errorf("should be closed after probe success: %v", err)
	}
}

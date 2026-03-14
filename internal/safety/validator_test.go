package safety

import (
	"testing"
	"time"
)

func TestValidateQuery_EmptyQuery(t *testing.T) {
	v := NewValidator(24*time.Hour, 500)
	if err := v.ValidateQuery("", "", ""); err == nil {
		t.Error("expected error for empty query")
	}
}

func TestValidateQuery_NoLabelMatcher(t *testing.T) {
	v := NewValidator(24*time.Hour, 500)
	if err := v.ValidateQuery(`{} |= "error"`, "", ""); err == nil {
		t.Error("expected error for missing label matcher")
	}
}

func TestValidateQuery_EmptySelector(t *testing.T) {
	v := NewValidator(24*time.Hour, 500)
	if err := v.ValidateQuery(`{} |= "error"`, "", ""); err == nil {
		t.Error("expected error for empty selector")
	}
}

func TestValidateQuery_ValidQuery(t *testing.T) {
	v := NewValidator(24*time.Hour, 500)
	if err := v.ValidateQuery(`{service="payments"} |= "error"`, "1h ago", "now"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateQuery_LargeTimeRange_NoError(t *testing.T) {
	// Time range clamping is handled by tool handlers, not the validator.
	// The validator only checks format and ordering.
	v := NewValidator(24*time.Hour, 500)
	err := v.ValidateQuery(`{service="payments"}`, "48h ago", "now")
	if err != nil {
		t.Errorf("validator should not reject large time ranges (clamping is in tools): %v", err)
	}
}

func TestMaxTimeRange(t *testing.T) {
	v := NewValidator(24*time.Hour, 500)
	if v.MaxTimeRange() != 24*time.Hour {
		t.Errorf("expected 24h, got %v", v.MaxTimeRange())
	}
}

func TestValidateQuery_DangerousRegex(t *testing.T) {
	v := NewValidator(24*time.Hour, 500)
	err := v.ValidateQuery(`{service="payments"} |~ ".*"`, "1h ago", "now")
	if err == nil {
		t.Error("expected error for dangerous regex")
	}
}

func TestValidateQuery_StartAfterEnd(t *testing.T) {
	v := NewValidator(24*time.Hour, 500)
	err := v.ValidateQuery(`{service="payments"}`, "2024-01-01T00:00:00Z", "2023-01-01T00:00:00Z")
	if err == nil {
		t.Error("expected error when start is after end")
	}
}

func TestMaxResults(t *testing.T) {
	v := NewValidator(24*time.Hour, 500)
	if v.MaxResults() != 500 {
		t.Errorf("expected 500, got %d", v.MaxResults())
	}
}

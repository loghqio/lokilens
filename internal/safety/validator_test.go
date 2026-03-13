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

func TestValidateQuery_TimeRangeExceeded(t *testing.T) {
	v := NewValidator(24*time.Hour, 500)
	err := v.ValidateQuery(`{service="payments"}`, "48h ago", "now")
	if err == nil {
		t.Error("expected error for exceeded time range")
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

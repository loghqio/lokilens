package safety

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	lokipkg "github.com/lokilens/lokilens/internal/loki"
)

// Validator enforces safety constraints on Loki queries.
type Validator struct {
	maxTimeRange time.Duration
	maxResults   int
}

// NewValidator creates a new query validator.
func NewValidator(maxTimeRange time.Duration, maxResults int) *Validator {
	return &Validator{
		maxTimeRange: maxTimeRange,
		maxResults:   maxResults,
	}
}

var (
	labelMatcherPattern = regexp.MustCompile(`\{[^}]*\w+\s*[=!~]+\s*"[^"]*"`)
	dangerousRegex      = regexp.MustCompile(`\|~\s*"\.[\*\+]"`)
)

// ValidateQuery checks that a LogQL query is safe to execute.
func (v *Validator) ValidateQuery(logql, startTime, endTime string) error {
	if strings.TrimSpace(logql) == "" {
		return fmt.Errorf("query cannot be empty")
	}

	// Require at least one label matcher
	if !labelMatcherPattern.MatchString(logql) {
		return fmt.Errorf("query must include at least one label matcher (e.g., {service=\"myapp\"})")
	}

	// Reject empty stream selector
	if strings.Contains(logql, "{}") {
		return fmt.Errorf("empty stream selector {} is not allowed — specify at least one label")
	}

	// Validate time format (time range clamping is handled by tool handlers)
	if startTime != "" {
		start, err := lokipkg.ParseRelativeTime(startTime)
		if err != nil {
			return fmt.Errorf("invalid start time: %w", err)
		}
		end := time.Now()
		if endTime != "" {
			end, err = lokipkg.ParseRelativeTime(endTime)
			if err != nil {
				return fmt.Errorf("invalid end time: %w", err)
			}
		}
		if end.Sub(start) < 0 {
			return fmt.Errorf("start time must be before end time")
		}
	}

	// Reject dangerous regex patterns
	if dangerousRegex.MatchString(logql) {
		return fmt.Errorf("query contains potentially expensive regex pattern — be more specific")
	}

	return nil
}

// MaxResults returns the configured maximum result limit.
func (v *Validator) MaxResults() int {
	return v.maxResults
}

// MaxTimeRange returns the configured maximum query time range.
func (v *Validator) MaxTimeRange() time.Duration {
	return v.maxTimeRange
}

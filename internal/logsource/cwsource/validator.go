package cwsource

import (
	"fmt"
	"regexp"
	"strings"
)

// dangerousPatterns catches potentially expensive or malicious CloudWatch Insights patterns.
var dangerousPatterns = regexp.MustCompile(
	`(?i)` +
		`parse\s+\S+\s+/\.\*/` + // unbounded parse regex (parse @field /.*/)
		`|parse\s+/\.\*/` + // unbounded parse regex (parse /.*/)
		`|like\s+/\.\*/` + // match-everything regex
		`|like\s+/\.\+/`, // match-everything regex
)

// validateInsightsQuery checks that a CloudWatch Insights query is safe to execute.
func validateInsightsQuery(query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("query cannot be empty")
	}

	// Reject dangerous regex patterns that could cause expensive scans
	if dangerousPatterns.MatchString(query) {
		return fmt.Errorf("query contains potentially expensive regex pattern — be more specific")
	}

	// Reject queries that attempt to use commands CloudWatch Insights doesn't support
	// (potential confusion/injection from LLM)
	lower := strings.ToLower(query)
	for _, blocked := range []string{"delete", "drop", "insert", "update", "create", "alter"} {
		if strings.Contains(lower, blocked+" ") || strings.HasPrefix(lower, blocked) {
			return fmt.Errorf("query contains blocked keyword %q", blocked)
		}
	}

	return nil
}

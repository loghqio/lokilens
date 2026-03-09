package safety

import (
	"regexp"
)

// PIIFilter detects and redacts personally identifiable information.
type PIIFilter struct {
	patterns []piiPattern
}

type piiPattern struct {
	name    string
	re      *regexp.Regexp
	replace string
}

// NewPIIFilter creates a PII filter with default patterns.
func NewPIIFilter() *PIIFilter {
	return &PIIFilter{
		patterns: []piiPattern{
			{
				name:    "email",
				re:      regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
				replace: "[REDACTED_EMAIL]",
			},
			{
				name:    "ssn",
				re:      regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
				replace: "[REDACTED_SSN]",
			},
			{
				name:    "credit_card",
				re:      regexp.MustCompile(`\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}\b`),
				replace: "[REDACTED_CC]",
			},
			{
				name:    "ipv4",
				re:      regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
				replace: "[REDACTED_IP]",
			},
			{
				name:    "jwt",
				re:      regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
				replace: "[REDACTED_JWT]",
			},
			{
				name:    "bearer_token",
				re:      regexp.MustCompile(`Bearer\s+[A-Za-z0-9._~+/=\-]+`),
				replace: "Bearer [REDACTED]",
			},
		},
	}
}

// Redact replaces PII patterns in text with redaction markers.
func (f *PIIFilter) Redact(text string) string {
	for _, p := range f.patterns {
		text = p.re.ReplaceAllString(text, p.replace)
	}
	return text
}

// RedactWithCount replaces PII patterns and returns the count of distinct
// pattern types that matched. Useful for audit logging without leaking content.
func (f *PIIFilter) RedactWithCount(text string) (string, int) {
	count := 0
	for _, p := range f.patterns {
		if p.re.MatchString(text) {
			text = p.re.ReplaceAllString(text, p.replace)
			count++
		}
	}
	return text, count
}

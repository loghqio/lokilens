package safety

import (
	"regexp"
	"strings"
)

// PromptGuard detects and blocks prompt injection attempts in user input.
type PromptGuard struct {
	patterns []*regexp.Regexp
}

// NewPromptGuard creates a prompt guard with default patterns.
func NewPromptGuard() *PromptGuard {
	rawPatterns := []string{
		// Instruction override attempts
		`(?i)ignore\s+(all\s+)?(previous|prior|above|your)\s+(instructions?|rules?|prompts?|guidelines?)`,
		`(?i)forget\s+(all\s+)?(your|previous|prior)\s+(instructions?|rules?|prompts?|guidelines?)`,
		`(?i)disregard\s+(all\s+)?(previous|prior|above|your)\s+(instructions?|rules?|prompts?)`,
		`(?i)override\s+(your|previous|system|all)\s+`,

		// System prompt extraction
		`(?i)(reveal|show|output|print|display|dump|repeat|echo)\s+(your|the|system)\s+(prompt|instructions?|rules?|guidelines?)`,
		`(?i)what\s+(are|is)\s+your\s+(system\s+)?(prompt|instructions?|rules?)`,

		// Role-play / persona hijacking
		`(?i)you\s+are\s+now\s+(a|an)\s+`,
		`(?i)pretend\s+(you\s+are|to\s+be)\s+`,
		`(?i)from\s+now\s+on[,.]?\s+(you|act|pretend|behave|respond|answer)`,
		`(?i)new\s+(role|persona|instructions?)\s*:`,

		// Known delimiter injection tokens
		`(?i)<\|?(system|endoftext|im_start|im_end)\|?>`,
		`(?i)\[INST\]|\[/INST\]`,
		`(?i)###\s*(system|instruction|new\s+role)`,
	}

	compiled := make([]*regexp.Regexp, 0, len(rawPatterns))
	for _, p := range rawPatterns {
		compiled = append(compiled, regexp.MustCompile(p))
	}

	return &PromptGuard{patterns: compiled}
}

// Check returns a user-friendly error if the input contains prompt injection patterns.
// Returns nil if the input is safe.
func (g *PromptGuard) Check(input string) error {
	normalized := strings.TrimSpace(input)
	if normalized == "" {
		return nil
	}
	for _, p := range g.patterns {
		if p.MatchString(normalized) {
			return &PromptInjectionError{}
		}
	}
	return nil
}

// PromptInjectionError indicates a blocked prompt injection attempt.
type PromptInjectionError struct{}

func (e *PromptInjectionError) Error() string {
	return "I can only help with log analysis queries. Please ask me about logs, errors, services, or metrics."
}

package errs

import (
	"fmt"
	"regexp"
)

// ValidationError indicates invalid user input (bad query, bad time range, etc.).
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// NewValidation creates a validation error.
func NewValidation(msg string, args ...any) *ValidationError {
	return &ValidationError{Message: fmt.Sprintf(msg, args...)}
}

// LokiError indicates a failure communicating with Loki.
type LokiError struct {
	StatusCode int
	Message    string
	Err        error
}

func (e *LokiError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("loki error (HTTP %d): %s: %v", e.StatusCode, e.Message, e.Err)
	}
	return fmt.Sprintf("loki error (HTTP %d): %s", e.StatusCode, e.Message)
}

func (e *LokiError) Unwrap() error { return e.Err }

// NewLoki creates a Loki error.
func NewLoki(statusCode int, msg string, err error) *LokiError {
	return &LokiError{StatusCode: statusCode, Message: msg, Err: err}
}

// LLMError indicates a failure from the Gemini LLM or ADK runner.
type LLMError struct {
	Message string
	Err     error
}

func (e *LLMError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("llm error: %s: %v", e.Message, e.Err)
	}
	return fmt.Sprintf("llm error: %s", e.Message)
}

func (e *LLMError) Unwrap() error { return e.Err }

// NewLLM creates an LLM error.
func NewLLM(msg string, err error) *LLMError {
	return &LLMError{Message: msg, Err: err}
}

// TimeoutError indicates the operation exceeded its deadline.
type TimeoutError struct {
	Operation string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("timeout: %s", e.Operation)
}

// NewTimeout creates a timeout error.
func NewTimeout(operation string) *TimeoutError {
	return &TimeoutError{Operation: operation}
}

// CircuitOpenError indicates the circuit breaker is open.
type CircuitOpenError struct{}

func (e *CircuitOpenError) Error() string {
	return "LokiLens is temporarily unavailable — the AI backend has been failing. Please try again in a minute or two."
}

// UserMessage returns the error message with sensitive info redacted.
func UserMessage(err error) string {
	return redactSecrets(err.Error())
}

// redactSecrets removes API keys, tokens, and project paths from error strings.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),                                // Gemini API keys
	regexp.MustCompile(`xoxb-[0-9A-Za-z-]+`),                                   // Slack bot tokens
	regexp.MustCompile(`xapp-[0-9A-Za-z-]+`),                                   // Slack app tokens
	regexp.MustCompile(`projects/[^/]+/locations/[^/]+`),                        // GCP project/location paths
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)\s*[:=]\s*\S+`),  // generic key=value secrets
}

func redactSecrets(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

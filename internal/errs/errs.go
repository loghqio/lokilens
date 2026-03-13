package errs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	// Match "429" only when preceded by HTTP-status indicators (case-insensitive).
	// Catches: "HTTP 429", "status 429", "code 429", "code: 429", "returned 429"
	rateLimitPattern = regexp.MustCompile(`(?i)(HTTP|status|code|returned)\s*:?\s*429\b`)

	// Match "401"/"403" only when preceded by HTTP-status indicators.
	authErrorPattern = regexp.MustCompile(`(?i)(HTTP|status|code|returned)\s*:?\s*(401|403)\b`)
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

// UserMessage returns the appropriate user-facing message for an error.
// Uses errors.As for proper unwrapping of wrapped errors.
func UserMessage(err error) string {
	errStr := err.Error()

	// Check typed errors first via errors.As to avoid misattributing Loki
	// errors as Gemini errors (e.g. a Loki 403 should not produce a
	// "Gemini API authentication failed" message).
	var validationErr *ValidationError
	if errors.As(err, &validationErr) {
		return validationErr.Error()
	}

	var lokiErr *LokiError
	if errors.As(err, &lokiErr) {
		// StatusCode 0 means no HTTP response was received — this is a network
		// error (connection refused, TLS failure, DNS timeout), not a Loki query
		// problem. Check for reachability before falling through to generic advice.
		if lokiErr.StatusCode == 0 && isLokiUnreachable(lokiErr.Error()) {
			return "Cannot reach Loki. The log backend is unreachable — please check your LOKI_BASE_URL configuration and network connectivity."
		}
		if lokiErr.StatusCode == 400 {
			return "Query syntax error — I'll try to fix the query and retry. If this persists, try rephrasing your question."
		}
		if lokiErr.StatusCode == 401 || lokiErr.StatusCode == 403 {
			return "Loki authentication failed. Please check your LOKI_API_KEY configuration."
		}
		if lokiErr.StatusCode == 429 {
			return "Loki rate limit hit — too many queries in a short time. Please wait a moment and try again."
		}
		if lokiErr.StatusCode >= 500 {
			return "Loki is having issues right now. Please try again in a moment."
		}
		return "Problem querying logs. Please try a simpler query or shorter time range."
	}

	var llmErr *LLMError
	if errors.As(err, &llmErr) {
		// Check the INNER error string for specific Gemini API failures before
		// falling through to the generic LLM message. Without this, Gemini 429s,
		// quota exhaustion, and auth errors all produce "try rephrasing" — useless
		// when the real problem is an API limit.
		innerStr := llmErr.Error()
		if isQuotaExhausted(innerStr) {
			return "Gemini API quota exhausted. The daily request limit has been reached — please try again tomorrow or configure a paid API key."
		}
		if isRateLimited(innerStr) {
			return "Gemini API rate limit hit. Too many requests per minute — please wait a moment and try again."
		}
		if isAuthError(innerStr) {
			return "Gemini API authentication failed. The API key may be invalid or expired — please check your GEMINI_API_KEY configuration."
		}
		if isLokiUnreachable(innerStr) {
			return "Cannot reach Loki. The log backend is unreachable — please check your LOKI_BASE_URL configuration and network connectivity."
		}
		return "I had trouble processing your request. Please try rephrasing or simplifying your question."
	}

	var timeoutErr *TimeoutError
	if errors.As(err, &timeoutErr) {
		return "That query took too long. Try narrowing to a specific service, shorter time range, or simpler filter."
	}

	var circuitErr *CircuitOpenError
	if errors.As(err, &circuitErr) {
		return circuitErr.Error()
	}

	// String-based checks for untyped errors (e.g. errors from Gemini SDK
	// that don't use our error types)
	if isLokiUnreachable(errStr) {
		return "Cannot reach Loki. The log backend is unreachable — please check your LOKI_BASE_URL configuration and network connectivity."
	}
	if isQuotaExhausted(errStr) {
		return "Gemini API quota exhausted. The daily request limit has been reached — please try again tomorrow or configure a paid API key."
	}
	if isRateLimited(errStr) {
		return "Gemini API rate limit hit. Too many requests per minute — please wait a moment and try again."
	}
	if isAuthError(errStr) {
		return "Gemini API authentication failed. The API key may be invalid or expired — please check your GEMINI_API_KEY configuration."
	}

	return "Something unexpected happened. Please try again — if the issue persists, try a different question."
}

func isQuotaExhausted(s string) bool {
	return strings.Contains(s, "RESOURCE_EXHAUSTED") ||
		(strings.Contains(s, "quota") && strings.Contains(s, "exceeded"))
}

func isRateLimited(s string) bool {
	if isQuotaExhausted(s) {
		return false
	}
	// Match "429" only in HTTP-context patterns to avoid false positives
	// on strings like "processed 429 records" or port numbers.
	return rateLimitPattern.MatchString(s)
}

func isAuthError(s string) bool {
	// Named error codes are unambiguous
	if strings.Contains(s, "PERMISSION_DENIED") || strings.Contains(s, "API_KEY_INVALID") {
		return true
	}
	// Match "401"/"403" only in HTTP-context patterns to avoid false positives
	// on strings like "processed 401 records" or numeric IDs.
	return authErrorPattern.MatchString(s)
}

func isLokiUnreachable(s string) bool {
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "dial tcp") ||
		strings.Contains(s, "x509: certificate") ||
		strings.Contains(s, "tls: handshake failure") ||
		strings.Contains(s, "i/o timeout")
}

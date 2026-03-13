package errs

import (
	"fmt"
	"testing"
)

func TestUserMessage_ValidationError(t *testing.T) {
	err := NewValidation("bad query: %s", "empty selector")
	msg := UserMessage(err)
	if msg != "bad query: empty selector" {
		t.Errorf("expected validation message, got: %s", msg)
	}
}

func TestUserMessage_WrappedValidationError(t *testing.T) {
	err := NewValidation("bad query")
	wrapped := fmt.Errorf("agent failed: %w", err)
	msg := UserMessage(wrapped)
	if msg != "bad query" {
		t.Errorf("expected unwrapped validation message, got: %s", msg)
	}
}

func TestUserMessage_LokiError_500(t *testing.T) {
	err := NewLoki(500, "internal error", nil)
	msg := UserMessage(err)
	if msg != "Loki is having issues right now. Please try again in a moment." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_LokiError_400(t *testing.T) {
	err := NewLoki(400, "bad request", nil)
	msg := UserMessage(err)
	if msg != "Query syntax error — I'll try to fix the query and retry. If this persists, try rephrasing your question." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_LokiError_429(t *testing.T) {
	err := NewLoki(429, "rate limited", nil)
	msg := UserMessage(err)
	if msg != "Loki rate limit hit — too many queries in a short time. Please wait a moment and try again." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_LokiError_Other(t *testing.T) {
	err := NewLoki(404, "not found", nil)
	msg := UserMessage(err)
	if msg != "Problem querying logs. Please try a simpler query or shorter time range." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_LokiError_401(t *testing.T) {
	err := NewLoki(401, "unauthorized", nil)
	msg := UserMessage(err)
	if msg != "Loki authentication failed. Please check your LOKI_API_KEY configuration." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_LokiError_403(t *testing.T) {
	err := NewLoki(403, "forbidden", nil)
	msg := UserMessage(err)
	expected := "Loki authentication failed. Please check your LOKI_API_KEY configuration."
	if msg != expected {
		t.Errorf("expected %q, got %q", expected, msg)
	}
}

func TestUserMessage_WrappedLoki403_NotMisattributedToGemini(t *testing.T) {
	// A wrapped Loki 403 should NOT produce a "Gemini API authentication failed" message
	err := NewLoki(403, "forbidden", nil)
	wrapped := fmt.Errorf("query failed: %w", err)
	msg := UserMessage(wrapped)
	expected := "Loki authentication failed. Please check your LOKI_API_KEY configuration."
	if msg != expected {
		t.Errorf("Loki 403 was misattributed: got %q, want %q", msg, expected)
	}
}

func TestUserMessage_WrappedLokiError(t *testing.T) {
	err := NewLoki(500, "internal error", nil)
	wrapped := fmt.Errorf("query failed: %w", err)
	msg := UserMessage(wrapped)
	if msg != "Loki is having issues right now. Please try again in a moment." {
		t.Errorf("expected loki message for wrapped error, got: %s", msg)
	}
}

func TestUserMessage_TimeoutError(t *testing.T) {
	err := NewTimeout("loki query")
	msg := UserMessage(err)
	if msg != "That query took too long. Try narrowing to a specific service, shorter time range, or simpler filter." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_CircuitOpenError(t *testing.T) {
	err := &CircuitOpenError{}
	msg := UserMessage(err)
	if msg != "LokiLens is temporarily unavailable — the AI backend has been failing. Please try again in a minute or two." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_QuotaExhausted(t *testing.T) {
	err := fmt.Errorf("RESOURCE_EXHAUSTED: something")
	msg := UserMessage(err)
	if msg != "Gemini API quota exhausted. The daily request limit has been reached — please try again tomorrow or configure a paid API key." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_QuotaExhaustedAlt(t *testing.T) {
	err := fmt.Errorf("quota exceeded for project")
	msg := UserMessage(err)
	if msg != "Gemini API quota exhausted. The daily request limit has been reached — please try again tomorrow or configure a paid API key." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_RateLimited(t *testing.T) {
	err := fmt.Errorf("HTTP 429 Too Many Requests")
	msg := UserMessage(err)
	if msg != "Gemini API rate limit hit. Too many requests per minute — please wait a moment and try again." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_RateLimited_StatusCode(t *testing.T) {
	err := fmt.Errorf("returned 429: rate limit exceeded")
	msg := UserMessage(err)
	if msg != "Gemini API rate limit hit. Too many requests per minute — please wait a moment and try again." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_RateLimited_NoFalsePositive(t *testing.T) {
	// "429" as part of a count or ID should NOT trigger rate limit message
	err := fmt.Errorf("processed 429 records successfully")
	msg := UserMessage(err)
	if msg == "Gemini API rate limit hit. Too many requests per minute — please wait a moment and try again." {
		t.Error("bare '429' in non-HTTP context should not be classified as rate limit")
	}
}

func TestUserMessage_AuthError(t *testing.T) {
	err := fmt.Errorf("API_KEY_INVALID")
	msg := UserMessage(err)
	if msg != "Gemini API authentication failed. The API key may be invalid or expired — please check your GEMINI_API_KEY configuration." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_AuthError_HTTP401(t *testing.T) {
	err := fmt.Errorf("HTTP 401 Unauthorized")
	msg := UserMessage(err)
	if msg != "Gemini API authentication failed. The API key may be invalid or expired — please check your GEMINI_API_KEY configuration." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_AuthError_NoFalsePositive(t *testing.T) {
	// "401" as part of a count should NOT trigger auth error message
	err := fmt.Errorf("matched 401 log entries in the last hour")
	msg := UserMessage(err)
	if msg == "Gemini API authentication failed. The API key may be invalid or expired — please check your GEMINI_API_KEY configuration." {
		t.Error("bare '401' in non-HTTP context should not be classified as auth error")
	}
}

func TestUserMessage_LokiUnreachable(t *testing.T) {
	err := fmt.Errorf("dial tcp connection refused")
	msg := UserMessage(err)
	if msg != "Cannot reach Loki. The log backend is unreachable — please check your LOKI_BASE_URL configuration and network connectivity." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_UnknownError(t *testing.T) {
	err := fmt.Errorf("something unexpected")
	msg := UserMessage(err)
	if msg != "Something unexpected happened. Please try again — if the issue persists, try a different question." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestUserMessage_LLMError(t *testing.T) {
	err := NewLLM("model failed", nil)
	msg := UserMessage(err)
	if msg != "I had trouble processing your request. Please try rephrasing or simplifying your question." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestLokiError_Unwrap(t *testing.T) {
	inner := fmt.Errorf("connection refused")
	err := NewLoki(500, "failed", inner)
	if err.Unwrap() != inner {
		t.Error("Unwrap should return the inner error")
	}
}

func TestLLMError_Unwrap(t *testing.T) {
	inner := fmt.Errorf("model unavailable")
	err := NewLLM("failed", inner)
	if err.Unwrap() != inner {
		t.Error("Unwrap should return the inner error")
	}
}

func TestUserMessage_LLMError_WrappingQuotaExhausted(t *testing.T) {
	// Gemini SDK returns a raw error with RESOURCE_EXHAUSTED, and agent.Run wraps
	// it in LLMError. UserMessage must detect this instead of returning the generic
	// "try rephrasing" message.
	inner := fmt.Errorf("rpc error: code = ResourceExhausted desc = RESOURCE_EXHAUSTED: quota exceeded")
	err := NewLLM("agent execution failed", inner)
	msg := UserMessage(err)
	expected := "Gemini API quota exhausted. The daily request limit has been reached — please try again tomorrow or configure a paid API key."
	if msg != expected {
		t.Errorf("Gemini quota error swallowed by LLMError wrapper:\n  got:  %s\n  want: %s", msg, expected)
	}
}

func TestUserMessage_LLMError_WrappingRateLimit(t *testing.T) {
	inner := fmt.Errorf("HTTP 429: rate limit exceeded for model")
	err := NewLLM("agent execution failed", inner)
	msg := UserMessage(err)
	expected := "Gemini API rate limit hit. Too many requests per minute — please wait a moment and try again."
	if msg != expected {
		t.Errorf("Gemini 429 swallowed by LLMError wrapper:\n  got:  %s\n  want: %s", msg, expected)
	}
}

func TestUserMessage_LLMError_WrappingAuthError(t *testing.T) {
	inner := fmt.Errorf("PERMISSION_DENIED: API key not valid")
	err := NewLLM("agent execution failed", inner)
	msg := UserMessage(err)
	expected := "Gemini API authentication failed. The API key may be invalid or expired — please check your GEMINI_API_KEY configuration."
	if msg != expected {
		t.Errorf("Gemini auth error swallowed by LLMError wrapper:\n  got:  %s\n  want: %s", msg, expected)
	}
}

func TestUserMessage_LLMError_WrappingLokiUnreachable(t *testing.T) {
	// An agent tool call fails with "connection refused" which surfaces as LLMError
	inner := fmt.Errorf("tool query_logs failed: dial tcp 10.0.0.1:3100: connection refused")
	err := NewLLM("agent execution failed", inner)
	msg := UserMessage(err)
	expected := "Cannot reach Loki. The log backend is unreachable — please check your LOKI_BASE_URL configuration and network connectivity."
	if msg != expected {
		t.Errorf("Loki unreachable swallowed by LLMError wrapper:\n  got:  %s\n  want: %s", msg, expected)
	}
}

func TestUserMessage_LLMError_GenericInner(t *testing.T) {
	// A truly generic LLM error should still get the default message
	inner := fmt.Errorf("model returned empty response")
	err := NewLLM("agent execution failed", inner)
	msg := UserMessage(err)
	expected := "I had trouble processing your request. Please try rephrasing or simplifying your question."
	if msg != expected {
		t.Errorf("generic LLM error should get default message:\n  got:  %s\n  want: %s", msg, expected)
	}
}

func TestUserMessage_LokiError_NetworkUnreachable(t *testing.T) {
	// Network errors produce LokiError with StatusCode 0 — must show
	// "Cannot reach Loki" instead of "Problem querying logs".
	inner := fmt.Errorf("dial tcp 10.0.0.1:3100: connection refused")
	err := NewLoki(0, "request failed after 3 attempts", inner)
	msg := UserMessage(err)
	expected := "Cannot reach Loki. The log backend is unreachable — please check your LOKI_BASE_URL configuration and network connectivity."
	if msg != expected {
		t.Errorf("Network error misclassified as query problem:\n  got:  %s\n  want: %s", msg, expected)
	}
}

func TestUserMessage_LokiError_TLSFailure(t *testing.T) {
	inner := fmt.Errorf("x509: certificate signed by unknown authority")
	err := NewLoki(0, "request failed after 3 attempts", inner)
	msg := UserMessage(err)
	expected := "Cannot reach Loki. The log backend is unreachable — please check your LOKI_BASE_URL configuration and network connectivity."
	if msg != expected {
		t.Errorf("TLS error not caught:\n  got:  %s\n  want: %s", msg, expected)
	}
}

func TestUserMessage_LokiError_IOTimeout(t *testing.T) {
	inner := fmt.Errorf("dial tcp 10.0.0.1:3100: i/o timeout")
	err := NewLoki(0, "request failed after 3 attempts", inner)
	msg := UserMessage(err)
	expected := "Cannot reach Loki. The log backend is unreachable — please check your LOKI_BASE_URL configuration and network connectivity."
	if msg != expected {
		t.Errorf("i/o timeout not caught:\n  got:  %s\n  want: %s", msg, expected)
	}
}

func TestIsLokiUnreachable_TLSHandshake(t *testing.T) {
	if !isLokiUnreachable("tls: handshake failure") {
		t.Error("tls: handshake failure should be detected as unreachable")
	}
}

func TestIsLokiUnreachable_X509(t *testing.T) {
	if !isLokiUnreachable("x509: certificate has expired") {
		t.Error("x509 certificate error should be detected as unreachable")
	}
}

func TestIsLokiUnreachable_IOTimeout(t *testing.T) {
	if !isLokiUnreachable("dial tcp 10.0.0.1:3100: i/o timeout") {
		t.Error("i/o timeout should be detected as unreachable")
	}
}

package errs

import (
	"fmt"
	"strings"
	"testing"
)

func TestUserMessage_ReturnsErrorString(t *testing.T) {
	err := fmt.Errorf("something broke")
	msg := UserMessage(err)
	if msg != "something broke" {
		t.Errorf("expected error string, got: %s", msg)
	}
}

func TestUserMessage_ValidationError(t *testing.T) {
	err := NewValidation("bad query: %s", "empty selector")
	msg := UserMessage(err)
	if msg != "bad query: empty selector" {
		t.Errorf("expected validation message, got: %s", msg)
	}
}

func TestUserMessage_LokiError(t *testing.T) {
	err := NewLoki(500, "internal error", nil)
	msg := UserMessage(err)
	if !strings.Contains(msg, "loki error") || !strings.Contains(msg, "500") {
		t.Errorf("expected loki error details, got: %s", msg)
	}
}

func TestUserMessage_LLMError(t *testing.T) {
	inner := fmt.Errorf("model returned empty response")
	err := NewLLM("agent execution failed", inner)
	msg := UserMessage(err)
	if !strings.Contains(msg, "agent execution failed") || !strings.Contains(msg, "model returned empty response") {
		t.Errorf("expected full LLM error, got: %s", msg)
	}
}

func TestUserMessage_TimeoutError(t *testing.T) {
	err := NewTimeout("loki query")
	msg := UserMessage(err)
	if msg != "timeout: loki query" {
		t.Errorf("expected timeout message, got: %s", msg)
	}
}

func TestUserMessage_CircuitOpenError(t *testing.T) {
	err := &CircuitOpenError{}
	msg := UserMessage(err)
	if !strings.Contains(msg, "temporarily unavailable") {
		t.Errorf("expected circuit breaker message, got: %s", msg)
	}
}

func TestUserMessage_RedactsGeminiAPIKey(t *testing.T) {
	err := fmt.Errorf("failed with key AIzaSyDu-3NI1fbYnauTT_XR29fVqWeWjrQsYQs")
	msg := UserMessage(err)
	if strings.Contains(msg, "AIza") {
		t.Errorf("API key not redacted: %s", msg)
	}
	if !strings.Contains(msg, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder, got: %s", msg)
	}
}

func TestUserMessage_RedactsSlackBotToken(t *testing.T) {
	err := fmt.Errorf("auth failed: xoxb-1234-5678-abcdef")
	msg := UserMessage(err)
	if strings.Contains(msg, "xoxb-") {
		t.Errorf("Slack bot token not redacted: %s", msg)
	}
}

func TestUserMessage_RedactsSlackAppToken(t *testing.T) {
	err := fmt.Errorf("auth failed: xapp-1-A0AHLG5625B-1234")
	msg := UserMessage(err)
	if strings.Contains(msg, "xapp-") {
		t.Errorf("Slack app token not redacted: %s", msg)
	}
}

func TestUserMessage_RedactsGCPProjectPath(t *testing.T) {
	err := fmt.Errorf("Model projects/oink-pong/locations/us-central1/publishers/google/models/gemini-2.5-flash not found")
	msg := UserMessage(err)
	if strings.Contains(msg, "oink-pong") {
		t.Errorf("GCP project not redacted: %s", msg)
	}
}

func TestUserMessage_RedactsGenericSecrets(t *testing.T) {
	err := fmt.Errorf("config: api_key=sk-secret123 failed")
	msg := UserMessage(err)
	if strings.Contains(msg, "sk-secret123") {
		t.Errorf("generic secret not redacted: %s", msg)
	}
}

func TestUserMessage_PreservesNonSensitiveContent(t *testing.T) {
	err := fmt.Errorf("RESOURCE_EXHAUSTED: quota exceeded for model gemini-2.5-flash")
	msg := UserMessage(err)
	if !strings.Contains(msg, "RESOURCE_EXHAUSTED") || !strings.Contains(msg, "quota exceeded") {
		t.Errorf("non-sensitive content was stripped: %s", msg)
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

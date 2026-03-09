package errs

import "fmt"

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
	return "service temporarily unavailable — please try again in a moment"
}

// UserMessage returns the appropriate user-facing message for an error.
func UserMessage(err error) string {
	switch err.(type) {
	case *ValidationError:
		return err.Error()
	case *LokiError:
		return "There was a problem querying the log backend. Please try again."
	case *LLMError:
		return "Something went wrong processing your request. Please try again."
	case *TimeoutError:
		return "Request timed out. Try a narrower query or shorter time range."
	case *CircuitOpenError:
		return err.Error()
	default:
		return "Something went wrong. Please try again."
	}
}

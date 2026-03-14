package logsource

import (
	"context"

	"google.golang.org/adk/tool"
)

// LogSource is the plugin interface for log backends.
// Each implementation provides backend-specific tools and system instruction
// for the LLM. At runtime, exactly one LogSource is loaded per agent.
type LogSource interface {
	// Name returns a human-readable name (e.g. "Loki", "CloudWatch").
	Name() string

	// Tools returns the ADK tools for this backend.
	Tools() ([]tool.Tool, error)

	// Instruction returns the system instruction for this backend.
	Instruction() string

	// Description returns a short description for the ADK agent config.
	Description() string

	// HealthCheck verifies connectivity to the log backend.
	// Returns nil if healthy, or an error describing the problem.
	HealthCheck(ctx context.Context) error
}

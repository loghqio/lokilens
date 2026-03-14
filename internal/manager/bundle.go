package manager

import (
	"context"

	agentpkg "github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/bot"
	"github.com/lokilens/lokilens/internal/logsource"
	"github.com/lokilens/lokilens/internal/safety"
	"github.com/lokilens/lokilens/internal/store"
)

// WorkspaceBundle groups all per-workspace runtime components.
type WorkspaceBundle struct {
	Workspace      *store.Workspace
	Bot            *bot.Bot
	Agent          *agentpkg.Agent
	LogSource      logsource.LogSource
	CircuitBreaker *safety.CircuitBreaker
	Cancel         context.CancelFunc
}

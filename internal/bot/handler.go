package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	agentpkg "github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/errs"
	"github.com/lokilens/lokilens/internal/safety"
)

const (
	agentTimeout       = 2 * time.Minute
	maxTurnsPerThread  = 20
	maxTrackedThreads  = 5000
	maxTrackedSessions = 5000
)

// UsageChecker enforces daily query limits for free-tier workspaces.
type UsageChecker interface {
	Check(ctx context.Context, workspaceID string) (count int, limit int, err error)
}

// Handler bridges Slack events to the ADK agent.
type Handler struct {
	slackAPI       *slack.Client
	agent          *agentpkg.Agent
	rateLimiter    *safety.RateLimiter
	piiFilter      *safety.PIIFilter
	promptGuard    *safety.PromptGuard
	circuitBreaker *safety.CircuitBreaker
	audit          *audit.Logger
	logger         *slog.Logger

	// Multi-tenant fields
	workspaceID     string
	workspaceStatus string       // "active" or "pending_setup"; empty = active
	usageChecker    UsageChecker // nil = no usage tracking

	activeThreadsMu sync.RWMutex
	activeThreads   map[string]time.Time

	turnCountsMu sync.Mutex
	turnCounts   map[string]int
}

// HandlerConfig holds parameters for NewHandler.
type HandlerConfig struct {
	SlackAPI        *slack.Client
	Agent           *agentpkg.Agent
	RateLimiter     *safety.RateLimiter
	PIIFilter       *safety.PIIFilter
	PromptGuard     *safety.PromptGuard
	CircuitBreaker  *safety.CircuitBreaker
	AuditLogger     *audit.Logger
	Logger          *slog.Logger
	WorkspaceID     string
	WorkspaceStatus string
	UsageChecker    UsageChecker
}

// NewHandler creates a new message handler.
func NewHandler(cfg HandlerConfig) *Handler {
	return &Handler{
		slackAPI:        cfg.SlackAPI,
		agent:           cfg.Agent,
		rateLimiter:     cfg.RateLimiter,
		piiFilter:       cfg.PIIFilter,
		promptGuard:     cfg.PromptGuard,
		circuitBreaker:  cfg.CircuitBreaker,
		audit:           cfg.AuditLogger,
		logger:          cfg.Logger,
		workspaceID:     cfg.WorkspaceID,
		workspaceStatus: cfg.WorkspaceStatus,
		usageChecker:    cfg.UsageChecker,
		activeThreads:   make(map[string]time.Time),
		turnCounts:      make(map[string]int),
	}
}

// HandleMessage processes an incoming user message.
// source identifies how the message arrived: "dm", "mention", or "thread_followup".
func (h *Handler) HandleMessage(ctx context.Context, channel, userID, text, threadTS, messageTS, source string) {
	// Determine the thread to reply in
	replyTS := threadTS
	if replyTS == "" {
		replyTS = messageTS
	}

	// Pending-setup guard: if workspace isn't active, prompt for setup
	if h.workspaceStatus == "pending_setup" {
		msg := "LokiLens isn't configured yet for this workspace. An admin can run `/lokilens-setup` to get started."
		h.postMessage(channel, replyTS, msg, FormatError(msg))
		return
	}

	sessionID := h.resolveSessionID(channel, replyTS)

	// Audit: every incoming message
	h.audit.MessageReceived(userID, channel, sessionID, source)

	// Mark this thread as active so follow-up replies are handled without @mention
	h.markThreadActive(channel, replyTS)

	// Rate limit check
	if err := h.rateLimiter.Allow(userID); err != nil {
		h.audit.RateLimitExceeded(userID, channel)
		h.postMessage(channel, replyTS, "Rate limit exceeded. Please wait a moment.", FormatError("You're sending messages too fast. Please wait a moment."))
		return
	}

	// Daily usage limit (free tier with shared Gemini key)
	if h.usageChecker != nil {
		count, limit, err := h.usageChecker.Check(ctx, h.workspaceID)
		if err != nil {
			h.logger.Error("usage check failed, allowing query", "error", err)
		} else if count > limit {
			msg := fmt.Sprintf(
				"Daily query limit reached (%d/%d). Resets at midnight UTC.\nRun `/lokilens-setup` to add your own Gemini API key for unlimited queries.",
				limit, limit,
			)
			h.postMessage(channel, replyTS, msg, FormatError(msg))
			return
		}
	}

	// Prompt injection check
	if err := h.promptGuard.Check(text); err != nil {
		h.audit.PromptInjectionBlocked(userID, channel)
		h.postMessage(channel, replyTS, err.Error(), FormatError(err.Error()))
		return
	}

	// Check conversation length
	turns := h.incrementTurns(sessionID)
	if turns > maxTurnsPerThread {
		h.audit.MaxTurnsExceeded(userID, channel, sessionID, turns)
		msg := "This conversation has grown quite long. For best results, please start a new thread — I'll have fresh context to work with."
		h.postMessage(channel, replyTS, msg, FormatError(msg))
		return
	}

	// Circuit breaker check — fail fast if LLM is unhealthy
	if err := h.circuitBreaker.Allow(); err != nil {
		h.audit.CircuitBreakerTripped(userID, channel)
		errMsg := errs.UserMessage(err)
		h.postMessage(channel, replyTS, errMsg, FormatError(errMsg))
		return
	}

	// Post "thinking" message — will be updated with the actual response
	thinkingTS := h.postThinking(channel, replyTS)

	// Run the agent with a timeout
	h.audit.AgentStarted(userID, channel, sessionID)
	start := time.Now()

	agentCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()

	response, err := h.agent.Run(agentCtx, userID, sessionID, text)
	durationMS := time.Since(start).Milliseconds()

	if err != nil {
		h.circuitBreaker.RecordFailure()
		h.audit.AgentFailed(userID, channel, sessionID, durationMS, err)
		h.logger.Error("agent execution failed", "error", err, "user", userID)
		if agentCtx.Err() == context.DeadlineExceeded {
			err = errs.NewTimeout("agent query")
		}
		errMsg := errs.UserMessage(err)
		h.resolveThinking(channel, replyTS, thinkingTS, errMsg, FormatError(errMsg))
		return
	}

	h.circuitBreaker.RecordSuccess()
	h.audit.AgentCompleted(userID, channel, sessionID, durationMS)

	// PII filter the response (with audit)
	response, piiCount := h.piiFilter.RedactWithCount(response)
	if piiCount > 0 {
		h.audit.PIIRedacted(channel, sessionID, piiCount)
	}

	// Format and deliver — update thinking message or post new
	blocks := FormatResponse(response, userID)
	h.resolveThinking(channel, replyTS, thinkingTS, response, blocks)
}

// postThinking posts the initial "Analyzing..." message and returns its timestamp.
// Returns "" if the post fails (caller should fall back to postMessage).
func (h *Handler) postThinking(channel, threadTS string) string {
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, ":hourglass_flowing_sand: *Analyzing your query...*", false, false),
			nil, nil,
		),
	}
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText("Analyzing your query...", false),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	_, ts, err := h.slackAPI.PostMessage(channel, opts...)
	if err != nil {
		h.logger.Error("failed to post thinking message", "error", err, "channel", channel)
		return ""
	}
	return ts
}

// resolveThinking updates the thinking message with the final response, or posts
// a new message if the thinking message wasn't created.
func (h *Handler) resolveThinking(channel, threadTS, thinkingTS, fallback string, blocks []slack.Block) {
	if thinkingTS != "" {
		h.updateMessage(channel, thinkingTS, fallback, blocks)
	} else {
		h.postMessage(channel, threadTS, fallback, blocks)
	}
}

func (h *Handler) resolveSessionID(channel, threadTS string) string {
	if h.workspaceID != "" {
		return fmt.Sprintf("%s:%s:%s", h.workspaceID, channel, threadTS)
	}
	return fmt.Sprintf("%s:%s", channel, threadTS)
}

func (h *Handler) postMessage(channel, threadTS, fallbackText string, blocks []slack.Block) {
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(fallbackText, false),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	_, _, err := h.slackAPI.PostMessage(channel, opts...)
	if err != nil {
		h.logger.Error("failed to post message", "error", err, "channel", channel)
	}
}

func (h *Handler) updateMessage(channel, timestamp, fallbackText string, blocks []slack.Block) {
	_, _, _, err := h.slackAPI.UpdateMessage(channel, timestamp,
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(fallbackText, false),
	)
	if err != nil {
		h.logger.Error("failed to update message", "error", err, "channel", channel)
	}
}

// IsBotMessage returns true if the message should be ignored (bot messages, subtypes).
func IsBotMessage(botID, subType string) bool {
	return botID != "" || subType != ""
}

// IsDM returns true if the channel is a direct message channel.
func IsDM(channel string) bool {
	return strings.HasPrefix(channel, "D")
}

// markThreadActive records that the bot is participating in a thread.
func (h *Handler) markThreadActive(channel, threadTS string) {
	if threadTS == "" {
		return
	}
	key := channel + ":" + threadTS
	h.activeThreadsMu.Lock()
	h.activeThreads[key] = time.Now()
	if len(h.activeThreads) > maxTrackedThreads {
		cutoff := time.Now().Add(-2 * time.Hour)
		for k, t := range h.activeThreads {
			if t.Before(cutoff) {
				delete(h.activeThreads, k)
			}
		}
	}
	h.activeThreadsMu.Unlock()
}

// IsActiveThread checks if the bot is participating in a thread.
func (h *Handler) IsActiveThread(channel, threadTS string) bool {
	if threadTS == "" {
		return false
	}
	key := channel + ":" + threadTS
	h.activeThreadsMu.RLock()
	defer h.activeThreadsMu.RUnlock()
	_, ok := h.activeThreads[key]
	return ok
}

// incrementTurns tracks conversation length and returns the updated count.
func (h *Handler) incrementTurns(sessionID string) int {
	h.turnCountsMu.Lock()
	defer h.turnCountsMu.Unlock()
	if len(h.turnCounts) > maxTrackedSessions {
		clear(h.turnCounts)
	}
	h.turnCounts[sessionID]++
	return h.turnCounts[sessionID]
}

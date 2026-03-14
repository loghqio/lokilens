package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
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
	maxTurnsPerThread  = 50
	maxTrackedThreads  = 5000
	maxTrackedSessions = 5000
	maxMessageLength   = 4000 // Slack's own message limit
	maxAttachmentSize  = 10 * 1024 * 1024 // 10MB per file
)

// Attachment represents a file uploaded with a Slack message.
type Attachment struct {
	MimeType string
	Data     []byte
	Name     string
}

// UsageChecker enforces daily query limits for free-tier workspaces.
type UsageChecker interface {
	Check(ctx context.Context, workspaceID string) (count int, limit int, err error)
}

// Handler bridges Slack events to the ADK agent.
type Handler struct {
	slackAPI       *slack.Client
	agent          *agentpkg.Agent
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
	turnCounts   map[string]turnEntry
}

// turnEntry tracks conversation turns with a timestamp for LRU eviction.
type turnEntry struct {
	count    int
	lastSeen time.Time
}

// HandlerConfig holds parameters for NewHandler.
type HandlerConfig struct {
	SlackAPI        *slack.Client
	Agent           *agentpkg.Agent
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
		piiFilter:       cfg.PIIFilter,
		promptGuard:     cfg.PromptGuard,
		circuitBreaker:  cfg.CircuitBreaker,
		audit:           cfg.AuditLogger,
		logger:          cfg.Logger,
		workspaceID:     cfg.WorkspaceID,
		workspaceStatus: cfg.WorkspaceStatus,
		usageChecker:    cfg.UsageChecker,
		activeThreads:   make(map[string]time.Time),
		turnCounts:      make(map[string]turnEntry),
	}
}

// HandleMessage processes an incoming user message.
// source identifies how the message arrived: "dm", "mention", or "thread_followup".
func (h *Handler) HandleMessage(ctx context.Context, channel, userID, text, threadTS, messageTS, source string, attachments []Attachment) {
	// Determine the thread to reply in
	replyTS := threadTS
	if replyTS == "" {
		replyTS = messageTS
	}

	// Input length check — reject oversized messages to control LLM token costs
	if len(text) > maxMessageLength {
		msg := fmt.Sprintf("Message too long (%d chars). Please keep it under %d characters.", len(text), maxMessageLength)
		h.postMessage(channel, replyTS, msg, FormatError(msg))
		return
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

	// Quick replies — respond instantly without calling the LLM.
	// These cost zero API, zero Loki, zero conversation turns.
	// Also catches empty mentions (@LokiLens with no text) and "help" —
	// both must work even when the AI backend is down.
	// Skip quick replies when files are attached — the user wants
	// the LLM to process the file, even if the text is just "hello".
	//
	// Use threadTS != "" (not source == "thread_followup") to detect threads.
	// DM thread replies arrive as source="dm" and @mention thread replies as
	// source="mention" — both need inThread=true so that "yeah" in response
	// to "Want me to dig deeper?" reaches the LLM instead of being
	// short-circuited as gratitude.
	if reply, ok := quickReplyFor(text, threadTS != ""); ok && len(attachments) == 0 {
		h.postMessage(channel, replyTS, reply, FormatQuickReply(reply))
		return
	}

	// Daily usage limit (free tier with shared Gemini key)
	if h.usageChecker != nil {
		count, limit, err := h.usageChecker.Check(ctx, h.workspaceID)
		if err != nil {
			h.logger.Error("usage check failed, allowing query", "error", err)
		} else if count >= limit {
			msg := fmt.Sprintf(
				"Daily query limit reached (%d/%d). Resets at midnight UTC.\nRun `/lokilens-setup` to add your own Gemini API key for unlimited queries.",
				count, limit,
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

	// Post "thinking" message — will be updated with the actual response.
	thinkingTS := h.postThinking(channel, replyTS, text)

	// Run the agent with a timeout
	h.audit.AgentStarted(userID, channel, sessionID)
	start := time.Now()

	agentCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()

	// Convert attachments to agent format
	var agentFiles []agentpkg.FileInput
	for _, att := range attachments {
		agentFiles = append(agentFiles, agentpkg.FileInput{
			MimeType: att.MimeType,
			Data:     att.Data,
			Name:     att.Name,
		})
	}

	response, err := h.agent.Run(agentCtx, userID, sessionID, text, agentFiles)
	durationMS := time.Since(start).Milliseconds()

	if err != nil {
		// Context cancellation from shutdown or client disconnect is not an LLM
		// failure — don't let it trip the circuit breaker during normal operations.
		if ctx.Err() != context.Canceled {
			h.circuitBreaker.RecordFailure()
		}
		h.audit.AgentFailed(userID, channel, sessionID, durationMS, err)
		h.logger.Error("agent execution failed", "error", err, "user", userID)
		// If parent context was cancelled (shutdown/disconnect), the Slack
		// connection is likely gone — don't attempt to send a response.
		if ctx.Err() == context.Canceled {
			return
		}
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

	// Format and deliver — update thinking message or post new.
	// Sanitize the fallback text for push notifications: the LLM emits standard
	// Markdown (**bold**, ## headings, [links](url)) despite instructions. Block Kit
	// blocks go through sanitizeForSlack inside FormatResponse, but the fallback
	// text is plain — literal "**Critical**" on a 3am push notification is ugly.
	blocks := FormatResponse(response, userID, durationMS)
	h.resolveThinking(channel, replyTS, thinkingTS, TruncateFallback(sanitizeForSlack(response)), blocks)
}

// postThinking posts the initial "thinking" message and returns its timestamp.
// The message is contextual based on the user's query to feel responsive.
// Returns "" if the post fails (caller should fall back to postMessage).
func (h *Handler) postThinking(channel, threadTS, userText string) string {
	thinkingText := inferThinkingMessage(userText)
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, ":hourglass_flowing_sand: "+thinkingText, false, false),
			nil, nil,
		),
	}
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(thinkingText, false),
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
// a new message if the thinking message wasn't created. If the update fails
// (message deleted, permission issue, Slack API error), falls back to posting
// a new message so the response is never silently lost.
func (h *Handler) resolveThinking(channel, threadTS, thinkingTS, fallback string, blocks []slack.Block) {
	if thinkingTS != "" {
		if err := h.updateMessage(channel, thinkingTS, fallback, blocks); err != nil {
			// Update failed — post as a new message rather than losing the response.
			if postErr := h.postMessage(channel, threadTS, fallback, blocks); postErr != nil {
				h.logger.Error("RESPONSE LOST: both update and post failed",
					"update_error", err, "post_error", postErr,
					"channel", channel, "thread", threadTS,
					"response_length", len(fallback),
				)
			}
		}
	} else {
		if err := h.postMessage(channel, threadTS, fallback, blocks); err != nil {
			h.logger.Error("RESPONSE LOST: failed to deliver response",
				"error", err, "channel", channel, "thread", threadTS,
				"response_length", len(fallback),
			)
		}
	}
}

// inferThinkingMessage generates a contextual thinking indicator based on user input.
func inferThinkingMessage(text string) string {
	lower := strings.ToLower(text)

	switch {
	case strings.Contains(lower, "compare") || containsWord(lower, "vs") || strings.Contains(lower, "versus") || strings.Contains(lower, "what changed") || strings.Contains(lower, "what's different"):
		return "*Comparing time periods...*"
	case containsWord(lower, "why") || strings.Contains(lower, "what happened") || strings.Contains(lower, "what's causing") || strings.Contains(lower, "broken") || containsWord(lower, "down") || strings.Contains(lower, "incident") || strings.Contains(lower, "outage") || containsWord(lower, "sev0") || containsWord(lower, "sev1") || containsWord(lower, "sev2") || containsWord(lower, "p0") || containsWord(lower, "p1") || containsWord(lower, "p2") || strings.Contains(lower, "sev-") || strings.Contains(lower, "root cause") || strings.Contains(lower, "failing") || strings.Contains(lower, "blast radius") || strings.Contains(lower, "affected") || containsWord(lower, "problem") || containsWord(lower, "problems") || strings.Contains(lower, "wrong") || strings.Contains(lower, "fire") || strings.Contains(lower, "alert") || strings.Contains(lower, "alarm") || strings.Contains(lower, "pager"):
		return "*Investigating...*"
	case strings.Contains(lower, "status") || strings.Contains(lower, "health") || strings.Contains(lower, "any issues") || strings.Contains(lower, "what's happening") || strings.Contains(lower, "what's going on") || strings.Contains(lower, "alive") || strings.Contains(lower, "running") || strings.Contains(lower, "overview") || strings.Contains(lower, "summary") || strings.Contains(lower, "situation"):
		return "*Checking service health...*"
	default:
		return "*Querying logs...*"
	}
}

// quickReplyFor returns a canned response and true if the message should be
// answered instantly without calling the LLM. Handles gratitude, greetings,
// empty mentions, and "help" — all of which must work even when the AI backend
// is down (circuit breaker open).
//
// The optional inThread parameter controls handling of ambiguous affirmatives
// ("yeah", "ok", "yep"). In thread follow-ups, these words often answer a
// bot question like "Want me to dig deeper?" — so they must reach the LLM.
// In top-level messages, they're safely treated as gratitude.
func quickReplyFor(text string, inThread ...bool) (string, bool) {
	lower := strings.ToLower(text)
	threadFollowup := len(inThread) > 0 && inThread[0]

	if threadFollowup {
		// In thread follow-ups, only short-circuit unambiguous gratitude.
		// "yeah", "ok", "sounds good" could be the user answering a question
		// the bot asked — e.g. "Want me to check upstream?" → "yeah"
		if isPureGratitude(lower) {
			return "You're welcome! Let me know if anything else comes up.", true
		}
	} else {
		if isGratitude(lower) {
			return "You're welcome! Let me know if anything else comes up.", true
		}
	}

	if isDismissal(lower) {
		return "No worries — let me know when you need anything!", true
	}

	// Strip punctuation for matching — "hey!" and "hello!!" should match
	cleaned := strings.Map(func(r rune) rune {
		if r == '!' || r == '.' || r == ',' || r == '?' {
			return -1
		}
		return r
	}, strings.TrimSpace(lower))

	// Empty mention (@LokiLens with no text) — guide the user without burning
	// an LLM call. The system instruction handles this too, but short-circuiting
	// saves latency, API cost, and works when the AI backend is down.
	if cleaned == "" {
		return "Hey! I'm LokiLens — ask me about logs, errors, or service health. What can I help with?", true
	}

	// Greetings
	switch cleaned {
	case "hi", "hello", "hey", "yo", "sup",
		"morning", "good morning", "gm",
		"afternoon", "good afternoon",
		"evening", "good evening",
		"night", "good night", "gn":
		return "Hey! I'm LokiLens — ask me about logs, errors, or service health. What can I help with?", true
	}

	// Help — must be available even during outages. This is when the user
	// most needs guidance, and the help text is static (no Loki queries needed).
	if isHelpRequest(cleaned) {
		return helpText, true
	}

	return "", false
}

// isHelpRequest returns true if the message is asking for help/usage guidance.
func isHelpRequest(cleaned string) bool {
	switch cleaned {
	case "help", "help me", "what can you do", "how do i use this",
		"what do you do", "how does this work", "usage":
		return true
	}
	return false
}

const helpText = `:wave: *I'm LokiLens — your team's log analysis assistant.*

Here are things I can help with:
• _"Show me errors from payments in the last hour"_
• _"Are there any issues right now?"_
• _"What's the error rate for orders vs yesterday?"_
• _"Which service has the most 5xx errors?"_
• _"Find timeout errors in gateway since 2pm"_
• _"Compare error rates across all services"_

I work best in threads — ask follow-ups and I'll remember context.`

// isGratitude returns true if the message is a simple thank-you or acknowledgment.
// Includes both pure gratitude and ambiguous affirmatives.
func isGratitude(lower string) bool {
	return isPureGratitude(lower) || isAffirmative(lower)
}

// isPureGratitude returns true for unambiguous "thank you" messages.
// These are safe to short-circuit even in thread follow-ups — "thanks"
// never means "yes, do that" in response to a bot question.
func isPureGratitude(lower string) bool {
	cleaned := strings.Map(func(r rune) rune {
		if r == '!' || r == '.' || r == ',' || r == '?' {
			return -1
		}
		return r
	}, strings.TrimSpace(lower))
	switch cleaned {
	case "thanks", "thank you", "thx", "ty", "cheers",
		"thanks a lot", "thank you so much", "much appreciated", "appreciated":
		return true
	}
	return false
}

// isAffirmative returns true for messages that could be either gratitude
// ("ok, I'm done") or an affirmative response ("ok, do it"). In top-level
// messages these are safe to short-circuit. But in thread follow-ups, the
// bot often asks "Want me to dig deeper?" and "yeah" means "yes, do it" —
// not "thanks, I'm done." These must reach the LLM in threads so it can
// use conversation context to determine the intent.
func isAffirmative(lower string) bool {
	cleaned := strings.Map(func(r rune) rune {
		if r == '!' || r == '.' || r == ',' || r == '?' {
			return -1
		}
		return r
	}, strings.TrimSpace(lower))
	switch cleaned {
	case "got it", "ok", "ok cool", "okay", "perfect",
		"great", "awesome", "nice", "cool",
		"no worries", "np", "nw", "all good", "sounds good",
		"looks good", "lgtm", "yep", "yup", "yea", "yeah",
		"noted", "copy that", "roger", "sweet", "nice one":
		return true
	}
	return false
}

// isDismissal returns true if the message is a user dismissing or canceling their
// request. Common in incident threads when the engineer figures it out themselves
// or decides to investigate differently. Without this, "nvm" triggers a full agent
// run — wasting tokens and cluttering the thread at 3am.
func isDismissal(lower string) bool {
	cleaned := strings.Map(func(r rune) rune {
		if r == '!' || r == '.' || r == ',' || r == '?' {
			return -1
		}
		return r
	}, strings.TrimSpace(lower))
	switch cleaned {
	case "nvm", "nevermind", "never mind", "cancel",
		"forget it", "skip it", "nah", "nah its fine",
		"figured it out", "found it", "i found it",
		"dont worry about it", "don't worry about it",
		"all set", "all sorted", "sorted", "resolved",
		"all clear", "false alarm",
		"im good", "i'm good", "were good", "we're good":
		return true
	}
	return false
}

// containsWord checks if a word appears as a standalone word in the text,
// avoiding false positives from substrings like "breakdown" matching "down".
func containsWord(text, word string) bool {
	if word == "" {
		return false
	}
	idx := 0
	for {
		pos := strings.Index(text[idx:], word)
		if pos == -1 {
			return false
		}
		pos += idx
		// Check word boundary before
		if pos > 0 && !isWordBoundary(text[pos-1]) {
			idx = pos + len(word)
			continue
		}
		// Check word boundary after
		end := pos + len(word)
		if end < len(text) && !isWordBoundary(text[end]) {
			idx = pos + len(word)
			continue
		}
		return true
	}
}

// isWordBoundary returns true if the byte is a character that separates words.
// Includes punctuation common in Slack messages (colons in emoji shortcodes,
// parentheses in asides, semicolons).
func isWordBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', ',', '.', '!', '?', ':', ';', '(', ')', '"', '\'':
		return true
	}
	return false
}

func (h *Handler) resolveSessionID(channel, threadTS string) string {
	if h.workspaceID != "" {
		return fmt.Sprintf("%s:%s:%s", h.workspaceID, channel, threadTS)
	}
	return fmt.Sprintf("%s:%s", channel, threadTS)
}

func (h *Handler) postMessage(channel, threadTS, fallbackText string, blocks []slack.Block) error {
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
	return err
}

func (h *Handler) updateMessage(channel, timestamp, fallbackText string, blocks []slack.Block) error {
	_, _, _, err := h.slackAPI.UpdateMessage(channel, timestamp,
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(fallbackText, false),
	)
	if err != nil {
		h.logger.Error("failed to update message", "error", err, "channel", channel)
	}
	return err
}

// IsBotMessage returns true if the message should be ignored (bot messages, subtypes).
// file_share is allowed through so the bot can process uploaded images and files.
func IsBotMessage(botID, subType string) bool {
	if botID != "" {
		return true
	}
	if subType == "" || subType == "file_share" {
		return false
	}
	return true
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
		cutoff := time.Now().Add(-6 * time.Hour)
		for k, t := range h.activeThreads {
			if t.Before(cutoff) {
				delete(h.activeThreads, k)
			}
		}
		// If still over capacity after time-based eviction (e.g. major incident
		// with thousands of threads in under 6 hours), sort by age and evict
		// the oldest 10%. Same strategy as incrementTurns — prevents unbounded
		// growth without randomly evicting active incident threads.
		if len(h.activeThreads) > maxTrackedThreads {
			type kv struct {
				key      string
				lastSeen time.Time
			}
			entries := make([]kv, 0, len(h.activeThreads))
			for k, t := range h.activeThreads {
				entries = append(entries, kv{k, t})
			}
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].lastSeen.Before(entries[j].lastSeen)
			})
			toRemove := len(entries) / 10
			if toRemove == 0 {
				toRemove = 1
			}
			for i := 0; i < toRemove; i++ {
				delete(h.activeThreads, entries[i].key)
			}
		}
	}
	h.activeThreadsMu.Unlock()
}

// IsActiveThread checks if the bot is participating in a thread.
// Respects the 6-hour session TTL — if the session has expired, the thread
// shouldn't auto-capture messages either. Without this, a user posting in a
// stale thread gets a response from a bot with no conversation context,
// which is confusing ("why did it forget everything?").
func (h *Handler) IsActiveThread(channel, threadTS string) bool {
	if threadTS == "" {
		return false
	}
	key := channel + ":" + threadTS
	h.activeThreadsMu.RLock()
	defer h.activeThreadsMu.RUnlock()
	t, ok := h.activeThreads[key]
	if !ok {
		return false
	}
	return time.Since(t) < 6*time.Hour
}

// incrementTurns tracks conversation length and returns the updated count.
// Uses LRU eviction to avoid clearing all entries when the map grows too large.
func (h *Handler) incrementTurns(sessionID string) int {
	h.turnCountsMu.Lock()
	defer h.turnCountsMu.Unlock()

	now := time.Now()

	// LRU eviction: remove entries older than 6 hours when at capacity.
	// Must match the session TTL (6h) — otherwise turn counts reset mid-session
	// and the 50-turn limit becomes ineffective during long incidents.
	if len(h.turnCounts) >= maxTrackedSessions {
		cutoff := now.Add(-6 * time.Hour)
		for k, v := range h.turnCounts {
			if v.lastSeen.Before(cutoff) {
				delete(h.turnCounts, k)
			}
		}
		// If still over capacity after time-based eviction, remove the
		// actually oldest 10% by lastSeen. The previous implementation
		// iterated in Go's random map order, which could evict active
		// sessions during a large incident with many concurrent threads.
		if len(h.turnCounts) >= maxTrackedSessions {
			type kv struct {
				key      string
				lastSeen time.Time
			}
			entries := make([]kv, 0, len(h.turnCounts))
			for k, v := range h.turnCounts {
				entries = append(entries, kv{k, v.lastSeen})
			}
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].lastSeen.Before(entries[j].lastSeen)
			})
			toRemove := len(entries) / 10
			if toRemove == 0 {
				toRemove = 1
			}
			for i := 0; i < toRemove; i++ {
				delete(h.turnCounts, entries[i].key)
			}
		}
	}

	entry := h.turnCounts[sessionID]
	entry.count++
	entry.lastSeen = now
	h.turnCounts[sessionID] = entry
	return entry.count
}

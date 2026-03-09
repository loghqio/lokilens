package bot

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	agentpkg "github.com/lokilens/lokilens/internal/agent"
	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/safety"
)

const maxConcurrentRequests = 20

// Config holds the bot's configuration.
type Config struct {
	BotToken       string
	AppToken       string
	Agent          *agentpkg.Agent
	RateLimiter    *safety.RateLimiter
	PIIFilter      *safety.PIIFilter
	PromptGuard    *safety.PromptGuard
	CircuitBreaker *safety.CircuitBreaker
	AuditLogger    *audit.Logger
	Logger         *slog.Logger

	// Multi-tenant fields (optional, empty for single-tenant)
	WorkspaceID     string
	WorkspaceStatus string       // "active" or "pending_setup"; empty = active
	UsageChecker    UsageChecker // nil = no usage tracking (BYOK or single-tenant)
}

// Bot is the Slack bot that listens for messages and delegates to the agent.
type Bot struct {
	api     *slack.Client
	client  *socketmode.Client
	handler *Handler
	logger  *slog.Logger

	ctx context.Context
	sem chan struct{}
	wg  sync.WaitGroup
}

// New creates a new Slack bot.
func New(cfg Config) (*Bot, error) {
	api := slack.New(
		cfg.BotToken,
		slack.OptionAppLevelToken(cfg.AppToken),
	)

	client := socketmode.New(api)

	handler := NewHandler(HandlerConfig{
		SlackAPI:        api,
		Agent:           cfg.Agent,
		RateLimiter:     cfg.RateLimiter,
		PIIFilter:       cfg.PIIFilter,
		PromptGuard:     cfg.PromptGuard,
		CircuitBreaker:  cfg.CircuitBreaker,
		AuditLogger:     cfg.AuditLogger,
		Logger:          cfg.Logger,
		WorkspaceID:     cfg.WorkspaceID,
		WorkspaceStatus: cfg.WorkspaceStatus,
		UsageChecker:    cfg.UsageChecker,
	})

	return &Bot{
		api:     api,
		client:  client,
		handler: handler,
		logger:  cfg.Logger,
		sem:     make(chan struct{}, maxConcurrentRequests),
	}, nil
}

// API returns the underlying Slack API client.
func (b *Bot) API() *slack.Client {
	return b.api
}

// Run starts the Slack bot and blocks until the context is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	b.ctx = ctx

	smHandler := socketmode.NewSocketmodeHandler(b.client)

	// Connection lifecycle
	smHandler.Handle(socketmode.EventTypeConnecting, b.onConnecting)
	smHandler.Handle(socketmode.EventTypeConnected, b.onConnected)
	smHandler.Handle(socketmode.EventTypeConnectionError, b.onConnectionError)

	// Events API
	smHandler.HandleEvents(slackevents.AppMention, b.onAppMention)
	smHandler.HandleEvents(slackevents.Message, b.onMessage)

	// Default handler for unhandled events
	smHandler.HandleDefault(func(evt *socketmode.Event, client *socketmode.Client) {})

	b.logger.Info("starting Slack bot in Socket Mode")
	err := smHandler.RunEventLoopContext(ctx)

	// Wait for in-flight requests to finish on shutdown
	b.wg.Wait()
	return err
}

// dispatch runs a handler in a bounded goroutine pool, derived from the bot's context.
func (b *Bot) dispatch(channel, userID, text, threadTS, messageTS, source string) {
	select {
	case b.sem <- struct{}{}:
	default:
		b.logger.Warn("too many concurrent requests, dropping message", "user", userID)
		// Best-effort feedback so the user knows their message wasn't ignored
		replyTS := threadTS
		if replyTS == "" {
			replyTS = messageTS
		}
		go func() {
			opts := []slack.MsgOption{
				slack.MsgOptionText("LokiLens is currently handling too many requests. Please try again in a moment.", false),
			}
			if replyTS != "" {
				opts = append(opts, slack.MsgOptionTS(replyTS))
			}
			_, _, _ = b.api.PostMessage(channel, opts...)
		}()
		return
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() { <-b.sem }()
		b.handler.HandleMessage(b.ctx, channel, userID, text, threadTS, messageTS, source)
	}()
}

func (b *Bot) onConnecting(evt *socketmode.Event, client *socketmode.Client) {
	b.logger.Info("connecting to Slack...")
}

func (b *Bot) onConnected(evt *socketmode.Event, client *socketmode.Client) {
	b.logger.Info("connected to Slack")
}

func (b *Bot) onConnectionError(evt *socketmode.Event, client *socketmode.Client) {
	b.logger.Warn("Slack connection error, will retry")
}

func (b *Bot) onAppMention(evt *socketmode.Event, client *socketmode.Client) {
	client.Ack(*evt.Request)

	eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}

	mention, ok := eventsAPI.InnerEvent.Data.(*slackevents.AppMentionEvent)
	if !ok {
		return
	}

	// Skip DM mentions — onMessage handles DMs exclusively to avoid duplicates
	if IsDM(mention.Channel) {
		return
	}

	query := StripMention(mention.Text)
	if query == "" {
		return
	}

	b.dispatch(mention.Channel, mention.User, query, mention.ThreadTimeStamp, mention.TimeStamp, "mention")
}

func (b *Bot) onMessage(evt *socketmode.Event, client *socketmode.Client) {
	client.Ack(*evt.Request)

	eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}

	msgEvent, ok := eventsAPI.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}

	// Ignore bot messages to prevent loops
	if IsBotMessage(msgEvent.BotID, msgEvent.SubType) {
		return
	}

	if msgEvent.Text == "" {
		return
	}

	// Handle DMs — no @mention required, strip mention if present
	if IsDM(msgEvent.Channel) {
		text := StripMention(msgEvent.Text)
		if text == "" {
			return
		}
		b.dispatch(msgEvent.Channel, msgEvent.User, text, msgEvent.ThreadTimeStamp, msgEvent.TimeStamp, "dm")
		return
	}

	// Handle thread follow-ups in channels where bot is active (no @mention needed)
	if msgEvent.ThreadTimeStamp != "" && b.handler.IsActiveThread(msgEvent.Channel, msgEvent.ThreadTimeStamp) {
		// Skip @mentions — they're handled by onAppMention
		if strings.HasPrefix(strings.TrimSpace(msgEvent.Text), "<@") {
			return
		}
		b.dispatch(msgEvent.Channel, msgEvent.User, msgEvent.Text, msgEvent.ThreadTimeStamp, msgEvent.TimeStamp, "thread_followup")
		return
	}
}

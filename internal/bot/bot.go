package bot

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
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

	// selfMention is the Slack mention tag for this bot (e.g. "<@U12345>").
	// Used to distinguish the bot's own mentions from mentions of other users
	// in thread follow-up deduplication.
	selfMention string

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

	// Resolve the bot's own user ID so we can distinguish self-mentions
	// from mentions of other users in thread follow-up deduplication.
	authResp, err := api.AuthTest()
	if err != nil {
		return nil, fmt.Errorf("slack auth test: %w", err)
	}
	selfMention := "<@" + authResp.UserID + ">"

	// Custom dialer with aggressive TCP keep-alive to prevent network
	// intermediaries (routers, firewalls, NAT) from killing idle WebSocket
	// connections. Without this, we see close 1006 every 10 seconds.
	wsDialer := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 45 * time.Second,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 5 * time.Second,
		}).DialContext,
	}

	client := socketmode.New(api,
		socketmode.OptionDialer(wsDialer),
		socketmode.OptionDebug(true),
		socketmode.OptionLog(log.New(os.Stderr, "socketmode: ", log.LstdFlags)),
	)

	handler := NewHandler(HandlerConfig{
		SlackAPI:        api,
		Agent:           cfg.Agent,
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
		api:         api,
		client:      client,
		handler:     handler,
		logger:      cfg.Logger,
		selfMention: selfMention,
		sem:         make(chan struct{}, maxConcurrentRequests),
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
	// Handle "hello" explicitly so it doesn't hit the default handler
	smHandler.Handle(socketmode.EventTypeHello, func(evt *socketmode.Event, client *socketmode.Client) {})

	// Events API
	smHandler.HandleEvents(slackevents.AppMention, b.onAppMention)
	smHandler.HandleEvents(slackevents.Message, b.onMessage)

	// Default handler — ack any event we don't explicitly handle.
	// Without this, unsubscribed Events API types (reaction_added, channel_join, etc.)
	// go unacknowledged, causing Slack to retry and eventually show
	// "Something went wrong processing your request" to the user.
	// Only ack events with non-empty envelope IDs — the "hello" message has
	// an empty envelope ID and acking it sends a malformed response to Slack.
	smHandler.HandleDefault(func(evt *socketmode.Event, client *socketmode.Client) {
		if evt.Request != nil && evt.Request.EnvelopeID != "" {
			client.Ack(*evt.Request)
		}
	})

	b.logger.Info("starting Slack bot in Socket Mode")
	err := smHandler.RunEventLoopContext(ctx)

	// Wait for in-flight requests to finish on shutdown
	b.wg.Wait()
	return err
}

// dispatch runs a handler in a bounded goroutine pool, derived from the bot's context.
func (b *Bot) dispatch(channel, userID, text, threadTS, messageTS, source string, attachments []Attachment) {
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
		defer func() {
			if r := recover(); r != nil {
				b.logger.Error("panic in message handler (recovered)",
					"panic", fmt.Sprintf("%v", r),
					"user", userID,
					"channel", channel,
					"source", source,
				)
				// Surface the error in Slack so engineers know something broke
				replyTS := threadTS
				if replyTS == "" {
					replyTS = messageTS
				}
				errMsg := "Something unexpected happened while processing your request. The team has been notified — please try again."
				opts := []slack.MsgOption{
					slack.MsgOptionBlocks(FormatError(errMsg)...),
					slack.MsgOptionText(errMsg, false),
				}
				if replyTS != "" {
					opts = append(opts, slack.MsgOptionTS(replyTS))
				}
				_, _, _ = b.api.PostMessage(channel, opts...)
			}
		}()
		b.handler.HandleMessage(b.ctx, channel, userID, text, threadTS, messageTS, source, attachments)
	}()
}

// supportedMimeTypes lists the MIME types Gemini can process as inline data.
var supportedMimeTypes = map[string]bool{
	// Images
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
	// Video
	"video/mp4": true, "video/mpeg": true, "video/quicktime": true, "video/webm": true,
	"video/x-msvideo": true, "video/3gpp": true,
	// Audio
	"audio/mpeg": true, "audio/wav": true, "audio/ogg": true, "audio/flac": true,
	// Documents
	"application/pdf": true, "text/plain": true,
}

// fetchAttachments downloads files from a Slack message for multimodal processing.
func (b *Bot) fetchAttachments(channel, messageTS string) []Attachment {
	resp, err := b.api.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: channel,
		Latest:    messageTS,
		Oldest:    messageTS,
		Inclusive: true,
		Limit:     1,
	})
	if err != nil || len(resp.Messages) == 0 {
		return nil
	}

	files := resp.Messages[0].Files
	if len(files) == 0 {
		return nil
	}

	var attachments []Attachment
	for _, f := range files {
		if !supportedMimeTypes[f.Mimetype] {
			b.logger.Info("skipping unsupported file type", "name", f.Name, "mimetype", f.Mimetype)
			continue
		}
		if f.Size > maxAttachmentSize {
			b.logger.Info("skipping oversized file", "name", f.Name, "size", f.Size)
			continue
		}

		url := f.URLPrivateDownload
		if url == "" {
			url = f.URLPrivate
		}
		if url == "" {
			continue
		}

		var buf bytes.Buffer
		if err := b.api.GetFileContext(context.Background(), url, &buf); err != nil {
			b.logger.Error("failed to download file", "name", f.Name, "error", err)
			continue
		}

		attachments = append(attachments, Attachment{
			MimeType: f.Mimetype,
			Data:     buf.Bytes(),
			Name:     f.Name,
		})
		b.logger.Info("downloaded attachment", "name", f.Name, "mimetype", f.Mimetype, "size", buf.Len())
	}

	return attachments
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
		b.logger.Warn("app_mention: unexpected data type", "type", fmt.Sprintf("%T", evt.Data))
		return
	}

	mention, ok := eventsAPI.InnerEvent.Data.(*slackevents.AppMentionEvent)
	if !ok {
		b.logger.Warn("app_mention: unexpected inner event type", "type", eventsAPI.InnerEvent.Type)
		return
	}

	b.logger.Info("app_mention received", "user", mention.User, "channel", mention.Channel, "text", mention.Text)

	// Skip DM mentions — onMessage handles DMs exclusively to avoid duplicates
	if IsDM(mention.Channel) {
		return
	}

	query := StripMention(mention.Text)

	// Fetch any file attachments from the message
	attachments := b.fetchAttachments(mention.Channel, mention.TimeStamp)

	// Don't drop empty queries — handler.quickReplyFor("") returns a greeting.
	// Silently ignoring @LokiLens with no text looks like the bot is broken.
	b.dispatch(mention.Channel, mention.User, query, mention.ThreadTimeStamp, mention.TimeStamp, "mention", attachments)
}

func (b *Bot) onMessage(evt *socketmode.Event, client *socketmode.Client) {
	client.Ack(*evt.Request)

	eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		b.logger.Warn("message: unexpected data type", "type", fmt.Sprintf("%T", evt.Data))
		return
	}

	msgEvent, ok := eventsAPI.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		b.logger.Warn("message: unexpected inner event type", "type", eventsAPI.InnerEvent.Type)
		return
	}

	// Ignore bot messages to prevent loops
	if IsBotMessage(msgEvent.BotID, msgEvent.SubType) {
		return
	}

	// Check for file attachments (file_share subtype or new upload API)
	hasFiles := msgEvent.SubType == "file_share"

	if msgEvent.Text == "" && !hasFiles {
		return
	}

	// Handle DMs — no @mention required, strip mention if present
	if IsDM(msgEvent.Channel) {
		b.logger.Info("dm received", "user", msgEvent.User, "channel", msgEvent.Channel, "text", msgEvent.Text, "has_files", hasFiles)
		text := StripMention(msgEvent.Text)
		var attachments []Attachment
		if hasFiles {
			attachments = b.fetchAttachments(msgEvent.Channel, msgEvent.TimeStamp)
		}
		// Don't drop empty text — handler.quickReplyFor("") returns a greeting.
		b.dispatch(msgEvent.Channel, msgEvent.User, text, msgEvent.ThreadTimeStamp, msgEvent.TimeStamp, "dm", attachments)
		return
	}

	// Handle thread follow-ups in channels where bot is active (no @mention needed)
	if msgEvent.ThreadTimeStamp != "" && b.handler.IsActiveThread(msgEvent.Channel, msgEvent.ThreadTimeStamp) {
		// Skip messages that mention this bot — onAppMention handles those.
		// Only check for the bot's own mention, not any <@...> mention:
		// "I talked to @jsmith about this, any errors?" should still be processed
		// as a thread follow-up, but "hey @LokiLens drill into that" must be
		// left to onAppMention to avoid double dispatch.
		if strings.Contains(msgEvent.Text, b.selfMention) {
			return
		}
		b.logger.Info("thread_followup received", "user", msgEvent.User, "channel", msgEvent.Channel, "text", msgEvent.Text, "has_files", hasFiles)
		var attachments []Attachment
		if hasFiles {
			attachments = b.fetchAttachments(msgEvent.Channel, msgEvent.TimeStamp)
		}
		b.dispatch(msgEvent.Channel, msgEvent.User, msgEvent.Text, msgEvent.ThreadTimeStamp, msgEvent.TimeStamp, "thread_followup", attachments)
		return
	}
}

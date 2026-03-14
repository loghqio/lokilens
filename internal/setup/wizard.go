package setup

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/lokilens/lokilens/internal/loki"
	"github.com/lokilens/lokilens/internal/store"
)

// action IDs for interactive components
const (
	ActionSetupLokiURL    = "setup_loki_url"
	ActionSetupGeminiKey  = "setup_gemini_key"
	ActionSetupFreeTier   = "setup_free_tier"
	CallbackLokiConfig    = "callback_loki_config"
	CallbackGeminiConfig  = "callback_gemini_config"
)

// WorkspaceReloader can reload a workspace's bot after config changes.
type WorkspaceReloader interface {
	ReloadWorkspace(ctx context.Context, workspaceID string) error
}

// Wizard manages the in-Slack setup flow for new workspaces.
type Wizard struct {
	store    store.WorkspaceStore
	reloader WorkspaceReloader
	logger   *slog.Logger
}

// New creates a setup wizard.
func New(s store.WorkspaceStore, reloader WorkspaceReloader, logger *slog.Logger) *Wizard {
	return &Wizard{store: s, reloader: reloader, logger: logger}
}

// StartSetup sends the welcome DM to the installer with a setup button.
func (w *Wizard) StartSetup(api *slack.Client, workspaceID, userID string) error {
	// Open a DM channel with the installer
	channel, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{
		Users: []string{userID},
	})
	if err != nil {
		return fmt.Errorf("opening DM with installer: %w", err)
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType,
				"*Welcome to LokiLens!*\nLet's connect your Grafana Loki instance so your team can query logs with natural language.",
				false, false),
			nil, nil,
		),
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType,
				"I'll need:\n1. Your Loki URL\n2. Loki API key (if required)\n3. Gemini API key (optional — skip for free tier with 100 queries/day)",
				false, false),
			nil, nil,
		),
		slack.NewActionBlock(
			"setup_actions",
			slack.NewButtonBlockElement(ActionSetupLokiURL, workspaceID,
				slack.NewTextBlockObject(slack.PlainTextType, "Configure Loki", false, false)).
				WithStyle(slack.StylePrimary),
		),
	}

	_, _, err = api.PostMessage(channel.ID,
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText("Welcome to LokiLens! Let's set up your workspace.", false),
	)
	if err != nil {
		return fmt.Errorf("posting setup message: %w", err)
	}

	return nil
}

// OpenLokiConfigModal opens a modal for Loki URL and API key input.
func (w *Wizard) OpenLokiConfigModal(api *slack.Client, triggerID, workspaceID string) error {
	urlInput := slack.NewInputBlock(
		"loki_url",
		slack.NewTextBlockObject(slack.PlainTextType, "Loki Base URL", false, false),
		nil,
		slack.NewPlainTextInputBlockElement(
			slack.NewTextBlockObject(slack.PlainTextType, "https://loki.example.com", false, false),
			"loki_url_input",
		),
	)

	apiKeyInput := slack.NewInputBlock(
		"loki_api_key",
		slack.NewTextBlockObject(slack.PlainTextType, "API Key (leave blank if none)", false, false),
		nil,
		slack.NewPlainTextInputBlockElement(
			slack.NewTextBlockObject(slack.PlainTextType, "Optional", false, false),
			"loki_api_key_input",
		),
	)
	apiKeyInput.Optional = true

	modal := slack.ModalViewRequest{
		Type:            "modal",
		CallbackID:      CallbackLokiConfig,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Loki Configuration", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Connect", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		PrivateMetadata: workspaceID,
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{urlInput, apiKeyInput},
		},
	}

	_, err := api.OpenView(triggerID, modal)
	if err != nil {
		return fmt.Errorf("opening loki config modal: %w", err)
	}
	return nil
}

// HandleLokiConfigSubmission processes the Loki config modal submission.
// Returns nil on success, or an error map for inline validation errors.
func (w *Wizard) HandleLokiConfigSubmission(ctx context.Context, api *slack.Client, workspaceID, userID string, values map[string]map[string]slack.BlockAction) (map[string]string, error) {
	lokiURL := values["loki_url"]["loki_url_input"].Value
	lokiAPIKey := ""
	if v, ok := values["loki_api_key"]; ok {
		lokiAPIKey = v["loki_api_key_input"].Value
	}

	if lokiURL == "" {
		return map[string]string{"loki_url": "Loki URL is required"}, nil
	}

	// SSRF protection: validate the URL before making any requests
	if err := validateLokiURL(lokiURL); err != nil {
		return map[string]string{"loki_url": err.Error()}, nil
	}

	// Validate the Loki connection
	client := loki.NewHTTPClient(loki.ClientConfig{
		BaseURL:    lokiURL,
		APIKey:     lokiAPIKey,
		Timeout:    10 * time.Second,
		MaxRetries: 1,
		Logger:     w.logger,
	})

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := client.Labels(checkCtx, loki.LabelsRequest{})
	if err != nil {
		w.logger.Warn("loki connection check failed", "workspace", workspaceID, "error", err)
		return map[string]string{"loki_url": "Could not connect to Loki — check the URL and API key, and ensure the endpoint is reachable."}, nil
	}

	// Save Loki config to workspace
	ws, err := w.store.Get(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("getting workspace: %w", err)
	}

	ws.LokiURL = lokiURL
	ws.LokiAPIKey = lokiAPIKey

	if err := w.store.Update(ctx, ws); err != nil {
		return nil, fmt.Errorf("updating workspace: %w", err)
	}

	w.logger.Info("loki configured", "workspace", workspaceID, "loki_url", lokiURL)

	// Send Gemini key choice message
	go w.sendGeminiChoice(api, workspaceID, userID)

	return nil, nil
}

// sendGeminiChoice sends a DM with buttons for Gemini API key vs free tier.
func (w *Wizard) sendGeminiChoice(api *slack.Client, workspaceID, userID string) {
	channel, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{
		Users: []string{userID},
	})
	if err != nil {
		w.logger.Error("failed to open DM for gemini choice", "error", err)
		return
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType,
				"*Loki connected!* Now let's set up AI.\n\nYou can bring your own Gemini API key for unlimited queries, or use the free tier (100 queries/day).",
				false, false),
			nil, nil,
		),
		slack.NewActionBlock(
			"gemini_actions",
			slack.NewButtonBlockElement(ActionSetupGeminiKey, workspaceID,
				slack.NewTextBlockObject(slack.PlainTextType, "Enter Gemini API Key", false, false)),
			slack.NewButtonBlockElement(ActionSetupFreeTier, workspaceID,
				slack.NewTextBlockObject(slack.PlainTextType, "Use Free Tier", false, false)).
				WithStyle(slack.StylePrimary),
		),
	}

	if _, _, err = api.PostMessage(channel.ID,
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText("Choose your Gemini API key setup.", false),
	); err != nil {
		w.logger.Error("failed to send gemini choice message", "workspace", workspaceID, "error", err)
	}
}

// OpenGeminiConfigModal opens a modal for Gemini API key input.
func (w *Wizard) OpenGeminiConfigModal(api *slack.Client, triggerID, workspaceID string) error {
	apiKeyInput := slack.NewInputBlock(
		"gemini_api_key",
		slack.NewTextBlockObject(slack.PlainTextType, "Gemini API Key", false, false),
		nil,
		slack.NewPlainTextInputBlockElement(
			slack.NewTextBlockObject(slack.PlainTextType, "AIza...", false, false),
			"gemini_api_key_input",
		),
	)

	modal := slack.ModalViewRequest{
		Type:            "modal",
		CallbackID:      CallbackGeminiConfig,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Gemini API Key", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Save", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		PrivateMetadata: workspaceID,
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{apiKeyInput},
		},
	}

	_, err := api.OpenView(triggerID, modal)
	if err != nil {
		return fmt.Errorf("opening gemini config modal: %w", err)
	}
	return nil
}

// HandleGeminiConfigSubmission saves the Gemini API key and activates the workspace.
func (w *Wizard) HandleGeminiConfigSubmission(ctx context.Context, api *slack.Client, workspaceID, userID string, values map[string]map[string]slack.BlockAction) error {
	geminiKey := values["gemini_api_key"]["gemini_api_key_input"].Value

	ws, err := w.store.Get(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("getting workspace: %w", err)
	}

	ws.GeminiAPIKey = geminiKey
	ws.Status = store.StatusActive

	if err := w.store.Update(ctx, ws); err != nil {
		return fmt.Errorf("updating workspace: %w", err)
	}

	// Reload the bot with new config
	if err := w.reloader.ReloadWorkspace(ctx, workspaceID); err != nil {
		w.logger.Error("failed to reload workspace after gemini config", "workspace", workspaceID, "error", err)
	}

	w.sendCompletionMessage(api, workspaceID, userID, false)
	return nil
}

// HandleFreeTier activates the workspace with the shared Gemini key.
func (w *Wizard) HandleFreeTier(ctx context.Context, api *slack.Client, workspaceID, userID string) error {
	ws, err := w.store.Get(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("getting workspace: %w", err)
	}

	ws.GeminiAPIKey = "" // empty = use shared key
	ws.Status = store.StatusActive

	if err := w.store.Update(ctx, ws); err != nil {
		return fmt.Errorf("updating workspace: %w", err)
	}

	// Reload the bot with new config
	if err := w.reloader.ReloadWorkspace(ctx, workspaceID); err != nil {
		w.logger.Error("failed to reload workspace after free tier selection", "workspace", workspaceID, "error", err)
	}

	w.sendCompletionMessage(api, workspaceID, userID, true)
	return nil
}

func (w *Wizard) sendCompletionMessage(api *slack.Client, workspaceID, userID string, freeTier bool) {
	channel, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{
		Users: []string{userID},
	})
	if err != nil {
		w.logger.Error("failed to open DM for completion", "error", err)
		return
	}

	tierInfo := "You're using your own Gemini API key (unlimited queries)."
	if freeTier {
		tierInfo = "You're on the free tier (100 queries/day). Run `/lokilens-setup` anytime to add your own Gemini API key for unlimited queries."
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType,
				fmt.Sprintf("*LokiLens is ready!*\n\n%s\n\n*Get started:*\n- Mention `@LokiLens` in any channel\n- DM me directly\n- Run `/lokilens-setup` anytime to reconfigure\n\nTry: _\"Show me errors from the last hour\"_", tierInfo),
				false, false),
			nil, nil,
		),
	}

	if _, _, err = api.PostMessage(channel.ID,
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText("LokiLens is ready! Try asking about your logs.", false),
	); err != nil {
		w.logger.Error("failed to send setup completion message", "workspace", workspaceID, "user", userID, "error", err)
	}
}

// validateLokiURL checks that a user-supplied Loki URL is safe to connect to.
// Prevents SSRF by rejecting internal/private network addresses.
func validateLokiURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}

	// Only allow http and https schemes
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must use http:// or https://")
	}

	if u.Host == "" {
		return fmt.Errorf("URL must include a hostname")
	}

	// Extract the hostname (without port)
	hostname := u.Hostname()

	// Reject obvious internal hostnames
	if hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1" || hostname == "0.0.0.0" {
		return fmt.Errorf("localhost URLs are not allowed in multi-tenant mode")
	}

	// Resolve DNS and check for private IP ranges
	ips, err := net.LookupHost(hostname)
	if err != nil {
		return fmt.Errorf("cannot resolve hostname %q: %v", hostname, err)
	}

	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("URL resolves to a private/internal address (%s) — use a public Loki endpoint", ipStr)
		}
	}

	return nil
}

package oauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/lokilens/lokilens/internal/store"
)

const (
	// maxRequestBodySize limits request body reads to prevent OOM from oversized payloads.
	maxRequestBodySize = 1 << 20 // 1 MB

	// oauthStateTTL is how long an OAuth state token is valid.
	oauthStateTTL = 10 * time.Minute
)

// WorkspaceManager manages workspace bot lifecycles.
type WorkspaceManager interface {
	AddWorkspace(ctx context.Context, workspaceID string) error
	GetBundle(workspaceID string) interface{ Bot() interface{ API() *slack.Client } }
}

// SetupWizard triggers the setup flow for a workspace.
type SetupWizard interface {
	StartSetup(api *slack.Client, workspaceID, userID string) error
	OpenLokiConfigModal(api *slack.Client, triggerID, workspaceID string) error
	OpenGeminiConfigModal(api *slack.Client, triggerID, workspaceID string) error
	HandleLokiConfigSubmission(ctx context.Context, api *slack.Client, workspaceID, userID string, values map[string]map[string]slack.BlockAction) (map[string]string, error)
	HandleGeminiConfigSubmission(ctx context.Context, api *slack.Client, workspaceID, userID string, values map[string]map[string]slack.BlockAction) error
	HandleFreeTier(ctx context.Context, api *slack.Client, workspaceID, userID string) error
}

// Handler handles OAuth callbacks and Slack interactions.
type Handler struct {
	clientID      string
	clientSecret  string
	signingSecret string
	appToken      string
	baseURL       string
	store         store.WorkspaceStore
	wizard        SetupWizard
	mgr           WorkspaceAdder
	logger        *slog.Logger

	// slackAPIForWorkspace returns a Slack API client for the given workspace.
	// Set by the caller after construction.
	SlackAPIForWorkspace func(workspaceID string) *slack.Client

	// oauthStates stores CSRF tokens for the OAuth flow.
	oauthStatesMu sync.Mutex
	oauthStates   map[string]time.Time
}

// WorkspaceAdder can add a new workspace to the bot manager at runtime.
type WorkspaceAdder interface {
	AddWorkspace(ctx context.Context, workspaceID string) error
}

// Config holds configuration for the OAuth handler.
type Config struct {
	ClientID      string
	ClientSecret  string
	SigningSecret string
	AppToken      string
	BaseURL       string
	Store         store.WorkspaceStore
	Wizard        SetupWizard
	Manager       WorkspaceAdder
	Logger        *slog.Logger
}

// NewHandler creates an OAuth handler.
func NewHandler(cfg Config) *Handler {
	return &Handler{
		clientID:      cfg.ClientID,
		clientSecret:  cfg.ClientSecret,
		signingSecret: cfg.SigningSecret,
		appToken:      cfg.AppToken,
		baseURL:       cfg.BaseURL,
		store:         cfg.Store,
		wizard:        cfg.Wizard,
		mgr:           cfg.Manager,
		logger:        cfg.Logger,
		oauthStates:   make(map[string]time.Time),
	}
}

// RegisterRoutes adds OAuth and interaction routes to the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /slack/oauth/start", h.HandleStart)
	mux.HandleFunc("GET /slack/oauth/callback", h.HandleCallback)
	mux.HandleFunc("POST /slack/interactions", h.HandleInteraction)
	mux.HandleFunc("POST /slack/commands", h.HandleSlashCommand)
}

// HandleStart redirects to Slack's OAuth authorize URL with a CSRF state token.
func (h *Handler) HandleStart(w http.ResponseWriter, r *http.Request) {
	scopes := []string{
		"app_mentions:read",
		"channels:history", "channels:read",
		"chat:write",
		"groups:history", "groups:read",
		"im:history", "im:read", "im:write",
		"mpim:history", "mpim:read",
		"commands",
	}

	state := h.generateOAuthState()

	authURL := fmt.Sprintf(
		"https://slack.com/oauth/v2/authorize?client_id=%s&scope=%s&redirect_uri=%s&state=%s",
		url.QueryEscape(h.clientID),
		url.QueryEscape(strings.Join(scopes, ",")),
		url.QueryEscape(h.baseURL+"/slack/oauth/callback"),
		url.QueryEscape(state),
	)

	http.Redirect(w, r, authURL, http.StatusFound)
}

// generateOAuthState creates a random state token and stores it with a TTL.
func (h *Handler) generateOAuthState() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		h.logger.Error("failed to generate OAuth state", "error", err)
		return ""
	}
	state := hex.EncodeToString(b)

	h.oauthStatesMu.Lock()
	// Evict expired states
	now := time.Now()
	for k, v := range h.oauthStates {
		if now.After(v) {
			delete(h.oauthStates, k)
		}
	}
	h.oauthStates[state] = now.Add(oauthStateTTL)
	h.oauthStatesMu.Unlock()

	return state
}

// validateOAuthState checks and consumes a state token. Returns false if invalid or expired.
func (h *Handler) validateOAuthState(state string) bool {
	if state == "" {
		return false
	}
	h.oauthStatesMu.Lock()
	defer h.oauthStatesMu.Unlock()

	expiry, ok := h.oauthStates[state]
	if !ok {
		return false
	}
	delete(h.oauthStates, state)
	return time.Now().Before(expiry)
}

// HandleCallback processes the OAuth redirect from Slack.
func (h *Handler) HandleCallback(w http.ResponseWriter, r *http.Request) {
	// Validate CSRF state token
	state := r.URL.Query().Get("state")
	if !h.validateOAuthState(state) {
		h.logger.Warn("OAuth callback with invalid state token")
		http.Error(w, "Invalid or expired authorization request. Please try again.", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		errMsg := r.URL.Query().Get("error")
		h.logger.Warn("OAuth callback missing code", "error", errMsg)
		http.Error(w, "Authorization failed: "+errMsg, http.StatusBadRequest)
		return
	}

	// Exchange code for token
	resp, err := slack.GetOAuthV2Response(
		http.DefaultClient,
		h.clientID,
		h.clientSecret,
		code,
		h.baseURL+"/slack/oauth/callback",
	)
	if err != nil {
		h.logger.Error("OAuth token exchange failed", "error", err)
		http.Error(w, "Authorization failed", http.StatusInternalServerError)
		return
	}

	workspaceID := resp.Team.ID
	teamName := resp.Team.Name
	botToken := resp.AccessToken
	installerID := resp.AuthedUser.ID

	h.logger.Info("OAuth callback received",
		"workspace", workspaceID,
		"team", teamName,
		"installer", installerID,
	)

	// Check if workspace already exists
	existing, err := h.store.Get(r.Context(), workspaceID)
	if err == nil && existing != nil {
		// Update the bot token (re-install)
		existing.BotToken = botToken
		existing.TeamName = teamName
		existing.InstalledBy = installerID
		if err := h.store.Update(r.Context(), existing); err != nil {
			h.logger.Error("failed to update workspace", "workspace", workspaceID, "error", err)
			http.Error(w, "Setup failed", http.StatusInternalServerError)
			return
		}
	} else {
		// Create new workspace
		ws := &store.Workspace{
			WorkspaceID:      workspaceID,
			TeamName:         teamName,
			BotToken:         botToken,
			DailyQueryLimit:  100,
			MaxTimeRange:     24 * time.Hour,
			MaxResults:       500,
			InstalledBy:      installerID,
			Status:           store.StatusPendingSetup,
		}
		if err := h.store.Create(r.Context(), ws); err != nil {
			h.logger.Error("failed to create workspace", "workspace", workspaceID, "error", err)
			http.Error(w, "Setup failed", http.StatusInternalServerError)
			return
		}
	}

	// Start the workspace bot via the manager, then trigger setup wizard
	if h.mgr != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if err := h.mgr.AddWorkspace(ctx, workspaceID); err != nil {
				h.logger.Error("failed to start workspace bot after OAuth", "workspace", workspaceID, "error", err)
				return
			}

			// Poll for the bot to be ready instead of sleeping a fixed duration
			api := h.waitForWorkspaceAPI(ctx, workspaceID)
			if api != nil {
				if err := h.wizard.StartSetup(api, workspaceID, installerID); err != nil {
					h.logger.Error("failed to start setup wizard", "workspace", workspaceID, "error", err)
				}
			} else {
				h.logger.Error("workspace bot not ready after timeout", "workspace", workspaceID)
			}
		}()
	}

	// Redirect to a success page
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>LokiLens Installed</title></head>
<body style="font-family: sans-serif; text-align: center; padding: 50px;">
<h1>LokiLens installed!</h1>
<p>Check your Slack DMs — I've sent you a setup guide.</p>
<p>You can close this tab.</p>
</body></html>`)
}

// HandleInteraction processes Slack interaction payloads (modal submissions, button clicks).
func (h *Handler) HandleInteraction(w http.ResponseWriter, r *http.Request) {
	// Read body once with size limit, then verify signature and parse payload from the same bytes.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if !h.verifySignature(r.Header, body) {
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	// Parse payload from the body we already have
	vals, _ := url.ParseQuery(string(body))
	payloadStr := vals.Get("payload")
	if payloadStr == "" {
		http.Error(w, "Missing payload", http.StatusBadRequest)
		return
	}

	var payload slack.InteractionCallback
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		h.logger.Error("failed to parse interaction payload", "error", err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	api := h.getAPIForPayload(payload)
	if api == nil {
		h.logger.Error("no API client for workspace", "team", payload.Team.ID)
		http.Error(w, "Workspace not found", http.StatusNotFound)
		return
	}

	switch payload.Type {
	case slack.InteractionTypeBlockActions:
		h.handleBlockAction(w, api, payload)
	case slack.InteractionTypeViewSubmission:
		h.handleViewSubmission(w, r, api, payload)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (h *Handler) handleBlockAction(w http.ResponseWriter, api *slack.Client, payload slack.InteractionCallback) {
	teamID := payload.Team.ID

	for _, action := range payload.ActionCallback.BlockActions {
		workspaceID := action.Value

		// Validate that the workspace ID in the action matches the team sending the event.
		// Prevents cross-workspace action tampering.
		if workspaceID != teamID {
			h.logger.Warn("workspace ID mismatch in block action",
				"action_workspace", workspaceID, "team", teamID)
			continue
		}

		switch action.ActionID {
		case "setup_loki_url":
			if err := h.wizard.OpenLokiConfigModal(api, payload.TriggerID, workspaceID); err != nil {
				h.logger.Error("failed to open loki modal", "error", err)
			}
		case "setup_gemini_key":
			if err := h.wizard.OpenGeminiConfigModal(api, payload.TriggerID, workspaceID); err != nil {
				h.logger.Error("failed to open gemini modal", "error", err)
			}
		case "setup_free_tier":
			if err := h.wizard.HandleFreeTier(context.Background(), api, workspaceID, payload.User.ID); err != nil {
				h.logger.Error("failed to handle free tier", "error", err)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleViewSubmission(w http.ResponseWriter, r *http.Request, api *slack.Client, payload slack.InteractionCallback) {
	workspaceID := payload.View.PrivateMetadata
	userID := payload.User.ID

	// Validate that the workspace ID in the modal metadata matches the team
	// sending the event. Matches the cross-workspace check in handleBlockAction.
	if workspaceID != "" && workspaceID != payload.Team.ID {
		h.logger.Warn("workspace ID mismatch in view submission",
			"metadata_workspace", workspaceID, "team", payload.Team.ID)
		http.Error(w, "Workspace mismatch", http.StatusBadRequest)
		return
	}

	switch payload.View.CallbackID {
	case "callback_loki_config":
		errs, err := h.wizard.HandleLokiConfigSubmission(r.Context(), api, workspaceID, userID, payload.View.State.Values)
		if err != nil {
			h.logger.Error("loki config submission failed", "error", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if errs != nil {
			// Return validation errors to the modal
			resp := map[string]interface{}{
				"response_action": "errors",
				"errors":          errs,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

	case "callback_gemini_config":
		if err := h.wizard.HandleGeminiConfigSubmission(r.Context(), api, workspaceID, userID, payload.View.State.Values); err != nil {
			h.logger.Error("gemini config submission failed", "error", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// HandleSlashCommand processes the /lokilens-setup slash command.
func (h *Handler) HandleSlashCommand(w http.ResponseWriter, r *http.Request) {
	// Read body once with size limit, then verify signature.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if !h.verifySignature(r.Header, body) {
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	// Restore body so SlashCommandParse can read it.
	r.Body = io.NopCloser(strings.NewReader(string(body)))

	cmd, err := slack.SlashCommandParse(r)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if cmd.Command != "/lokilens-setup" {
		http.Error(w, "Unknown command", http.StatusBadRequest)
		return
	}

	workspaceID := cmd.TeamID
	userID := cmd.UserID

	// Authorization check: only the original installer or workspace admins should configure.
	ws, wsErr := h.store.Get(r.Context(), workspaceID)
	if wsErr == nil && ws.InstalledBy != "" && ws.InstalledBy != userID {
		w.Write([]byte("Only the workspace administrator who installed LokiLens can run setup. Contact <@" + ws.InstalledBy + ">."))
		return
	}

	api := h.getAPIForTeam(workspaceID)
	if api == nil {
		w.Write([]byte("LokiLens is not installed in this workspace. Please install it first."))
		return
	}

	// Trigger setup wizard
	go func() {
		if err := h.wizard.StartSetup(api, workspaceID, userID); err != nil {
			h.logger.Error("failed to start setup from slash command", "workspace", workspaceID, "error", err)
		}
	}()

	w.Write([]byte("Check your DMs — I've sent you a setup guide!"))
}

// waitForWorkspaceAPI polls until the workspace bot is available or context expires.
func (h *Handler) waitForWorkspaceAPI(ctx context.Context, workspaceID string) *slack.Client {
	if h.SlackAPIForWorkspace == nil {
		return nil
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if api := h.SlackAPIForWorkspace(workspaceID); api != nil {
			return api
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (h *Handler) getAPIForPayload(payload slack.InteractionCallback) *slack.Client {
	return h.getAPIForTeam(payload.Team.ID)
}

func (h *Handler) getAPIForTeam(teamID string) *slack.Client {
	if h.SlackAPIForWorkspace != nil {
		return h.SlackAPIForWorkspace(teamID)
	}
	return nil
}

// verifySignature validates Slack's request signature against pre-read body bytes.
// This avoids the double-read problem where body is consumed before parsing.
func (h *Handler) verifySignature(header http.Header, body []byte) bool {
	timestamp := header.Get("X-Slack-Request-Timestamp")
	signature := header.Get("X-Slack-Signature")

	if timestamp == "" || signature == "" {
		return false
	}

	// Check timestamp is within 5 minutes to prevent replay attacks
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if abs(time.Now().Unix()-ts) > 300 {
		return false
	}

	// Compute HMAC-SHA256
	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(h.signingSecret))
	mac.Write([]byte(baseString))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

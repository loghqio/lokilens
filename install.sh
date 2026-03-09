#!/usr/bin/env bash
set -euo pipefail

# LokiLens Installer
# Usage: curl -sSL https://get.lokilens.dev | bash
# Or:    bash install.sh

LOKILENS_IMAGE="${LOKILENS_IMAGE:-ghcr.io/lokilens/lokilens:latest}"
LOKILENS_DIR="${LOKILENS_DIR:-/opt/lokilens}"
COMPOSE_FILE="$LOKILENS_DIR/docker-compose.yml"
ENV_FILE="$LOKILENS_DIR/.env"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[0;33m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${CYAN}[lokilens]${NC} $1"; }
ok()    { echo -e "${GREEN}[lokilens]${NC} $1"; }
warn()  { echo -e "${YELLOW}[lokilens]${NC} $1"; }
err()   { echo -e "${RED}[lokilens]${NC} $1" >&2; }

# ─── Preflight ────────────────────────────────────────────────────────
check_docker() {
    if ! command -v docker &>/dev/null; then
        err "Docker is not installed. Install it first: https://docs.docker.com/get-docker/"
        exit 1
    fi
    if ! docker info &>/dev/null; then
        err "Docker daemon is not running. Start it and try again."
        exit 1
    fi
    if ! docker compose version &>/dev/null 2>&1; then
        err "Docker Compose (v2) is required. Update Docker or install the compose plugin."
        exit 1
    fi
    ok "Docker is ready"
}

# ─── Helpers ──────────────────────────────────────────────────────────
ask() {
    local prompt="$1" default="${2:-}" var="$3"
    if [[ -n "$default" ]]; then
        read -rp "$(echo -e "${BOLD}$prompt${NC} [$default]: ")" input
        eval "$var='${input:-$default}'"
    else
        while true; do
            read -rp "$(echo -e "${BOLD}$prompt${NC}: ")" input
            if [[ -n "$input" ]]; then
                eval "$var='$input'"
                return
            fi
            err "This field is required."
        done
    fi
}

ask_secret() {
    local prompt="$1" var="$2" required="${3:-true}"
    while true; do
        read -srp "$(echo -e "${BOLD}$prompt${NC}: ")" input
        echo
        if [[ -n "$input" ]]; then
            eval "$var='$input'"
            return
        fi
        if [[ "$required" == "false" ]]; then
            eval "$var=''"
            return
        fi
        err "This field is required."
    done
}

open_url() {
    local url="$1"
    if command -v xdg-open &>/dev/null; then
        xdg-open "$url" 2>/dev/null || true
    elif command -v open &>/dev/null; then
        open "$url" 2>/dev/null || true
    else
        return 1
    fi
}

wait_for_enter() {
    read -rp "$(echo -e "${BOLD}Press Enter when done...${NC}")" _
}

# ─── Slack manifest for one-click app creation ────────────────────────
SLACK_MANIFEST_JSON='{"display_information":{"name":"LokiLens","description":"Natural language log analysis powered by Grafana Loki","background_color":"#2C2D30"},"features":{"app_home":{"home_tab_enabled":false,"messages_tab_enabled":true,"messages_tab_read_only_enabled":false},"bot_user":{"display_name":"LokiLens","always_online":true}},"oauth_config":{"scopes":{"bot":["app_mentions:read","channels:history","channels:read","chat:write","groups:history","groups:read","im:history","im:read","im:write","mpim:history","mpim:read"]}},"settings":{"event_subscriptions":{"bot_events":["app_mention","message.channels","message.groups","message.im","message.mpim"]},"org_deploy_enabled":false,"socket_mode_enabled":true,"token_rotation_enabled":false}}'

url_encode() {
    python3 -c "import urllib.parse, sys; print(urllib.parse.quote(sys.stdin.read(), safe=''))" <<< "$1" 2>/dev/null \
        || echo "$1"
}

# ─── Main ─────────────────────────────────────────────────────────────
main() {
    echo
    echo -e "${BOLD}╔══════════════════════════════════════╗${NC}"
    echo -e "${BOLD}║        LokiLens Installer            ║${NC}"
    echo -e "${BOLD}║  Natural language log analysis        ║${NC}"
    echo -e "${BOLD}╚══════════════════════════════════════╝${NC}"
    echo

    check_docker

    # ── Step 1: Create Slack app ──────────────────────────────────────
    echo
    echo -e "${BOLD}Step 1 of 4: Create Slack App${NC}"
    echo
    info "Opening Slack to create your LokiLens app..."
    info "The app is pre-configured — just pick your workspace and click ${BOLD}Create${NC}."
    echo

    ENCODED_MANIFEST=$(url_encode "$SLACK_MANIFEST_JSON")
    SLACK_CREATE_URL="https://api.slack.com/apps?new_app=1&manifest_json=${ENCODED_MANIFEST}"

    if open_url "$SLACK_CREATE_URL"; then
        info "Opened in your browser."
    else
        info "Open this URL in your browser:"
        echo -e "  ${CYAN}${SLACK_CREATE_URL}${NC}"
    fi

    echo
    info "After creating the app:"
    info "  1. Go to ${BOLD}OAuth & Permissions${NC} → click ${BOLD}Install to Workspace${NC}"
    info "  2. Copy the ${BOLD}Bot User OAuth Token${NC} (starts with xoxb-)"
    info "  3. Go to ${BOLD}Basic Information${NC} → ${BOLD}App-Level Tokens${NC} → ${BOLD}Generate Token${NC}"
    info "     Name it anything, add scope ${BOLD}connections:write${NC}, copy it (starts with xapp-)"
    echo
    wait_for_enter

    # ── Step 2: Slack tokens ──────────────────────────────────────────
    echo
    echo -e "${BOLD}Step 2 of 4: Slack Tokens${NC}"
    echo
    ask_secret "Paste Bot Token (xoxb-...)" SLACK_BOT_TOKEN
    if [[ ! "$SLACK_BOT_TOKEN" =~ ^xoxb- ]]; then
        err "Bot token must start with xoxb-"
        exit 1
    fi
    ok "Bot token saved"

    ask_secret "Paste App Token (xapp-...)" SLACK_APP_TOKEN
    if [[ ! "$SLACK_APP_TOKEN" =~ ^xapp- ]]; then
        err "App token must start with xapp-"
        exit 1
    fi
    ok "App token saved"

    # ── Step 3: Loki ──────────────────────────────────────────────────
    echo
    echo -e "${BOLD}Step 3 of 4: Grafana Loki${NC}"
    echo
    info "Enter the URL of your Loki instance."
    info "Examples: http://loki:3100, https://logs-prod.grafana.net"
    echo
    ask "Loki URL" "http://localhost:3100" LOKI_BASE_URL
    ask_secret "Loki API Key (press Enter if none)" LOKI_API_KEY false

    # Validate Loki connection
    info "Testing Loki connection..."
    LOKI_LABELS_URL="${LOKI_BASE_URL%/}/loki/api/v1/labels"
    CURL_ARGS=(-s -o /dev/null -w "%{http_code}" --connect-timeout 5)
    if [[ -n "$LOKI_API_KEY" ]]; then
        CURL_ARGS+=(-H "Authorization: Bearer $LOKI_API_KEY")
    fi
    HTTP_CODE=$(curl "${CURL_ARGS[@]}" "$LOKI_LABELS_URL" 2>/dev/null || echo "000")
    if [[ "$HTTP_CODE" == "200" ]]; then
        ok "Loki is reachable"
    elif [[ "$HTTP_CODE" == "000" ]]; then
        warn "Could not reach Loki at $LOKI_BASE_URL (connection refused)"
        warn "LokiLens will start but queries will fail until Loki is available."
    else
        warn "Loki returned HTTP $HTTP_CODE — may need different auth. Continuing."
    fi

    # ── Step 4: Gemini ────────────────────────────────────────────────
    echo
    echo -e "${BOLD}Step 4 of 4: Gemini AI${NC}"
    echo
    info "LokiLens uses Google Gemini to understand your questions."
    info "Get a free API key (takes 30 seconds):"

    GEMINI_URL="https://aistudio.google.com/apikey"
    if open_url "$GEMINI_URL"; then
        info "Opened ${GEMINI_URL} in your browser."
    else
        echo -e "  ${CYAN}${GEMINI_URL}${NC}"
    fi
    echo
    ask_secret "Paste Gemini API Key" GEMINI_API_KEY
    ok "Gemini key saved"

    # ── Write config ──────────────────────────────────────────────────
    echo
    info "Creating $LOKILENS_DIR ..."
    sudo mkdir -p "$LOKILENS_DIR"
    sudo chown "$(whoami)" "$LOKILENS_DIR"

    cat > "$ENV_FILE" <<EOF
# LokiLens configuration — generated by install.sh
SLACK_BOT_TOKEN=$SLACK_BOT_TOKEN
SLACK_APP_TOKEN=$SLACK_APP_TOKEN
GEMINI_API_KEY=$GEMINI_API_KEY
GEMINI_MODEL=gemini-2.5-flash
LOKI_BASE_URL=$LOKI_BASE_URL
LOKI_API_KEY=$LOKI_API_KEY
HEALTH_ADDR=:8080
LOG_LEVEL=info
MAX_TIME_RANGE=24h
MAX_RESULTS=500
RATE_LIMIT_PER_USER=20
RATE_LIMIT_BURST=5
EOF
    chmod 600 "$ENV_FILE"
    ok "Configuration saved to $ENV_FILE"

    # ── Docker Compose ────────────────────────────────────────────────
    cat > "$COMPOSE_FILE" <<EOF
services:
  lokilens:
    image: $LOKILENS_IMAGE
    restart: unless-stopped
    env_file: .env
    ports:
      - "8080:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
EOF
    ok "Docker Compose file saved"

    # ── Start ─────────────────────────────────────────────────────────
    echo
    info "Starting LokiLens..."
    cd "$LOKILENS_DIR"
    docker compose pull
    docker compose up -d

    echo
    echo -e "${GREEN}${BOLD}╔══════════════════════════════════════╗${NC}"
    echo -e "${GREEN}${BOLD}║      LokiLens is running!            ║${NC}"
    echo -e "${GREEN}${BOLD}╚══════════════════════════════════════╝${NC}"
    echo
    info "Go to Slack and try:"
    info "  • DM @LokiLens: \"Show me errors from the last hour\""
    info "  • In a channel: \"@LokiLens what services are logging?\""
    echo
    info "Useful commands:"
    info "  Logs:    cd $LOKILENS_DIR && docker compose logs -f"
    info "  Stop:    cd $LOKILENS_DIR && docker compose down"
    info "  Update:  cd $LOKILENS_DIR && docker compose pull && docker compose up -d"
    info "  Config:  $ENV_FILE"
    echo
}

main "$@"

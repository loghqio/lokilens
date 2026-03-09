-- LokiLens multi-tenant schema

CREATE TYPE workspace_status AS ENUM ('pending_setup', 'active', 'suspended');

CREATE TABLE workspaces (
    workspace_id       TEXT PRIMARY KEY,                -- Slack team ID (T...)
    team_name          TEXT NOT NULL DEFAULT '',

    -- Slack bot token (encrypted at application layer)
    bot_token_enc      BYTEA NOT NULL,

    -- Loki connection
    loki_url           TEXT NOT NULL DEFAULT '',
    loki_api_key_enc   BYTEA,                           -- nullable = no auth required

    -- Gemini config (nullable = use shared key / free tier)
    gemini_api_key_enc BYTEA,

    -- Per-workspace limits
    daily_query_limit    INT NOT NULL DEFAULT 100,
    rate_limit_per_user  INT NOT NULL DEFAULT 20,
    rate_limit_burst     INT NOT NULL DEFAULT 5,
    max_time_range       TEXT NOT NULL DEFAULT '24h',     -- parsed as Go duration
    max_results          INT NOT NULL DEFAULT 500,

    -- Installer
    installed_by       TEXT NOT NULL DEFAULT '',          -- Slack user ID

    -- Lifecycle
    status             workspace_status NOT NULL DEFAULT 'pending_setup',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE daily_usage (
    workspace_id  TEXT NOT NULL REFERENCES workspaces(workspace_id) ON DELETE CASCADE,
    usage_date    DATE NOT NULL DEFAULT CURRENT_DATE,
    query_count   INT NOT NULL DEFAULT 0,
    PRIMARY KEY (workspace_id, usage_date)
);

CREATE INDEX idx_daily_usage_date ON daily_usage (usage_date);
CREATE INDEX idx_workspaces_status ON workspaces (status);

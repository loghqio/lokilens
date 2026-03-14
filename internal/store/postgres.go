package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

// PostgresStore implements WorkspaceStore using PostgreSQL.
type PostgresStore struct {
	db     *sql.DB
	cipher *Cipher
}

// NewPostgresStore opens a PostgreSQL connection and returns a store.
func NewPostgresStore(databaseURL string, cipher *Cipher) (*PostgresStore, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(10 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return &PostgresStore{db: db, cipher: cipher}, nil
}

// Migrate runs the schema migration idempotently.
func (s *PostgresStore) Migrate(ctx context.Context) error {
	const ddl = `
	DO $$ BEGIN
		CREATE TYPE workspace_status AS ENUM ('pending_setup', 'active', 'suspended');
	EXCEPTION
		WHEN duplicate_object THEN NULL;
	END $$;

	CREATE TABLE IF NOT EXISTS workspaces (
		workspace_id       TEXT PRIMARY KEY,
		team_name          TEXT NOT NULL DEFAULT '',
		bot_token_enc      BYTEA NOT NULL,
		log_backend        TEXT NOT NULL DEFAULT 'loki',
		loki_url           TEXT NOT NULL DEFAULT '',
		loki_api_key_enc   BYTEA,
		aws_region         TEXT NOT NULL DEFAULT '',
		cw_log_groups      TEXT NOT NULL DEFAULT '',
		gemini_api_key_enc BYTEA,
		daily_query_limit    INT NOT NULL DEFAULT 100,
		max_time_range       TEXT NOT NULL DEFAULT '24h',
		max_results          INT NOT NULL DEFAULT 500,
		installed_by       TEXT NOT NULL DEFAULT '',
		status             workspace_status NOT NULL DEFAULT 'pending_setup',
		created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
	);

	CREATE TABLE IF NOT EXISTS daily_usage (
		workspace_id  TEXT NOT NULL REFERENCES workspaces(workspace_id) ON DELETE CASCADE,
		usage_date    DATE NOT NULL DEFAULT CURRENT_DATE,
		query_count   INT NOT NULL DEFAULT 0,
		PRIMARY KEY (workspace_id, usage_date)
	);

	CREATE INDEX IF NOT EXISTS idx_daily_usage_date ON daily_usage (usage_date);
	CREATE INDEX IF NOT EXISTS idx_workspaces_status ON workspaces (status);
	`

	_, err := s.db.ExecContext(ctx, ddl)
	if err != nil {
		return fmt.Errorf("running migration: %w", err)
	}
	return nil
}

func (s *PostgresStore) Get(ctx context.Context, workspaceID string) (*Workspace, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT workspace_id, team_name, bot_token_enc, log_backend,
		       loki_url, loki_api_key_enc, aws_region, cw_log_groups,
		       gemini_api_key_enc, daily_query_limit,
		       max_time_range, max_results, installed_by, status, created_at, updated_at
		FROM workspaces WHERE workspace_id = $1
	`, workspaceID)

	return s.scanWorkspace(row)
}

func (s *PostgresStore) List(ctx context.Context, status WorkspaceStatus) ([]*Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT workspace_id, team_name, bot_token_enc, log_backend,
		       loki_url, loki_api_key_enc, aws_region, cw_log_groups,
		       gemini_api_key_enc, daily_query_limit,
		       max_time_range, max_results, installed_by, status, created_at, updated_at
		FROM workspaces WHERE status = $1
		ORDER BY created_at
	`, status)
	if err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}
	defer rows.Close()

	var workspaces []*Workspace
	for rows.Next() {
		w, err := s.scanWorkspaceRows(rows)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}

func (s *PostgresStore) Create(ctx context.Context, w *Workspace) error {
	botTokenEnc, err := s.cipher.Encrypt(w.BotToken)
	if err != nil {
		return fmt.Errorf("encrypting bot token: %w", err)
	}

	lokiKeyEnc, err := s.cipher.EncryptOptional(w.LokiAPIKey)
	if err != nil {
		return fmt.Errorf("encrypting loki key: %w", err)
	}

	geminiKeyEnc, err := s.cipher.EncryptOptional(w.GeminiAPIKey)
	if err != nil {
		return fmt.Errorf("encrypting gemini key: %w", err)
	}

	logBackend := w.LogBackend
	if logBackend == "" {
		logBackend = "loki"
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workspaces (
			workspace_id, team_name, bot_token_enc, log_backend,
			loki_url, loki_api_key_enc, aws_region, cw_log_groups,
			gemini_api_key_enc, daily_query_limit,
			max_time_range, max_results, installed_by, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`,
		w.WorkspaceID, w.TeamName, botTokenEnc, logBackend,
		w.LokiURL, lokiKeyEnc, w.AWSRegion, w.CWLogGroups,
		geminiKeyEnc, w.DailyQueryLimit,
		w.MaxTimeRange.String(), w.MaxResults, w.InstalledBy, w.Status,
	)
	if err != nil {
		return fmt.Errorf("creating workspace: %w", err)
	}
	return nil
}

func (s *PostgresStore) Update(ctx context.Context, w *Workspace) error {
	botTokenEnc, err := s.cipher.Encrypt(w.BotToken)
	if err != nil {
		return fmt.Errorf("encrypting bot token: %w", err)
	}

	lokiKeyEnc, err := s.cipher.EncryptOptional(w.LokiAPIKey)
	if err != nil {
		return fmt.Errorf("encrypting loki key: %w", err)
	}

	geminiKeyEnc, err := s.cipher.EncryptOptional(w.GeminiAPIKey)
	if err != nil {
		return fmt.Errorf("encrypting gemini key: %w", err)
	}

	logBackend := w.LogBackend
	if logBackend == "" {
		logBackend = "loki"
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE workspaces SET
			team_name = $2, bot_token_enc = $3, log_backend = $4,
			loki_url = $5, loki_api_key_enc = $6, aws_region = $7, cw_log_groups = $8,
			gemini_api_key_enc = $9, daily_query_limit = $10,
			max_time_range = $11, max_results = $12,
			installed_by = $13, status = $14, updated_at = now()
		WHERE workspace_id = $1
	`,
		w.WorkspaceID, w.TeamName, botTokenEnc, logBackend,
		w.LokiURL, lokiKeyEnc, w.AWSRegion, w.CWLogGroups,
		geminiKeyEnc, w.DailyQueryLimit,
		w.MaxTimeRange.String(), w.MaxResults, w.InstalledBy, w.Status,
	)
	if err != nil {
		return fmt.Errorf("updating workspace: %w", err)
	}
	return nil
}

func (s *PostgresStore) Delete(ctx context.Context, workspaceID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return fmt.Errorf("deleting workspace: %w", err)
	}
	return nil
}

func (s *PostgresStore) IncrementUsage(ctx context.Context, workspaceID string) (int, int, error) {
	var count, limit int
	// Conditional increment: only increments if the count is still under the limit.
	// This prevents rejected queries from consuming usage slots.
	err := s.db.QueryRowContext(ctx, `
		WITH current AS (
			INSERT INTO daily_usage (workspace_id, usage_date, query_count)
			VALUES ($1, CURRENT_DATE, 0)
			ON CONFLICT (workspace_id, usage_date) DO NOTHING
			RETURNING query_count
		),
		maybe_inc AS (
			UPDATE daily_usage du
			SET query_count = du.query_count + 1
			FROM workspaces w
			WHERE du.workspace_id = $1
			  AND du.usage_date = CURRENT_DATE
			  AND w.workspace_id = $1
			  AND du.query_count < w.daily_query_limit
			RETURNING du.query_count
		)
		SELECT
			COALESCE(
				(SELECT query_count FROM maybe_inc),
				(SELECT du.query_count FROM daily_usage du WHERE du.workspace_id = $1 AND du.usage_date = CURRENT_DATE)
			),
			w.daily_query_limit
		FROM workspaces w
		WHERE w.workspace_id = $1
	`, workspaceID).Scan(&count, &limit)
	if err != nil {
		return 0, 0, fmt.Errorf("incrementing usage: %w", err)
	}
	return count, limit, nil
}

func (s *PostgresStore) DeleteOldUsage(ctx context.Context, daysToKeep int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM daily_usage WHERE usage_date < CURRENT_DATE - $1::int
	`, daysToKeep)
	if err != nil {
		return fmt.Errorf("deleting old usage: %w", err)
	}
	return nil
}

func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// DB exposes the underlying *sql.DB for health checks.
func (s *PostgresStore) DB() *sql.DB {
	return s.db
}

// scanWorkspace scans a single row into a Workspace, decrypting secrets.
func (s *PostgresStore) scanWorkspace(row *sql.Row) (*Workspace, error) {
	var (
		w              Workspace
		botTokenEnc    []byte
		lokiKeyEnc     []byte
		geminiKeyEnc   []byte
		maxTimeRangeS  string
		status         string
	)

	err := row.Scan(
		&w.WorkspaceID, &w.TeamName, &botTokenEnc, &w.LogBackend,
		&w.LokiURL, &lokiKeyEnc, &w.AWSRegion, &w.CWLogGroups,
		&geminiKeyEnc, &w.DailyQueryLimit,
		&maxTimeRangeS, &w.MaxResults, &w.InstalledBy, &status, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scanning workspace: %w", err)
	}

	w.Status = WorkspaceStatus(status)

	w.BotToken, err = s.cipher.Decrypt(botTokenEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypting bot token: %w", err)
	}

	w.LokiAPIKey, err = s.cipher.DecryptOptional(lokiKeyEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypting loki key: %w", err)
	}

	w.GeminiAPIKey, err = s.cipher.DecryptOptional(geminiKeyEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypting gemini key: %w", err)
	}

	w.MaxTimeRange, err = time.ParseDuration(maxTimeRangeS)
	if err != nil {
		w.MaxTimeRange = 24 * time.Hour
	}

	return &w, nil
}

// scanWorkspaceRows scans a rows iterator into a Workspace.
func (s *PostgresStore) scanWorkspaceRows(rows *sql.Rows) (*Workspace, error) {
	var (
		w              Workspace
		botTokenEnc    []byte
		lokiKeyEnc     []byte
		geminiKeyEnc   []byte
		maxTimeRangeS  string
		status         string
	)

	err := rows.Scan(
		&w.WorkspaceID, &w.TeamName, &botTokenEnc, &w.LogBackend,
		&w.LokiURL, &lokiKeyEnc, &w.AWSRegion, &w.CWLogGroups,
		&geminiKeyEnc, &w.DailyQueryLimit,
		&maxTimeRangeS, &w.MaxResults, &w.InstalledBy, &status, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scanning workspace row: %w", err)
	}

	w.Status = WorkspaceStatus(status)

	w.BotToken, err = s.cipher.Decrypt(botTokenEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypting bot token: %w", err)
	}

	w.LokiAPIKey, err = s.cipher.DecryptOptional(lokiKeyEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypting loki key: %w", err)
	}

	w.GeminiAPIKey, err = s.cipher.DecryptOptional(geminiKeyEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypting gemini key: %w", err)
	}

	w.MaxTimeRange, err = time.ParseDuration(maxTimeRangeS)
	if err != nil {
		w.MaxTimeRange = 24 * time.Hour
	}

	return &w, nil
}

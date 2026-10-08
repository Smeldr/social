package social

import (
	"context"
	"fmt"

	"smeldr.dev/core"
)

// CreateTables creates all smeldr_social_* database tables and indexes.
// It is safe to call multiple times — all statements use CREATE ... IF NOT EXISTS.
// Call this once at application startup before any other social operations.
//
// It also applies idempotent migrations that rename legacy forge_social_* tables
// and add columns introduced in earlier minor versions.
func CreateTables(db smeldr.DB) error {
	if err := migrateLegacyTableNames(context.Background(), db); err != nil {
		return fmt.Errorf("social: migrate tables: %w", err)
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS smeldr_social_credentials (
			id            TEXT PRIMARY KEY,
			platform      TEXT NOT NULL,
			name          TEXT NOT NULL,
			instance_url  TEXT NOT NULL,
			actor_id      TEXT NOT NULL DEFAULT '',
			access_token  TEXT NOT NULL,
			refresh_token TEXT NOT NULL DEFAULT '',
			expires_at    TIMESTAMP,
			created_at    TIMESTAMP NOT NULL,
			updated_at    TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS smeldr_social_posts (
			id               TEXT PRIMARY KEY,
			platform         TEXT NOT NULL DEFAULT 'mastodon',
			credential_id    TEXT NOT NULL REFERENCES smeldr_social_credentials(id),
			body             TEXT NOT NULL,
			media_url        TEXT NOT NULL DEFAULT '',
			alt_text         TEXT NOT NULL DEFAULT '',
			scheduled_at     TIMESTAMP,
			status           TEXT NOT NULL DEFAULT 'draft',
			platform_post_id TEXT NOT NULL DEFAULT '',
			error_msg        TEXT NOT NULL DEFAULT '',
			created_at       TIMESTAMP NOT NULL,
			updated_at       TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS smeldr_social_oauth_states (
			state      TEXT PRIMARY KEY,
			platform   TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS smeldr_social_delivery_log (
			id           TEXT PRIMARY KEY,
			post_id      TEXT NOT NULL REFERENCES smeldr_social_posts(id),
			attempt      INTEGER NOT NULL,
			status_code  INTEGER NOT NULL DEFAULT 0,
			error        TEXT NOT NULL DEFAULT '',
			attempted_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_smeldr_social_posts_status_scheduled
			ON smeldr_social_posts(status, scheduled_at)`,
		`CREATE TABLE IF NOT EXISTS smeldr_social_route_jobs (
			id           TEXT PRIMARY KEY,
			signal       TEXT NOT NULL,
			content_type TEXT NOT NULL,
			agent_url    TEXT NOT NULL,
			payload      TEXT NOT NULL,
			status       TEXT NOT NULL DEFAULT 'pending',
			attempts     INTEGER NOT NULL DEFAULT 0,
			next_attempt TIMESTAMP,
			last_error   TEXT NOT NULL DEFAULT '',
			created_at   TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_smeldr_social_route_jobs_status
			ON smeldr_social_route_jobs(status, next_attempt)`,
		`CREATE TABLE IF NOT EXISTS smeldr_social_route_log (
			id           TEXT PRIMARY KEY,
			job_id       TEXT NOT NULL REFERENCES smeldr_social_route_jobs(id),
			attempt      INTEGER NOT NULL,
			status_code  INTEGER NOT NULL DEFAULT 0,
			error        TEXT NOT NULL DEFAULT '',
			attempted_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS smeldr_social_publication_schedules (
			id            TEXT PRIMARY KEY,
			credential_id TEXT NOT NULL UNIQUE,
			slots         TEXT NOT NULL DEFAULT '[]',
			status        TEXT NOT NULL DEFAULT 'active',
			last_tick_at  TIMESTAMP,
			created_at    TIMESTAMP NOT NULL,
			updated_at    TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_smeldr_social_pub_schedules_status
			ON smeldr_social_publication_schedules(status)`,
		`CREATE TABLE IF NOT EXISTS smeldr_social_platform_config (
			platform   TEXT PRIMARY KEY,
			config     TEXT NOT NULL,
			updated_at TIMESTAMP NOT NULL
		)`,
	}

	ctx := context.Background()
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("social: create tables: %w", err)
		}
	}

	// Columns added after the first releases, for databases that predate them:
	// actor_id (v0.2.0) and code_verifier (v0.5.0, the PKCE verifier of the X
	// OAuth 2.0 flow). EnsureColumn probes first and works on SQLite and Postgres.
	if err := smeldr.EnsureColumn(ctx, db, "smeldr_social_credentials", "actor_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("social: migrate actor_id: %w", err)
	}
	if err := smeldr.EnsureColumn(ctx, db, "smeldr_social_oauth_states", "code_verifier", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("social: migrate code_verifier: %w", err)
	}

	return nil
}

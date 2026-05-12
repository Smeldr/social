package forgesocial

import (
	"context"
	"fmt"
	"strings"

	forge "forge-cms.dev/forge"
)

// CreateTables creates all forge_social_* database tables and indexes.
// It is safe to call multiple times — all statements use CREATE ... IF NOT EXISTS.
// Call this once at application startup before any other forge-social operations.
//
// It also applies an idempotent migration that adds the actor_id column to
// forge_social_credentials for databases created before v0.2.0.
func CreateTables(db forge.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS forge_social_credentials (
			id            TEXT PRIMARY KEY,
			platform      TEXT NOT NULL,
			name          TEXT NOT NULL,
			instance_url  TEXT NOT NULL,
			actor_id      TEXT NOT NULL DEFAULT '',
			access_token  TEXT NOT NULL,
			refresh_token TEXT NOT NULL DEFAULT '',
			expires_at    DATETIME,
			created_at    DATETIME NOT NULL,
			updated_at    DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS forge_social_posts (
			id               TEXT PRIMARY KEY,
			platform         TEXT NOT NULL DEFAULT 'mastodon',
			credential_id    TEXT NOT NULL REFERENCES forge_social_credentials(id),
			body             TEXT NOT NULL,
			media_url        TEXT NOT NULL DEFAULT '',
			alt_text         TEXT NOT NULL DEFAULT '',
			scheduled_at     DATETIME,
			status           TEXT NOT NULL DEFAULT 'draft',
			platform_post_id TEXT NOT NULL DEFAULT '',
			error_msg        TEXT NOT NULL DEFAULT '',
			created_at       DATETIME NOT NULL,
			updated_at       DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS forge_social_oauth_states (
			state      TEXT PRIMARY KEY,
			platform   TEXT NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS forge_social_delivery_log (
			id           TEXT PRIMARY KEY,
			post_id      TEXT NOT NULL REFERENCES forge_social_posts(id),
			attempt      INTEGER NOT NULL,
			status_code  INTEGER NOT NULL DEFAULT 0,
			error        TEXT NOT NULL DEFAULT '',
			attempted_at DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_forge_social_posts_status_scheduled
			ON forge_social_posts(status, scheduled_at)`,
		`CREATE TABLE IF NOT EXISTS forge_social_route_jobs (
			id           TEXT PRIMARY KEY,
			signal       TEXT NOT NULL,
			content_type TEXT NOT NULL,
			agent_url    TEXT NOT NULL,
			payload      TEXT NOT NULL,
			status       TEXT NOT NULL DEFAULT 'pending',
			attempts     INTEGER NOT NULL DEFAULT 0,
			next_attempt DATETIME,
			last_error   TEXT NOT NULL DEFAULT '',
			created_at   DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_forge_social_route_jobs_status
			ON forge_social_route_jobs(status, next_attempt)`,
		`CREATE TABLE IF NOT EXISTS forge_social_route_log (
			id           TEXT PRIMARY KEY,
			job_id       TEXT NOT NULL REFERENCES forge_social_route_jobs(id),
			attempt      INTEGER NOT NULL,
			status_code  INTEGER NOT NULL DEFAULT 0,
			error        TEXT NOT NULL DEFAULT '',
			attempted_at DATETIME NOT NULL
		)`,
	}

	ctx := context.Background()
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("forgesocial: create tables: %w", err)
		}
	}

	// Idempotent migration: add actor_id column for databases created before v0.2.0.
	// On a fresh database the column already exists (declared above); SQLite returns
	// "duplicate column name" which we swallow. On an existing v0.1.0 database the
	// ALTER TABLE adds the column.
	_, err := db.ExecContext(ctx,
		`ALTER TABLE forge_social_credentials ADD COLUMN actor_id TEXT NOT NULL DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("forgesocial: migrate actor_id: %w", err)
	}

	return nil
}

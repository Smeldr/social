package forgesocial

import (
	"context"
	"fmt"

	forge "forge-cms.dev/forge"
)

// CreateTables creates all forge_social_* database tables and indexes.
// It is safe to call multiple times — all statements use CREATE ... IF NOT EXISTS.
// Call this once at application startup before any other forge-social operations.
func CreateTables(db forge.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS forge_social_credentials (
			id            TEXT PRIMARY KEY,
			platform      TEXT NOT NULL,
			name          TEXT NOT NULL,
			instance_url  TEXT NOT NULL,
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
	}

	for _, stmt := range stmts {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			return fmt.Errorf("forgesocial: create tables: %w", err)
		}
	}
	return nil
}

package social

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"smeldr.dev/core"
)

// migrateLegacyTableNames renames any forge_social_* tables that still exist in
// the database to their smeldr_social_* equivalents. It is called from
// [CreateTables] once at startup before any CREATE TABLE statements run.
//
// Only operates on SQLite databases (identified by sqlite_master). For other
// databases it returns nil immediately.
//
// Idempotency: if both the source (forge_social_*) and destination
// (smeldr_social_*) tables already exist the pair is skipped with a warning.
// Re-running on an already-migrated database is safe.
func migrateLegacyTableNames(ctx context.Context, db smeldr.DB) error {
	pairs := [][2]string{
		{"forge_social_credentials", "smeldr_social_credentials"},
		{"forge_social_posts", "smeldr_social_posts"},
		{"forge_social_oauth_states", "smeldr_social_oauth_states"},
		{"forge_social_delivery_log", "smeldr_social_delivery_log"},
		{"forge_social_route_jobs", "smeldr_social_route_jobs"},
		{"forge_social_route_log", "smeldr_social_route_log"},
		{"forge_social_publication_schedules", "smeldr_social_publication_schedules"},
		{"forge_social_platform_config", "smeldr_social_platform_config"},
	}

	var dummy int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master`).Scan(&dummy); err != nil {
		return nil // not SQLite — skip silently
	}

	var toRename [][2]string
	for _, pair := range pairs {
		var srcN int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, pair[0],
		).Scan(&srcN); err != nil || srcN == 0 {
			continue // source doesn't exist — nothing to rename
		}
		var dstN int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, pair[1],
		).Scan(&dstN); err == nil && dstN > 0 {
			slog.Warn("social: legacy table migration skipped — destination already exists",
				"src", pair[0], "dst", pair[1])
			continue
		}
		toRename = append(toRename, pair)
	}
	if len(toRename) == 0 {
		return nil
	}

	type transactor interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	}

	execDB := db
	var commit func() error = func() error { return nil }
	var rollback func() error = func() error { return nil }

	if tr, ok := db.(transactor); ok {
		tx, err := tr.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("social: migrate legacy tables: begin: %w", err)
		}
		execDB = tx
		commit = tx.Commit
		rollback = tx.Rollback
	}

	for _, pair := range toRename {
		slog.Info("social: renaming legacy table", "from", pair[0], "to", pair[1])
		if _, err := execDB.ExecContext(ctx, `ALTER TABLE `+pair[0]+` RENAME TO `+pair[1]); err != nil {
			_ = rollback()
			return fmt.Errorf("social: migrate legacy tables: %s → %s: %w", pair[0], pair[1], err)
		}
	}
	return commit()
}

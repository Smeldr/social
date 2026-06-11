package social

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"smeldr.dev/core"
)

func tableExists(t *testing.T, db smeldr.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&n)
	if err != nil {
		t.Fatalf("tableExists(%q): %v", name, err)
	}
	return n > 0
}

func openSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Skipf("sqlite unavailable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateLegacySocialTables_freshDB(t *testing.T) {
	db := openSQLiteDB(t)
	if err := CreateTables(db); err != nil {
		t.Fatalf("CreateTables: %v", err)
	}
	expected := []string{
		"smeldr_social_credentials",
		"smeldr_social_posts",
		"smeldr_social_oauth_states",
		"smeldr_social_delivery_log",
		"smeldr_social_route_jobs",
		"smeldr_social_route_log",
		"smeldr_social_publication_schedules",
		"smeldr_social_platform_config",
	}
	for _, name := range expected {
		if !tableExists(t, db, name) {
			t.Errorf("expected table %q to exist", name)
		}
	}
	for _, name := range []string{"forge_social_credentials", "forge_social_posts"} {
		if tableExists(t, db, name) {
			t.Errorf("expected legacy table %q to not exist", name)
		}
	}
}

func TestMigrateLegacySocialTables_existingForge(t *testing.T) {
	rawDB := openSQLiteDB(t)
	var db smeldr.DB = rawDB

	// Create the 8 legacy tables.
	_, err := rawDB.Exec(`
		CREATE TABLE forge_social_credentials (id TEXT PRIMARY KEY, platform TEXT, name TEXT, instance_url TEXT, access_token TEXT, refresh_token TEXT NOT NULL DEFAULT '', expires_at DATETIME, created_at DATETIME, updated_at DATETIME);
		CREATE TABLE forge_social_posts (id TEXT PRIMARY KEY, platform TEXT, credential_id TEXT, body TEXT, media_url TEXT NOT NULL DEFAULT '', alt_text TEXT NOT NULL DEFAULT '', scheduled_at DATETIME, status TEXT, platform_post_id TEXT NOT NULL DEFAULT '', error_msg TEXT NOT NULL DEFAULT '', created_at DATETIME, updated_at DATETIME);
		CREATE TABLE forge_social_oauth_states (state TEXT PRIMARY KEY, platform TEXT, created_at DATETIME);
		CREATE TABLE forge_social_delivery_log (id TEXT PRIMARY KEY, post_id TEXT, attempt INTEGER, status_code INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', attempted_at DATETIME);
		CREATE TABLE forge_social_route_jobs (id TEXT PRIMARY KEY, signal TEXT, content_type TEXT, agent_url TEXT, payload TEXT, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt DATETIME, last_error TEXT NOT NULL DEFAULT '', created_at DATETIME);
		CREATE TABLE forge_social_route_log (id TEXT PRIMARY KEY, job_id TEXT, attempt INTEGER, status_code INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', attempted_at DATETIME);
		CREATE TABLE forge_social_publication_schedules (id TEXT PRIMARY KEY, credential_id TEXT, slots TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL DEFAULT 'active', last_tick_at DATETIME, created_at DATETIME, updated_at DATETIME);
		CREATE TABLE forge_social_platform_config (platform TEXT PRIMARY KEY, config TEXT, updated_at DATETIME);
	`)
	if err != nil {
		t.Fatalf("create legacy tables: %v", err)
	}

	if err := CreateTables(db); err != nil {
		t.Fatalf("CreateTables: %v", err)
	}

	for _, name := range []string{
		"smeldr_social_credentials", "smeldr_social_posts", "smeldr_social_oauth_states",
		"smeldr_social_delivery_log", "smeldr_social_route_jobs", "smeldr_social_route_log",
		"smeldr_social_publication_schedules", "smeldr_social_platform_config",
	} {
		if !tableExists(t, db, name) {
			t.Errorf("expected table %q to exist after migration", name)
		}
	}
	for _, name := range []string{"forge_social_credentials", "forge_social_posts"} {
		if tableExists(t, db, name) {
			t.Errorf("expected legacy table %q to be gone after migration", name)
		}
	}
}

func TestMigrateLegacySocialTables_idempotent(t *testing.T) {
	rawDB := openSQLiteDB(t)
	var db smeldr.DB = rawDB

	// Create the 8 legacy tables.
	_, err := rawDB.Exec(`
		CREATE TABLE forge_social_credentials (id TEXT PRIMARY KEY, platform TEXT, name TEXT, instance_url TEXT, access_token TEXT, refresh_token TEXT NOT NULL DEFAULT '', expires_at DATETIME, created_at DATETIME, updated_at DATETIME);
		CREATE TABLE forge_social_posts (id TEXT PRIMARY KEY, platform TEXT, credential_id TEXT, body TEXT, media_url TEXT NOT NULL DEFAULT '', alt_text TEXT NOT NULL DEFAULT '', scheduled_at DATETIME, status TEXT, platform_post_id TEXT NOT NULL DEFAULT '', error_msg TEXT NOT NULL DEFAULT '', created_at DATETIME, updated_at DATETIME);
		CREATE TABLE forge_social_oauth_states (state TEXT PRIMARY KEY, platform TEXT, created_at DATETIME);
		CREATE TABLE forge_social_delivery_log (id TEXT PRIMARY KEY, post_id TEXT, attempt INTEGER, status_code INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', attempted_at DATETIME);
		CREATE TABLE forge_social_route_jobs (id TEXT PRIMARY KEY, signal TEXT, content_type TEXT, agent_url TEXT, payload TEXT, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt DATETIME, last_error TEXT NOT NULL DEFAULT '', created_at DATETIME);
		CREATE TABLE forge_social_route_log (id TEXT PRIMARY KEY, job_id TEXT, attempt INTEGER, status_code INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', attempted_at DATETIME);
		CREATE TABLE forge_social_publication_schedules (id TEXT PRIMARY KEY, credential_id TEXT, slots TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL DEFAULT 'active', last_tick_at DATETIME, created_at DATETIME, updated_at DATETIME);
		CREATE TABLE forge_social_platform_config (platform TEXT PRIMARY KEY, config TEXT, updated_at DATETIME);
	`)
	if err != nil {
		t.Fatalf("create legacy tables: %v", err)
	}

	ctx := context.Background()
	if err := migrateLegacyTableNames(ctx, db); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call: forge_* are gone; smeldr_* exist — must not error.
	if err := migrateLegacyTableNames(ctx, db); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

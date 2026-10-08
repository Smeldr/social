package social

import (
	"context"

	"smeldr.dev/core"
)

// migrateLegacyTableNames renames the legacy forge_social_* tables to their
// smeldr_social_* names. It is called from [CreateTables] once at startup before
// the CREATE TABLE statements run. It works on SQLite and Postgres through
// [smeldr.RenameLegacyTables]: a pair whose source is absent is skipped, and one
// whose source and destination both exist is skipped with a warning, so
// re-running on a migrated database is safe.
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

	return smeldr.RenameLegacyTables(ctx, db, pairs)
}

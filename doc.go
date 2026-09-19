// Package social provides platform publishing for Smeldr applications.
// It supports scheduling and publishing content to Mastodon, LinkedIn, and
// X (Twitter) via OAuth 2.0.
//
// # Quick start
//
//	import "smeldr.dev/social"
//
//	social := social.New(db, social.Config{
//	    Secret: cfg.Secret,
//	    // Platform credentials (Mastodon/LinkedIn/X) are configured at
//	    // runtime via the create_platform_config MCP tool (Admin role) and
//	    // stored AES-256-GCM encrypted in the DB. Config.Mastodon and
//	    // Config.LinkedIn below are a deprecated fallback only, read once
//	    // at startup if no DB-stored config exists yet for that platform.
//	})
//	social.Register(app)
//	defer social.Stop()
//
//	// Wire MCP tools.
//	mcpSrv := mcp.New(app,
//	    mcp.WithModule(social.PostModule()),
//	    mcp.WithModule(social.CredentialModule()),
//	    mcp.WithModule(social.ConfigModule()),   // create_platform_config
//	    mcp.WithModule(social.ScheduleModule()), // slot-queue publication schedule
//	)
//	// Layer 1 — wire agent routing (optional).
//	// Fires on AfterPublish for "Post" content type.
//	social.AddRoutes(app,
//	    social.OnPublish("Post", "https://agent.example.com/social"),
//	)
//
// # Scheduling models
//
// Two models are supported, independently per post: an explicit
// scheduled_at timestamp, or a slot-queue [PublicationSchedule] — recurring
// weekly slots that publish queued posts FIFO as each slot fires.
package social

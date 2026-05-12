// Package forgesocial provides platform publishing for Forge applications.
// It supports scheduling and publishing content to Mastodon and LinkedIn
// via OAuth 2.0.
//
// # Quick start
//
//	import forgesocial "forge-cms.dev/forge-social"
//
//	social := forgesocial.New(db, forgesocial.Config{
//	    Secret: cfg.Secret,
//	    Mastodon: forgesocial.MastodonConfig{
//	        ClientID:     os.Getenv("MASTODON_CLIENT_ID"),
//	        ClientSecret: os.Getenv("MASTODON_CLIENT_SECRET"),
//	        InstanceURL:  os.Getenv("MASTODON_INSTANCE_URL"),
//	        RedirectURL:  cfg.BaseURL + "/oauth/mastodon/callback",
//	    },
//	    LinkedIn: forgesocial.LinkedInConfig{
//	        ClientID:     os.Getenv("LINKEDIN_CLIENT_ID"),
//	        ClientSecret: os.Getenv("LINKEDIN_CLIENT_SECRET"),
//	        RedirectURL:  cfg.BaseURL + "/oauth/linkedin/callback",
//	    },
//	})
//	social.Register(app)
//	defer social.Stop()
//
//	// Wire MCP tools.
//	mcpSrv := forgemcp.New(app,
//	    forgemcp.WithModule(social.PostModule()),
//	    forgemcp.WithModule(social.CredentialModule()),
//	)
package forgesocial

import (
	"fmt"
	"net/http"

	forge "forge-cms.dev/forge"
)

// Config holds the configuration for a Social instance.
type Config struct {
	// Secret is the application's Config.Secret, used to derive the AES-256-GCM
	// key for encrypting stored OAuth tokens. Must match the forge.App's secret.
	Secret []byte
	// Mastodon holds the Mastodon OAuth 2.0 client credentials.
	Mastodon MastodonConfig
	// LinkedIn holds the LinkedIn OAuth 2.0 client credentials.
	// Leave zero-valued to disable LinkedIn publishing.
	LinkedIn LinkedInConfig
}

// Social manages platform publishing for a Forge application.
// Create it with [New] and register its HTTP routes with [Register].
// Call [Stop] in your application's shutdown handler.
type Social struct {
	db       forge.DB
	cfg      Config
	creds    *credentialStore
	mastodon *mastodonClient
	linkedin *linkedinClient
	sched    *scheduler
}

// New creates a Social instance backed by db. It panics if db is nil,
// if Config.Secret is empty, or if the database tables cannot be created.
func New(db forge.DB, cfg Config) *Social {
	if db == nil {
		panic("forgesocial.New: db is nil")
	}
	if len(cfg.Secret) == 0 {
		panic("forgesocial.New: Config.Secret is required")
	}
	if err := CreateTables(db); err != nil {
		panic(fmt.Sprintf("forgesocial.New: create tables: %v", err))
	}
	cs := newCredentialStore(db, cfg.Secret)
	mc := newMastodonClient(cfg.Mastodon)
	s := &Social{
		db:       db,
		cfg:      cfg,
		creds:    cs,
		mastodon: mc,
	}
	if cfg.LinkedIn.ClientID != "" {
		s.linkedin = newLinkedinClient(cfg.LinkedIn)
	}
	s.sched = newScheduler(s)
	return s
}

// Register mounts the forge-social HTTP routes on app and starts the
// internal scheduler goroutine.
//
// Routes registered:
//
//	GET /oauth/mastodon/callback — OAuth 2.0 callback from Mastodon
//	GET /oauth/linkedin/callback — OAuth 2.0 callback from LinkedIn (when configured)
//
// Call [Social.Stop] in your shutdown handler to drain the scheduler.
func (s *Social) Register(app *forge.App) {
	app.Handle("GET /oauth/mastodon/callback", http.HandlerFunc(s.handleMastodonCallback))
	if s.linkedin != nil {
		app.Handle("GET /oauth/linkedin/callback", http.HandlerFunc(s.handleLinkedInCallback))
	}
	s.sched.start()
}

// Stop gracefully shuts down the scheduler and waits for any in-progress
// publish operation to complete. Call this in your application's shutdown handler.
func (s *Social) Stop() {
	s.sched.stop()
}

// PostModule returns a [forge.MCPModule] that exposes [ScheduledPost] as MCP
// tools (create, update, publish, archive, delete, list, get).
// Pass it to forgemcp.WithModule when wiring the MCP server.
func (s *Social) PostModule() forge.MCPModule {
	return &postModule{social: s}
}

// CredentialModule returns a [forge.MCPModule] that exposes [PlatformCredential]
// as MCP tools (create/connect, list, get, delete).
// Pass it to forgemcp.WithModule when wiring the MCP server.
func (s *Social) CredentialModule() forge.MCPModule {
	return &credentialModule{social: s}
}

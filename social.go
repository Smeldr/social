// Package forgesocial provides platform publishing for Forge applications.
// It supports scheduling and publishing content to Mastodon and LinkedIn
// via OAuth 2.0.
//
// # Quick start
//
//	import forgesocial "smeldr.dev/social"
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
//	// Layer 1 — wire agent routing (optional).
//	// Fires on AfterPublish for "Post" content type.
//	social.AddRoutes(app,
//	    forgesocial.OnPublish("Post", "https://agent.example.com/social"),
//	)
package forgesocial

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	forge "smeldr.dev/core"
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
// Optionally register agent routes with [AddRoutes] for Layer 1 routing.
// Call [Stop] in your application's shutdown handler.
type Social struct {
	db           forge.DB
	cfg          Config
	creds        *credentialStore
	platformCfgs *platformConfigStore
	mu           sync.RWMutex // protects mastodon, linkedin, twitter
	mastodon     *mastodonClient
	linkedin     *linkedinClient
	twitter      *twitterClient
	sched        *scheduler
	router       *Router // nil if AddRoutes was not called
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
	pcs := newPlatformConfigStore(db, cfg.Secret)
	s := &Social{
		db:           db,
		cfg:          cfg,
		creds:        cs,
		platformCfgs: pcs,
	}

	// Load platform config from DB; fall back to forgesocial.Config env vars.
	if dbCfg, ok, err := pcs.load("mastodon"); err == nil && ok {
		s.mastodon = newMastodonClient(MastodonConfig{
			ClientID:     dbCfg.ClientID,
			ClientSecret: dbCfg.ClientSecret,
			InstanceURL:  dbCfg.InstanceURL,
			RedirectURL:  dbCfg.RedirectURL,
			SuccessURL:   dbCfg.SuccessURL,
		})
	} else if err == nil && cfg.Mastodon.ClientID != "" {
		log.Println("forgesocial: DEPRECATED: MastodonConfig in forgesocial.Config; use configure_platform MCP tool instead")
		s.mastodon = newMastodonClient(cfg.Mastodon)
	}

	if dbCfg, ok, err := pcs.load("linkedin"); err == nil && ok {
		s.linkedin = newLinkedinClient(LinkedInConfig{
			ClientID:     dbCfg.ClientID,
			ClientSecret: dbCfg.ClientSecret,
			RedirectURL:  dbCfg.RedirectURL,
			SuccessURL:   dbCfg.SuccessURL,
		})
	} else if err == nil && cfg.LinkedIn.ClientID != "" {
		log.Println("forgesocial: DEPRECATED: LinkedInConfig in forgesocial.Config; use configure_platform MCP tool instead")
		s.linkedin = newLinkedinClient(cfg.LinkedIn)
	}

	if dbCfg, ok, err := pcs.load("x"); err == nil && ok {
		s.twitter = newTwitterClient(xConfig{
			ClientID:     dbCfg.ClientID,
			ClientSecret: dbCfg.ClientSecret,
			RedirectURL:  dbCfg.RedirectURL,
			SuccessURL:   dbCfg.SuccessURL,
		})
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
//	GET /oauth/x/callback        — OAuth 2.0 + PKCE callback from X (when configured)
//
// Call [Social.Stop] in your shutdown handler to drain the scheduler.
func (s *Social) Register(app *forge.App) {
	app.Handle("GET /oauth/mastodon/callback", http.HandlerFunc(s.handleMastodonCallback))
	if s.linkedin != nil {
		app.Handle("GET /oauth/linkedin/callback", http.HandlerFunc(s.handleLinkedInCallback))
	}
	app.Handle("GET /oauth/x/callback", http.HandlerFunc(s.handleXCallback))
	s.sched.start()
}

// Stop gracefully shuts down the scheduler and (if AddRoutes was called) the
// route delivery worker. It waits for any in-progress operations to complete.
// Call this in your application's shutdown handler.
func (s *Social) Stop() {
	s.sched.stop()
	if s.router != nil {
		s.router.stopWorker()
	}
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

// ScheduleModule returns a [forge.MCPModule] that exposes [PublicationSchedule]
// as MCP tools (create, update, get, list, delete).
// Pass it to forgemcp.WithModule when wiring the MCP server.
func (s *Social) ScheduleModule() forge.MCPModule {
	return &scheduleModule{social: s}
}

// ConfigModule returns a [forge.MCPModule] that exposes the configure_platform
// admin tool for storing per-platform OAuth 2.0 app credentials in the DB.
// Pass it to forgemcp.WithModule when wiring the MCP server.
// Only users with Admin role can call the configure_platform tool.
func (s *Social) ConfigModule() forge.MCPModule {
	return &configModule{social: s}
}

// reloadPlatformClient re-initialises the client for the given platform
// using the provided config. Called by ConfigModule after saving new credentials
// so the new config takes effect for subsequent publishes without a restart.
func (s *Social) reloadPlatformClient(platform string, cfg PlatformConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch platform {
	case "mastodon":
		s.mastodon = newMastodonClient(MastodonConfig{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			InstanceURL:  cfg.InstanceURL,
			RedirectURL:  cfg.RedirectURL,
			SuccessURL:   cfg.SuccessURL,
		})
	case "linkedin":
		s.linkedin = newLinkedinClient(LinkedInConfig{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			SuccessURL:   cfg.SuccessURL,
		})
	case "x":
		s.twitter = newTwitterClient(xConfig{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			SuccessURL:   cfg.SuccessURL,
		})
	}
}

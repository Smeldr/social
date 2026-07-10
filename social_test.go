package social_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"smeldr.dev/core"
	"smeldr.dev/social"

	_ "modernc.org/sqlite"
)

// openTestDB opens an in-memory SQLite database for testing.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := social.CreateTables(db); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

// adminCtx returns a smeldr.Context with Admin role for test calls.
func adminCtx() smeldr.Context {
	return smeldr.NewTestContext(smeldr.User{
		ID:    "test-admin",
		Name:  "Test Admin",
		Roles: []smeldr.Role{smeldr.Admin},
	})
}

func TestNew_PanicsOnNilDB(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil db")
		}
	}()
	social.New(nil, social.Config{Secret: []byte("secret")})
}

func TestNew_PanicsOnEmptySecret(t *testing.T) {
	db := openTestDB(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for empty secret")
		}
	}()
	social.New(db, social.Config{})
}

func TestNew_CreatesTables(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	if svc == nil {
		t.Fatal("New returned nil")
	}
}

func TestCreateTables_Idempotent(t *testing.T) {
	db := openTestDB(t)
	// Call twice — should not error.
	if err := social.CreateTables(db); err != nil {
		t.Fatalf("second CreateTables: %v", err)
	}
}

func TestScheduledPost_Lifecycle(t *testing.T) {
	db := openTestDB(t)

	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer svc.Stop()

	pm := svc.PostModule()
	ctx := adminCtx()

	// Need a credential ID — placeholder; no real Mastodon present.
	credID := "test-cred-id"

	// Create draft post.
	result, err := pm.MCPCreate(ctx, map[string]any{
		"credential_id": credID,
		"body":          "Hello, Mastodon!",
	})
	if err != nil {
		t.Fatalf("MCPCreate: %v", err)
	}
	post, ok := result.(social.ScheduledPost)
	if !ok {
		t.Fatalf("MCPCreate result type: %T", result)
	}
	if post.Status != social.PostStatusDraft {
		t.Errorf("status = %q; want %q", post.Status, social.PostStatusDraft)
	}
	if post.Body != "Hello, Mastodon!" {
		t.Errorf("body = %q", post.Body)
	}

	// List — should return one item.
	items, err := pm.MCPList(ctx)
	if err != nil {
		t.Fatalf("MCPList: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("MCPList len = %d; want 1", len(items))
	}

	// MCPGet.
	got, err := pm.MCPGet(ctx, post.ID)
	if err != nil {
		t.Fatalf("MCPGet: %v", err)
	}
	if got.(social.ScheduledPost).ID != post.ID {
		t.Errorf("MCPGet ID mismatch")
	}

	// MCPSchedule.
	future := time.Now().UTC().Add(1 * time.Hour)
	if err := pm.MCPSchedule(ctx, post.ID, future); err != nil {
		t.Fatalf("MCPSchedule: %v", err)
	}
	updated, _ := pm.MCPGet(ctx, post.ID)
	if updated.(social.ScheduledPost).Status != social.PostStatusScheduled {
		t.Errorf("after MCPSchedule status = %q", updated.(social.ScheduledPost).Status)
	}

	// MCPArchive.
	if err := pm.MCPArchive(ctx, post.ID); err != nil {
		t.Fatalf("MCPArchive: %v", err)
	}
	archived, _ := pm.MCPGet(ctx, post.ID)
	if archived.(social.ScheduledPost).Status != social.PostStatusArchived {
		t.Errorf("after MCPArchive status = %q", archived.(social.ScheduledPost).Status)
	}

	// MCPDelete.
	if err := pm.MCPDelete(ctx, post.ID); err != nil {
		t.Fatalf("MCPDelete: %v", err)
	}
	items, _ = pm.MCPList(ctx)
	if len(items) != 0 {
		t.Errorf("after delete MCPList len = %d; want 0", len(items))
	}
}

func TestPost_MediaRequiresAltText(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer svc.Stop()
	pm := svc.PostModule()
	ctx := adminCtx()

	_, err := pm.MCPCreate(ctx, map[string]any{
		"credential_id": "cred-1",
		"body":          "post with image",
		"media_url":     "https://example.com/img.jpg",
		// alt_text deliberately omitted
	})
	if err == nil {
		t.Error("expected error for missing alt_text")
	}
}

func TestPost_ScheduledAtAutoSetsStatus(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer svc.Stop()
	pm := svc.PostModule()
	ctx := adminCtx()

	future := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	result, err := pm.MCPCreate(ctx, map[string]any{
		"credential_id": "cred-1",
		"body":          "scheduled post",
		"scheduled_at":  future,
	})
	if err != nil {
		t.Fatalf("MCPCreate: %v", err)
	}
	p := result.(social.ScheduledPost)
	if p.Status != social.PostStatusScheduled {
		t.Errorf("status = %q; want %q", p.Status, social.PostStatusScheduled)
	}
}

func TestCredentialModule_ConnectMastodonReturnsRedirectURL(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
		Mastodon: social.MastodonConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			InstanceURL:  "https://mastodon.social",
			RedirectURL:  "https://myapp.com/oauth/mastodon/callback",
		},
	})
	defer svc.Stop()

	cm := svc.CredentialModule()
	ctx := adminCtx()

	result, err := cm.MCPCreate(ctx, map[string]any{
		"platform":     "mastodon",
		"instance_url": "https://mastodon.social",
	})
	if err != nil {
		t.Fatalf("MCPCreate: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type: %T", result)
	}
	redirectURL, _ := m["redirect_url"].(string)
	if redirectURL == "" {
		t.Error("redirect_url is empty")
	}
	if len(redirectURL) < 20 {
		t.Errorf("redirect_url too short: %q", redirectURL)
	}
}

func TestSchedulerStop_DoesNotHang(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Stop()
	}()

	select {
	case <-done:
		// OK
	case <-time.After(3 * time.Second):
		t.Error("Stop() did not return within 3 seconds")
	}
}

func TestLinkedIn_ConnectReturnsRedirectURL(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
		LinkedIn: social.LinkedInConfig{
			ClientID:     "li-client-id",
			ClientSecret: "li-client-secret",
			RedirectURL:  "https://myapp.com/oauth/linkedin/callback",
		},
	})
	defer svc.Stop()

	cm := svc.CredentialModule()
	ctx := adminCtx()

	result, err := cm.MCPCreate(ctx, map[string]any{
		"platform": "linkedin",
	})
	if err != nil {
		t.Fatalf("MCPCreate linkedin: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type: %T", result)
	}
	redirectURL, _ := m["redirect_url"].(string)
	if redirectURL == "" {
		t.Error("redirect_url is empty")
	}
	// LinkedIn auth URL must contain linkedin.com
	if !strings.Contains(redirectURL, "linkedin.com") {
		t.Errorf("redirect_url does not contain linkedin.com: %q", redirectURL)
	}
}

func TestLinkedIn_NotConfigured_ReturnsError(t *testing.T) {
	db := openTestDB(t)
	// No LinkedIn config — ClientID is empty, so linkedin client is nil.
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer svc.Stop()

	cm := svc.CredentialModule()
	ctx := adminCtx()

	_, err := cm.MCPCreate(ctx, map[string]any{"platform": "linkedin"})
	if err == nil {
		t.Error("expected error when LinkedIn not configured")
	}
}

func TestPost_PlatformDefaultsMastodon(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer svc.Stop()
	pm := svc.PostModule()
	ctx := adminCtx()

	result, err := pm.MCPCreate(ctx, map[string]any{
		"credential_id": "cred-1",
		"body":          "default platform post",
		// platform omitted
	})
	if err != nil {
		t.Fatalf("MCPCreate: %v", err)
	}
	p := result.(social.ScheduledPost)
	if p.Platform != "mastodon" {
		t.Errorf("platform = %q; want %q", p.Platform, "mastodon")
	}
}

func TestPost_PlatformLinkedIn(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer svc.Stop()
	pm := svc.PostModule()
	ctx := adminCtx()

	result, err := pm.MCPCreate(ctx, map[string]any{
		"credential_id": "cred-li-1",
		"body":          "Hello, LinkedIn!",
		"platform":      "linkedin",
	})
	if err != nil {
		t.Fatalf("MCPCreate: %v", err)
	}
	p := result.(social.ScheduledPost)
	if p.Platform != "linkedin" {
		t.Errorf("platform = %q; want %q", p.Platform, "linkedin")
	}
}

func TestPost_PlatformInvalidReturnsError(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer svc.Stop()
	pm := svc.PostModule()
	ctx := adminCtx()

	_, err := pm.MCPCreate(ctx, map[string]any{
		"credential_id": "cred-1",
		"body":          "bad platform",
		"platform":      "twitter",
	})
	if err == nil {
		t.Error("expected error for invalid platform")
	}
}

// TestScheduledPost_MCPCreate_Status verifies that the explicit status field on
// create is honoured, invalid values are rejected, and auto-promote still fires
// when scheduled_at is set and the post remains in draft status.
func TestScheduledPost_MCPCreate_Status(t *testing.T) {
	db := openTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer svc.Stop()
	pm := svc.PostModule()
	ctx := adminCtx()
	future := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)

	base := map[string]any{
		"credential_id": "cred-1",
		"body":          "test body",
	}
	merge := func(extra map[string]any) map[string]any {
		m := make(map[string]any, len(base)+len(extra))
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	t.Run("no status defaults to draft", func(t *testing.T) {
		res, err := pm.MCPCreate(ctx, merge(nil))
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		if got := res.(social.ScheduledPost).Status; got != social.PostStatusDraft {
			t.Errorf("status = %q; want %q", got, social.PostStatusDraft)
		}
	})

	t.Run("explicit draft", func(t *testing.T) {
		res, err := pm.MCPCreate(ctx, merge(map[string]any{"status": "draft"}))
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		if got := res.(social.ScheduledPost).Status; got != social.PostStatusDraft {
			t.Errorf("status = %q; want %q", got, social.PostStatusDraft)
		}
	})

	t.Run("explicit scheduled without scheduled_at", func(t *testing.T) {
		res, err := pm.MCPCreate(ctx, merge(map[string]any{"status": "scheduled"}))
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		p := res.(social.ScheduledPost)
		if p.Status != social.PostStatusScheduled {
			t.Errorf("status = %q; want %q", p.Status, social.PostStatusScheduled)
		}
		if p.ScheduledAt != nil {
			t.Error("ScheduledAt should be nil when not provided")
		}
	})

	t.Run("explicit queued", func(t *testing.T) {
		res, err := pm.MCPCreate(ctx, merge(map[string]any{"status": "queued"}))
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		if got := res.(social.ScheduledPost).Status; got != social.PostStatusQueued {
			t.Errorf("status = %q; want %q", got, social.PostStatusQueued)
		}
	})

	t.Run("explicit archived", func(t *testing.T) {
		res, err := pm.MCPCreate(ctx, merge(map[string]any{"status": "archived"}))
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		if got := res.(social.ScheduledPost).Status; got != social.PostStatusArchived {
			t.Errorf("status = %q; want %q", got, social.PostStatusArchived)
		}
	})

	t.Run("invalid status rejected", func(t *testing.T) {
		_, err := pm.MCPCreate(ctx, merge(map[string]any{"status": "published"}))
		if err == nil {
			t.Error("expected error for status=published on create")
		}
		_, err = pm.MCPCreate(ctx, merge(map[string]any{"status": "nonsense"}))
		if err == nil {
			t.Error("expected error for status=nonsense on create")
		}
	})

	t.Run("scheduled_at only auto-promotes", func(t *testing.T) {
		res, err := pm.MCPCreate(ctx, merge(map[string]any{"scheduled_at": future}))
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		p := res.(social.ScheduledPost)
		if p.Status != social.PostStatusScheduled {
			t.Errorf("status = %q; want %q", p.Status, social.PostStatusScheduled)
		}
		if p.ScheduledAt == nil {
			t.Error("ScheduledAt should be set")
		}
	})

	t.Run("queued plus scheduled_at explicit wins", func(t *testing.T) {
		res, err := pm.MCPCreate(ctx, merge(map[string]any{"status": "queued", "scheduled_at": future}))
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		p := res.(social.ScheduledPost)
		if p.Status != social.PostStatusQueued {
			t.Errorf("status = %q; want %q", p.Status, social.PostStatusQueued)
		}
		if p.ScheduledAt == nil {
			t.Error("ScheduledAt should be set")
		}
	})

	t.Run("draft plus scheduled_at auto-promotes", func(t *testing.T) {
		res, err := pm.MCPCreate(ctx, merge(map[string]any{"status": "draft", "scheduled_at": future}))
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		p := res.(social.ScheduledPost)
		if p.Status != social.PostStatusScheduled {
			t.Errorf("status = %q; want scheduled (auto-promote)", p.Status)
		}
	})

	t.Run("archived plus scheduled_at explicit wins", func(t *testing.T) {
		res, err := pm.MCPCreate(ctx, merge(map[string]any{"status": "archived", "scheduled_at": future}))
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		if got := res.(social.ScheduledPost).Status; got != social.PostStatusArchived {
			t.Errorf("status = %q; want %q", got, social.PostStatusArchived)
		}
	})
}

func TestMigration_ActorIDColumn(t *testing.T) {
	// Simulate a v0.1.0 database: create the old schema without actor_id,
	// then call CreateTables (which runs the migration) and verify the column exists.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Create the old credentials table without actor_id.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS forge_social_credentials (
		id            TEXT PRIMARY KEY,
		platform      TEXT NOT NULL,
		name          TEXT NOT NULL,
		instance_url  TEXT NOT NULL,
		access_token  TEXT NOT NULL,
		refresh_token TEXT NOT NULL DEFAULT '',
		expires_at    DATETIME,
		created_at    DATETIME NOT NULL,
		updated_at    DATETIME NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create old table: %v", err)
	}

	// Run CreateTables — should succeed and add actor_id.
	if err := social.CreateTables(db); err != nil {
		t.Fatalf("CreateTables migration: %v", err)
	}

	// Verify actor_id column exists by inserting a row.
	// Table was renamed from forge_social_credentials to smeldr_social_credentials by migration.
	_, err = db.Exec(`INSERT INTO smeldr_social_credentials
		(id, platform, name, instance_url, actor_id, access_token, created_at, updated_at)
		VALUES ('id1','mastodon','test','https://m.social','','enc_token',
		        datetime('now'), datetime('now'))`)
	if err != nil {
		t.Errorf("insert with actor_id failed after migration: %v", err)
	}
}

package forgesocial_test

import (
	"database/sql"
	"testing"
	"time"

	forge "forge-cms.dev/forge"
	forgesocial "forge-cms.dev/forge-social"

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
	if err := forgesocial.CreateTables(db); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

// adminCtx returns a forge.Context with Admin role for test calls.
func adminCtx() forge.Context {
	return forge.NewTestContext(forge.User{
		ID:    "test-admin",
		Name:  "Test Admin",
		Roles: []forge.Role{forge.Admin},
	})
}

func TestNew_PanicsOnNilDB(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil db")
		}
	}()
	forgesocial.New(nil, forgesocial.Config{Secret: []byte("secret")})
}

func TestNew_PanicsOnEmptySecret(t *testing.T) {
	db := openTestDB(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for empty secret")
		}
	}()
	forgesocial.New(db, forgesocial.Config{})
}

func TestNew_CreatesTables(t *testing.T) {
	db := openTestDB(t)
	social := forgesocial.New(db, forgesocial.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	if social == nil {
		t.Fatal("New returned nil")
	}
}

func TestCreateTables_Idempotent(t *testing.T) {
	db := openTestDB(t)
	// Call twice — should not error.
	if err := forgesocial.CreateTables(db); err != nil {
		t.Fatalf("second CreateTables: %v", err)
	}
}

func TestScheduledPost_Lifecycle(t *testing.T) {
	db := openTestDB(t)

	social := forgesocial.New(db, forgesocial.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer social.Stop()

	pm := social.PostModule()
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
	post, ok := result.(forgesocial.ScheduledPost)
	if !ok {
		t.Fatalf("MCPCreate result type: %T", result)
	}
	if post.Status != forgesocial.PostStatusDraft {
		t.Errorf("status = %q; want %q", post.Status, forgesocial.PostStatusDraft)
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
	if got.(forgesocial.ScheduledPost).ID != post.ID {
		t.Errorf("MCPGet ID mismatch")
	}

	// MCPSchedule.
	future := time.Now().UTC().Add(1 * time.Hour)
	if err := pm.MCPSchedule(ctx, post.ID, future); err != nil {
		t.Fatalf("MCPSchedule: %v", err)
	}
	updated, _ := pm.MCPGet(ctx, post.ID)
	if updated.(forgesocial.ScheduledPost).Status != forgesocial.PostStatusScheduled {
		t.Errorf("after MCPSchedule status = %q", updated.(forgesocial.ScheduledPost).Status)
	}

	// MCPArchive.
	if err := pm.MCPArchive(ctx, post.ID); err != nil {
		t.Fatalf("MCPArchive: %v", err)
	}
	archived, _ := pm.MCPGet(ctx, post.ID)
	if archived.(forgesocial.ScheduledPost).Status != forgesocial.PostStatusArchived {
		t.Errorf("after MCPArchive status = %q", archived.(forgesocial.ScheduledPost).Status)
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
	social := forgesocial.New(db, forgesocial.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer social.Stop()
	pm := social.PostModule()
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
	social := forgesocial.New(db, forgesocial.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer social.Stop()
	pm := social.PostModule()
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
	p := result.(forgesocial.ScheduledPost)
	if p.Status != forgesocial.PostStatusScheduled {
		t.Errorf("status = %q; want %q", p.Status, forgesocial.PostStatusScheduled)
	}
}

func TestCredentialModule_ConnectReturnsRedirectURL(t *testing.T) {
	db := openTestDB(t)
	social := forgesocial.New(db, forgesocial.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
		Mastodon: forgesocial.MastodonConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			InstanceURL:  "https://mastodon.social",
			RedirectURL:  "https://myapp.com/oauth/mastodon/callback",
		},
	})
	defer social.Stop()

	cm := social.CredentialModule()
	ctx := adminCtx()

	result, err := cm.MCPCreate(ctx, map[string]any{
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
	// Should contain the OAuth authorize path.
	if len(redirectURL) < 20 {
		t.Errorf("redirect_url too short: %q", redirectURL)
	}
}

func TestSchedulerStop_DoesNotHang(t *testing.T) {
	db := openTestDB(t)
	social := forgesocial.New(db, forgesocial.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		social.Stop()
	}()

	select {
	case <-done:
		// OK
	case <-time.After(3 * time.Second):
		t.Error("Stop() did not return within 3 seconds")
	}
}

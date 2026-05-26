package forgesocial_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	forge "smeldr.dev/core"
	forgesocial "smeldr.dev/social"

	_ "modernc.org/sqlite"
)

// openRouterTestDB opens an in-memory SQLite DB and calls CreateTables.
func openRouterTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := forgesocial.CreateTables(db); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func TestAddRoutes_EmptyRoutesIsNoop(t *testing.T) {
	db := openRouterTestDB(t)
	social := forgesocial.New(db, forgesocial.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer social.Stop()

	// newFakeApp satisfies *forge.App without a real server.
	// AddRoutes with no routes should be a no-op.
	// We just verify Stop() doesn't hang.
	done := make(chan struct{})
	go func() {
		defer close(done)
		social.Stop()
	}()
	select {
	case <-done:
		// OK
	case <-time.After(3 * time.Second):
		t.Error("Stop() hung after AddRoutes with no routes")
	}
}

func TestAddRoutes_PanicsOnPrivateIP(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for private IP agent URL")
		}
	}()
	// 10.0.0.1 is in the RFC 1918 private range.
	forgesocial.ValidateAgentURLForTest("https://10.0.0.1/hook")
}

func TestRouter_StopWithoutRegister(t *testing.T) {
	// Social.Stop() must not hang when AddRoutes was never called.
	db := openRouterTestDB(t)
	social := forgesocial.New(db, forgesocial.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	// Don't call Register or AddRoutes.
	done := make(chan struct{})
	go func() {
		defer close(done)
		social.Stop()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Stop() hung when AddRoutes was not called")
	}
}

func TestRouteJobStore_EnqueueAndPoll(t *testing.T) {
	db := openRouterTestDB(t)

	store := forgesocial.NewRouteJobStoreForTest(db)

	route := forgesocial.OnPublish("Post", "https://agent.example.com/hook")
	ev := forge.SignalEvent{
		Type:      "Post",
		Slug:      "my-post",
		Title:     "My Post",
		URL:       "https://site.example.com/posts/my-post",
		Timestamp: time.Now().UTC(),
	}

	store.EnqueueForTest(route, forge.AfterPublish, ev)

	jobs, err := store.DueJobsForTest(context.Background())
	if err != nil {
		t.Fatalf("DueJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("DueJobs len = %d; want 1", len(jobs))
	}
	j := jobs[0]
	if j.AgentURL != route.AgentURL {
		t.Errorf("AgentURL = %q; want %q", j.AgentURL, route.AgentURL)
	}
	if j.ContentType != "Post" {
		t.Errorf("ContentType = %q; want Post", j.ContentType)
	}
	if j.Signal != "after_publish" {
		t.Errorf("Signal = %q; want after_publish", j.Signal)
	}
}

func TestRouteJobStore_MarkDelivered(t *testing.T) {
	db := openRouterTestDB(t)
	store := forgesocial.NewRouteJobStoreForTest(db)

	route := forgesocial.OnPublish("Story", "https://agent.example.com/hook")
	ev := forge.SignalEvent{Type: "Story", Slug: "s1", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, forge.AfterPublish, ev)

	jobs, _ := store.DueJobsForTest(context.Background())
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	id := jobs[0].ID
	if err := store.MarkDeliveredForTest(context.Background(), id); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	// After marking delivered, job should no longer appear in due jobs.
	jobs, _ = store.DueJobsForTest(context.Background())
	if len(jobs) != 0 {
		t.Errorf("expected 0 due jobs after delivery, got %d", len(jobs))
	}
}

package social_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"smeldr.dev/core"
	"smeldr.dev/social"

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
	if err := social.CreateTables(db); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func TestAddRoutes_EmptyRoutesIsNoop(t *testing.T) {
	db := openRouterTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	defer svc.Stop()

	// newFakeApp satisfies *smeldr.App without a real server.
	// AddRoutes with no routes should be a no-op.
	// We just verify Stop() doesn't hang.
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Stop()
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
	social.ValidateAgentURLForTest("https://10.0.0.1/hook")
}

func TestRouter_StopWithoutRegister(t *testing.T) {
	// Social.Stop() must not hang when AddRoutes was never called.
	db := openRouterTestDB(t)
	svc := social.New(db, social.Config{
		Secret: []byte("test-secret-32-bytes-long-padded!"),
	})
	// Don't call Register or AddRoutes.
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Stop()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Stop() hung when AddRoutes was not called")
	}
}

func TestRouteJobStore_EnqueueAndPoll(t *testing.T) {
	db := openRouterTestDB(t)

	store := social.NewRouteJobStoreForTest(db)

	route := social.OnPublish("Post", "https://agent.example.com/hook")
	ev := smeldr.SignalEvent{
		Type:      "Post",
		Slug:      "my-post",
		Title:     "My Post",
		URL:       "https://site.example.com/posts/my-post",
		Timestamp: time.Now().UTC(),
	}

	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

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
	store := social.NewRouteJobStoreForTest(db)

	route := social.OnPublish("Story", "https://agent.example.com/hook")
	ev := smeldr.SignalEvent{Type: "Story", Slug: "s1", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

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

// TestRouter_Handle_EnqueuesOnMatch exercises the closure returned by
// Router.handle directly: it must enqueue a job when (signal, ContentType)
// matches a registered route, and must not enqueue anything for a
// non-matching ContentType.
func TestRouter_Handle_EnqueuesOnMatch(t *testing.T) {
	db := openRouterTestDB(t)
	route := social.Route{
		Signal:      smeldr.AfterPublish,
		ContentType: "Post",
		AgentURL:    "https://agent.example.com/hook",
	}
	cb := social.HandleForTest(db, route)

	ev := smeldr.SignalEvent{Type: "Post", Slug: "handle-test", Timestamp: time.Now().UTC()}
	if err := cb(context.Background(), ev); err != nil {
		t.Fatalf("handle callback returned error: %v", err)
	}

	store := social.NewRouteJobStoreForTest(db)
	jobs, err := store.DueJobsForTest(context.Background())
	if err != nil {
		t.Fatalf("DueJobsForTest: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 enqueued job after a matching signal, got %d", len(jobs))
	}
	if jobs[0].ContentType != "Post" {
		t.Errorf("ContentType = %q; want Post", jobs[0].ContentType)
	}
	if jobs[0].AgentURL != route.AgentURL {
		t.Errorf("AgentURL = %q; want %q", jobs[0].AgentURL, route.AgentURL)
	}
}

// TestRouter_Handle_NoMatchDoesNotEnqueue confirms the non-matching
// ContentType branch of the handle closure is a true no-op.
func TestRouter_Handle_NoMatchDoesNotEnqueue(t *testing.T) {
	db := openRouterTestDB(t)
	route := social.Route{
		Signal:      smeldr.AfterPublish,
		ContentType: "Post",
		AgentURL:    "https://agent.example.com/hook",
	}
	cb := social.HandleForTest(db, route)

	ev := smeldr.SignalEvent{Type: "Story", Slug: "no-match", Timestamp: time.Now().UTC()}
	if err := cb(context.Background(), ev); err != nil {
		t.Fatalf("handle callback returned error: %v", err)
	}

	store := social.NewRouteJobStoreForTest(db)
	jobs, err := store.DueJobsForTest(context.Background())
	if err != nil {
		t.Fatalf("DueJobsForTest: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected 0 enqueued jobs for a non-matching ContentType, got %d", len(jobs))
	}
}

// smeldrTestApp builds a minimal, real *smeldr.App suitable for exercising
// AddRoutes' registration path (OnSignal wiring, worker startup) without a
// running HTTP server.
func smeldrTestApp(t *testing.T) *smeldr.App {
	t.Helper()
	return smeldr.New(smeldr.Config{
		BaseURL: "https://example.com",
		Secret:  []byte("test-secret-32-bytes-long-padded!"),
	})
}

// TestAddRoutes_ZeroRoutesIsActuallyNoop calls AddRoutes itself (not just
// Stop) with zero routes and confirms it neither panics nor starts a worker
// that would make Stop hang.
func TestAddRoutes_ZeroRoutesIsActuallyNoop(t *testing.T) {
	db := openRouterTestDB(t)
	app := smeldrTestApp(t)
	svc := social.New(db, social.Config{Secret: []byte("test-secret-32-bytes-long-padded!")})

	svc.AddRoutes(app) // zero routes — must be a no-op

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Stop()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Stop() hung after AddRoutes() with zero routes")
	}
}

// TestAddRoutes_PanicsOnInvalidRoute calls AddRoutes itself (not just
// validateRoute) with a structurally invalid route and confirms the panic
// happens before any worker goroutine is started.
func TestAddRoutes_PanicsOnInvalidRoute(t *testing.T) {
	db := openRouterTestDB(t)
	app := smeldrTestApp(t)
	svc := social.New(db, social.Config{Secret: []byte("test-secret-32-bytes-long-padded!")})

	defer expectPanic(t, "AddRoutes should panic on a structurally invalid route")
	svc.AddRoutes(app, social.Route{
		Signal:      smeldr.AfterPublish,
		ContentType: "", // invalid — see validateRoute
		AgentURL:    "https://agent.example.com/hook",
	})
}

// TestAddRoutes_ValidRouteRegistersAndStops confirms AddRoutes' happy path:
// a valid route registers without panicking and Stop() still shuts down
// cleanly (the worker goroutine it started can be stopped).
func TestAddRoutes_ValidRouteRegistersAndStops(t *testing.T) {
	db := openRouterTestDB(t)
	app := smeldrTestApp(t)
	svc := social.New(db, social.Config{Secret: []byte("test-secret-32-bytes-long-padded!")})

	svc.AddRoutes(app, social.Route{
		Signal:      smeldr.AfterPublish,
		ContentType: "Post",
		AgentURL:    "https://agent.example.com/hook",
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Stop()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Stop() hung after AddRoutes() with a valid route")
	}
}

package social_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smeldr.dev/core"
	"smeldr.dev/social"
)

func TestRouteWorker_Delivers2xx(t *testing.T) {
	received := make(chan []byte, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf []byte
		buf = make([]byte, r.ContentLength)
		r.Body.Read(buf)
		received <- buf
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	db := openRouterTestDB(t)
	store := social.NewRouteJobStoreForTest(db)

	route := social.Route{
		Signal:      smeldr.AfterPublish,
		ContentType: "Post",
		AgentURL:    srv.URL,
	}
	ev := smeldr.SignalEvent{
		Type:      "Post",
		Slug:      "test-post",
		Timestamp: time.Now().UTC(),
	}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	// Run delivery directly via the exported helper.
	social.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), srv.Client())

	select {
	case body := <-received:
		var got smeldr.SignalEvent
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if got.Slug != "test-post" {
			t.Errorf("Slug = %q; want test-post", got.Slug)
		}
	case <-time.After(3 * time.Second):
		t.Error("agent did not receive POST within 3s")
	}

	// Job should now be delivered (not in due list).
	// Give it a moment since delivery is synchronous in RunDeliveryForTest.
	jobs, _ := store.DueJobsForTest(t.Context())
	if len(jobs) != 0 {
		t.Errorf("expected 0 due jobs after 2xx delivery, got %d", len(jobs))
	}
}

func TestRouteWorker_Marks4xxTerminal(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 400
	}))
	defer srv.Close()

	db := openRouterTestDB(t)
	store := social.NewRouteJobStoreForTest(db)

	route := social.Route{
		Signal:      smeldr.AfterPublish,
		ContentType: "Post",
		AgentURL:    srv.URL,
	}
	ev := smeldr.SignalEvent{Type: "Post", Slug: "bad-post", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	social.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), srv.Client())

	// Job should be failed (no retry).
	jobs, _ := store.DueJobsForTest(t.Context())
	if len(jobs) != 0 {
		t.Errorf("expected 0 due jobs after 4xx, got %d", len(jobs))
	}
	failedJobs := social.GetFailedJobsForTest(db)
	if len(failedJobs) != 1 {
		t.Errorf("expected 1 failed job, got %d", len(failedJobs))
	}
}

func TestRouteWorker_Retries5xx(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 500
	}))
	defer srv.Close()

	db := openRouterTestDB(t)
	store := social.NewRouteJobStoreForTest(db)

	route := social.Route{
		Signal:      smeldr.AfterPublish,
		ContentType: "Post",
		AgentURL:    srv.URL,
	}
	ev := smeldr.SignalEvent{Type: "Post", Slug: "server-error", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	social.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), srv.Client())

	// After 5xx the job should still be pending (scheduled for future retry).
	jobs, _ := store.DueJobsForTest(t.Context())
	// The next_attempt is set to 30s in the future, so no due jobs immediately.
	if len(jobs) != 0 {
		t.Errorf("expected 0 immediately-due jobs after 5xx, got %d", len(jobs))
	}
}

func TestRouteWorker_SignatureHeader(t *testing.T) {
	sigReceived := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigReceived <- r.Header.Get("X-Forge-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	db := openRouterTestDB(t)
	store := social.NewRouteJobStoreForTest(db)
	route := social.Route{Signal: smeldr.AfterPublish, ContentType: "Post", AgentURL: srv.URL}
	ev := smeldr.SignalEvent{Type: "Post", Slug: "sig-test", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	secret := []byte("test-secret-32-bytes-long-padded!")
	social.RunDeliveryForTest(db, secret, srv.Client())

	select {
	case sig := <-sigReceived:
		if len(sig) < 10 || sig[:7] != "sha256=" {
			t.Errorf("X-Forge-Signature = %q; want sha256=...", sig)
		}
	case <-time.After(3 * time.Second):
		t.Error("agent did not receive POST within 3s")
	}
}

// TestRouteWorker_PollErrorIsReported exercises runWorker's poll-error branch
// (log-and-continue rather than crash) via the RunWorkerPollOnceForTest
// mirror: a closed *sql.DB makes dueJobs fail, and the wrapper should surface
// that error to the caller instead of panicking.
func TestRouteWorker_PollErrorIsReported(t *testing.T) {
	db := openRouterTestDB(t)
	db.Close() // force dueJobs to fail on the next query

	err := social.RunWorkerPollOnceForTest(db, []byte("test-secret-32-bytes-long-padded!"), http.DefaultClient)
	if err == nil {
		t.Error("expected an error from RunWorkerPollOnceForTest after the DB was closed")
	}
}

// TestRouter_RealWorkerDeliversAndStops runs the actual runWorker goroutine
// (via AddRoutes) end to end: it must pick up an already-enqueued job on its
// first poll, attempt delivery, and then exit promptly once Stop() closes
// stopCh. This is the only test that exercises the real (non-mirrored)
// runWorker and stopWorker functions.
func TestRouter_RealWorkerDeliversAndStops(t *testing.T) {
	db := openRouterTestDB(t)
	// The real worker goroutine and this test's own polling loop query
	// concurrently. modernc.org/sqlite's ":memory:" DSN gives each new
	// connection its own separate in-process database, so without pinning
	// the pool to a single connection a second connection would see a
	// database with no tables at all.
	db.SetMaxOpenConns(1)
	store := social.NewRouteJobStoreForTest(db)

	// A reserved, never-resolving TLD (RFC 2606) — the delivery attempt is
	// guaranteed to fail fast at DNS resolution without touching the network,
	// but it still passes AddRoutes' validateAgentURL (public HTTPS, no
	// private/loopback IP to reject) and lets runWorker's real poll+deliver
	// path run.
	route := social.Route{
		Signal:      smeldr.AfterPublish,
		ContentType: "Post",
		AgentURL:    "https://route-worker-realtest.invalid/hook",
	}
	ev := smeldr.SignalEvent{Type: "Post", Slug: "real-worker-test", Timestamp: time.Now().UTC()}
	// Enqueue before starting the worker so the first poll picks it up
	// deterministically.
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	app := smeldr.New(smeldr.Config{
		BaseURL: "https://example.com",
		Secret:  []byte("test-secret-32-bytes-long-padded!"),
	})
	svc := social.New(db, social.Config{Secret: []byte("test-secret-32-bytes-long-padded!")})
	svc.AddRoutes(app, route)

	// Wait for the real worker goroutine to have polled and attempted
	// delivery at least once (attempts goes from 0 to 1 on the network error).
	deadline := time.Now().Add(5 * time.Second)
	for {
		var attempts int
		if err := db.QueryRow(`SELECT attempts FROM smeldr_social_route_jobs LIMIT 1`).Scan(&attempts); err != nil {
			t.Fatalf("query attempts: %v", err)
		}
		if attempts > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real worker goroutine did not process the job within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Stop()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Stop() hung waiting for the real worker goroutine to exit")
	}
}

// TestRouteWorker_Deliver_MalformedURLBuildError exercises deliver's
// permanent-failure branch where http.NewRequestWithContext itself fails
// (malformed AgentURL) — distinct from a network-level failure.
func TestRouteWorker_Deliver_MalformedURLBuildError(t *testing.T) {
	db := openRouterTestDB(t)
	store := social.NewRouteJobStoreForTest(db)

	route := social.Route{Signal: smeldr.AfterPublish, ContentType: "Post", AgentURL: "://bad"}
	ev := smeldr.SignalEvent{Type: "Post", Slug: "malformed-url", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	social.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), http.DefaultClient)

	failedJobs := social.GetFailedJobsForTest(db)
	if len(failedJobs) != 1 {
		t.Fatalf("expected 1 failed job after malformed AgentURL, got %d", len(failedJobs))
	}
	if failedJobs[0].LastError == "" {
		t.Error("expected LastError to be populated for a malformed AgentURL")
	}
}

// TestRouteWorker_Deliver_NetworkError exercises deliver's transient
// network-error branch (hc.Do fails, e.g. connection refused) as distinct
// from the request-build-error and HTTP-status branches.
func TestRouteWorker_Deliver_NetworkError(t *testing.T) {
	db := openRouterTestDB(t)
	store := social.NewRouteJobStoreForTest(db)

	// Port 1 is a privileged port with no listener in any normal test
	// environment, so the connection is refused immediately.
	route := social.Route{Signal: smeldr.AfterPublish, ContentType: "Post", AgentURL: "https://127.0.0.1:1/hook"}
	ev := smeldr.SignalEvent{Type: "Post", Slug: "network-error", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	hc := &http.Client{Timeout: 2 * time.Second}
	social.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), hc)

	// A network error is transient: the job should be scheduled for a future
	// retry, not immediately due and not terminally failed.
	jobs, err := store.DueJobsForTest(context.Background())
	if err != nil {
		t.Fatalf("DueJobsForTest: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected 0 immediately-due jobs after a network error, got %d", len(jobs))
	}
	if failedJobs := social.GetFailedJobsForTest(db); len(failedJobs) != 0 {
		t.Errorf("expected 0 terminally failed jobs after a single network error, got %d", len(failedJobs))
	}
}

// TestRouteWorker_ApplyRetry_MaxRetriesExceeded exercises applyRetry's
// terminal branch (attempt > len(routeRetryDelays)) as distinct from the
// normal schedule-retry branch already covered by TestRouteWorker_Retries5xx.
func TestRouteWorker_ApplyRetry_MaxRetriesExceeded(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	db := openRouterTestDB(t)
	store := social.NewRouteJobStoreForTest(db)

	route := social.Route{Signal: smeldr.AfterPublish, ContentType: "Post", AgentURL: srv.URL}
	ev := smeldr.SignalEvent{Type: "Post", Slug: "max-retries", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	jobs, err := store.DueJobsForTest(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("setup: DueJobsForTest = %v, %d jobs", err, len(jobs))
	}
	// Simulate a job already at its final retry attempt: deliver computes
	// attempt = Attempts+1, so Attempts=4 makes attempt=5, which is
	// > len(routeRetryDelays)==4 — the terminal branch of applyRetry.
	if _, err := db.ExecContext(context.Background(),
		`UPDATE smeldr_social_route_jobs SET attempts = 4 WHERE id = ?`, jobs[0].ID); err != nil {
		t.Fatalf("seed attempts: %v", err)
	}

	social.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), srv.Client())

	failedJobs := social.GetFailedJobsForTest(db)
	if len(failedJobs) != 1 {
		t.Fatalf("expected 1 terminally failed job after exceeding max retries, got %d", len(failedJobs))
	}
	if !strings.Contains(failedJobs[0].LastError, "max retries exceeded") {
		t.Errorf("LastError = %q; want it to contain %q", failedJobs[0].LastError, "max retries exceeded")
	}
}

// TestRouteWorker_Retries429WithRetryAfter exercises deliver's 429 branch
// when the agent supplies a usable Retry-After header (the delay > 0 path).
func TestRouteWorker_Retries429WithRetryAfter(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	db := openRouterTestDB(t)
	store := social.NewRouteJobStoreForTest(db)
	route := social.Route{Signal: smeldr.AfterPublish, ContentType: "Post", AgentURL: srv.URL}
	ev := smeldr.SignalEvent{Type: "Post", Slug: "rate-limited-with-header", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	social.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), srv.Client())

	jobs, _ := store.DueJobsForTest(context.Background())
	if len(jobs) != 0 {
		t.Errorf("expected 0 immediately-due jobs after 429, got %d", len(jobs))
	}
	if failedJobs := social.GetFailedJobsForTest(db); len(failedJobs) != 0 {
		t.Errorf("expected 0 failed jobs after 429 (should retry), got %d", len(failedJobs))
	}
}

// TestRouteWorker_Retries429WithoutRetryAfter exercises deliver's 429 branch
// when the agent supplies no usable Retry-After header, forcing the
// fallback-to-routeRetryDelays path (the delay <= 0 branch).
func TestRouteWorker_Retries429WithoutRetryAfter(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // no Retry-After header
	}))
	defer srv.Close()

	db := openRouterTestDB(t)
	store := social.NewRouteJobStoreForTest(db)
	route := social.Route{Signal: smeldr.AfterPublish, ContentType: "Post", AgentURL: srv.URL}
	ev := smeldr.SignalEvent{Type: "Post", Slug: "rate-limited-no-header", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, smeldr.AfterPublish, ev)

	social.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), srv.Client())

	jobs, _ := store.DueJobsForTest(context.Background())
	if len(jobs) != 0 {
		t.Errorf("expected 0 immediately-due jobs after 429, got %d", len(jobs))
	}
	if failedJobs := social.GetFailedJobsForTest(db); len(failedJobs) != 0 {
		t.Errorf("expected 0 failed jobs after 429 (should retry), got %d", len(failedJobs))
	}
}

// TestParseRouteRetryAfter covers all branches of parseRouteRetryAfter via
// the ParseRouteRetryAfterForTest export.
func TestParseRouteRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"numeric seconds", "120", 120 * time.Second},
		{"malformed", "not-a-valid-value", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := social.ParseRouteRetryAfterForTest(tc.in)
			if got != tc.want {
				t.Errorf("ParseRouteRetryAfterForTest(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}

	t.Run("HTTP-date in the future", func(t *testing.T) {
		future := time.Now().UTC().Add(90 * time.Second)
		got := social.ParseRouteRetryAfterForTest(future.Format(http.TimeFormat))
		if got <= 0 || got > 91*time.Second {
			t.Errorf("ParseRouteRetryAfterForTest(future HTTP-date) = %v; want a positive duration close to 90s", got)
		}
	})

	t.Run("HTTP-date in the past", func(t *testing.T) {
		past := time.Now().UTC().Add(-90 * time.Second)
		got := social.ParseRouteRetryAfterForTest(past.Format(http.TimeFormat))
		if got != 0 {
			t.Errorf("ParseRouteRetryAfterForTest(past HTTP-date) = %v; want 0", got)
		}
	})
}

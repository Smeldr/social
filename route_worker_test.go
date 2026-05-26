package forgesocial_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	forge "smeldr.dev/core"
	forgesocial "smeldr.dev/social"
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
	store := forgesocial.NewRouteJobStoreForTest(db)

	route := forgesocial.Route{
		Signal:      forge.AfterPublish,
		ContentType: "Post",
		AgentURL:    srv.URL,
	}
	ev := forge.SignalEvent{
		Type:      "Post",
		Slug:      "test-post",
		Timestamp: time.Now().UTC(),
	}
	store.EnqueueForTest(route, forge.AfterPublish, ev)

	// Run delivery directly via the exported helper.
	forgesocial.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), srv.Client())

	select {
	case body := <-received:
		var got forge.SignalEvent
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
	store := forgesocial.NewRouteJobStoreForTest(db)

	route := forgesocial.Route{
		Signal:      forge.AfterPublish,
		ContentType: "Post",
		AgentURL:    srv.URL,
	}
	ev := forge.SignalEvent{Type: "Post", Slug: "bad-post", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, forge.AfterPublish, ev)

	forgesocial.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), srv.Client())

	// Job should be failed (no retry).
	jobs, _ := store.DueJobsForTest(t.Context())
	if len(jobs) != 0 {
		t.Errorf("expected 0 due jobs after 4xx, got %d", len(jobs))
	}
	failedJobs := forgesocial.GetFailedJobsForTest(db)
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
	store := forgesocial.NewRouteJobStoreForTest(db)

	route := forgesocial.Route{
		Signal:      forge.AfterPublish,
		ContentType: "Post",
		AgentURL:    srv.URL,
	}
	ev := forge.SignalEvent{Type: "Post", Slug: "server-error", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, forge.AfterPublish, ev)

	forgesocial.RunDeliveryForTest(db, []byte("test-secret-32-bytes-long-padded!"), srv.Client())

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
	store := forgesocial.NewRouteJobStoreForTest(db)
	route := forgesocial.Route{Signal: forge.AfterPublish, ContentType: "Post", AgentURL: srv.URL}
	ev := forge.SignalEvent{Type: "Post", Slug: "sig-test", Timestamp: time.Now().UTC()}
	store.EnqueueForTest(route, forge.AfterPublish, ev)

	secret := []byte("test-secret-32-bytes-long-padded!")
	forgesocial.RunDeliveryForTest(db, secret, srv.Client())

	select {
	case sig := <-sigReceived:
		if len(sig) < 10 || sig[:7] != "sha256=" {
			t.Errorf("X-Forge-Signature = %q; want sha256=...", sig)
		}
	case <-time.After(3 * time.Second):
		t.Error("agent did not receive POST within 3s")
	}
}

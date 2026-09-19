package social

// scheduler_test.go covers scheduler.go: the scheduler goroutine lifecycle
// (start/stop/run), processDue, processSlotQueue, publishWithRetry,
// publishNow, callPlatformPublish, and nextInterval.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smeldr.dev/core"

	_ "modernc.org/sqlite"
)

// ─── shared setup ───────────────────────────────────────────────────────────

// newSchedulerTestSocial opens a fresh in-memory DB and returns a *Social
// backed by it, with no platform clients wired in.
func newSchedulerTestSocial(t *testing.T) (*Social, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db, Config{Secret: refreshTestSecret})
	return s, db
}

// insertMastodonCredential creates a mastodon credential pointed at srv and
// returns its ID.
func insertMastodonCredential(t *testing.T, s *Social, srvURL string) string {
	t.Helper()
	id, err := s.creds.upsertCredentialByInstance("mastodon", srvURL, "test-cred", "tok", "", "", nil)
	if err != nil {
		t.Fatalf("insertMastodonCredential: %v", err)
	}
	return id
}

// ctxErrAfterN is a context.Context whose Err() returns nil for the first n
// calls and context.Canceled after that. Used to distinguish an "outer loop"
// ctx.Err() check from an "inner loop" one within the same function call,
// which cannot be done with a plain WithCancel context.
type ctxErrAfterN struct {
	context.Context
	calls int
	after int
}

func (c *ctxErrAfterN) Err() error {
	c.calls++
	if c.calls > c.after {
		return context.Canceled
	}
	return nil
}

// ─── processDue ─────────────────────────────────────────────────────────────

func TestSchedulerProcessDue(t *testing.T) {
	t.Run("duePosts error stops before processSlotQueue", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		sched := seedSchedule(t, db, "cred-due-err", ScheduleStatusActive)

		if _, err := db.Exec("DROP TABLE smeldr_social_posts"); err != nil {
			t.Fatalf("drop posts table: %v", err)
		}

		sc := newScheduler(s)
		sc.processDue(context.Background())

		// If processSlotQueue had run despite duePosts failing, last_tick_at
		// would now be set. It must still be nil — dropping smeldr_social_posts
		// doesn't touch smeldr_social_publication_schedules, so we can still
		// read it directly.
		var lastTick sql.NullTime
		if err := db.QueryRow(
			`SELECT last_tick_at FROM smeldr_social_publication_schedules WHERE id=?`, sched.ID,
		).Scan(&lastTick); err != nil {
			t.Fatalf("query last_tick_at: %v", err)
		}
		if lastTick.Valid {
			t.Error("processSlotQueue must not run when duePosts fails")
		}
	})

	t.Run("context cancelled skips publish and slot queue advance", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		credID := insertMastodonCredential(t, s, "http://127.0.0.1:1")

		past := time.Now().UTC().Add(-1 * time.Hour)
		for i := 0; i < 2; i++ {
			p := ScheduledPost{
				ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "due post",
				Status: PostStatusScheduled, ScheduledAt: &past,
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			}
			if err := insertPost(db, p); err != nil {
				t.Fatalf("insertPost: %v", err)
			}
		}
		sched := seedSchedule(t, db, "cred-due-cancel", ScheduleStatusActive)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		sc := newScheduler(s)
		sc.processDue(ctx)

		posts, err := listPosts(db, PostStatusScheduled)
		if err != nil {
			t.Fatalf("listPosts: %v", err)
		}
		if len(posts) != 2 {
			t.Errorf("expected both posts to remain scheduled (untouched), got %d", len(posts))
		}

		var lastTick sql.NullTime
		if err := db.QueryRow(
			`SELECT last_tick_at FROM smeldr_social_publication_schedules WHERE id=?`, sched.ID,
		).Scan(&lastTick); err != nil {
			t.Fatalf("query last_tick_at: %v", err)
		}
		if lastTick.Valid {
			t.Error("processSlotQueue's outer loop must bail before updating last_tick_at")
		}
	})

	t.Run("happy path publishes due post and always purges expired oauth state", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"posted-1"}`))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}

		credID := insertMastodonCredential(t, s, srv.URL)
		past := time.Now().UTC().Add(-1 * time.Hour)
		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "hello",
			Status: PostStatusScheduled, ScheduledAt: &past,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		old := time.Now().UTC().Add(-oauthStateTTL - time.Minute)
		if _, err := db.Exec(`
			INSERT INTO smeldr_social_oauth_states (state, platform, code_verifier, created_at)
			VALUES (?, ?, ?, ?)`, "expired-for-processdue", "mastodon", "", old,
		); err != nil {
			t.Fatalf("seed expired oauth state: %v", err)
		}

		sc := newScheduler(s)
		sc.processDue(context.Background())

		got, err := getPost(db, p.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.Status != PostStatusPublished {
			t.Errorf("Status = %q, want published", got.Status)
		}

		var count int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM smeldr_social_oauth_states WHERE state=?`, "expired-for-processdue",
		).Scan(&count); err != nil {
			t.Fatalf("count expired state: %v", err)
		}
		if count != 0 {
			t.Error("expired oauth state should have been purged by processDue")
		}
	})
}

// ─── processSlotQueue ───────────────────────────────────────────────────────

func TestSchedulerProcessSlotQueue(t *testing.T) {
	t.Run("listActiveSchedules error logs and returns", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		sc := newScheduler(s)
		sc.processSlotQueue(context.Background()) // must not panic
	})

	t.Run("updateScheduleLastTick error logs but does not stop the loop", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		sched := seedSchedule(t, db, "cred-tick-err", ScheduleStatusActive)

		// listActiveSchedules is QueryContext (once, overall); the next
		// ExecContext call is updateScheduleLastTick's own UPDATE — fail
		// just that one call so the loop can be observed continuing past it.
		s.db = &nthExecFailDB{DB: db, fail: 1}

		sc := newScheduler(s)
		sc.processSlotQueue(context.Background()) // must not panic

		var lastTick sql.NullTime
		if err := db.QueryRow(
			`SELECT last_tick_at FROM smeldr_social_publication_schedules WHERE id=?`, sched.ID,
		).Scan(&lastTick); err != nil {
			t.Fatalf("query last_tick_at: %v", err)
		}
		if lastTick.Valid {
			t.Error("last_tick_at should be unchanged since its own update failed")
		}
	})

	t.Run("schedule with zero slots is skipped after the last_tick update", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		now := time.Now().UTC()
		sched := PublicationSchedule{
			ID:           smeldr.NewID(),
			CredentialID: "cred-zero-slots",
			Slots:        []Slot{},
			Status:       ScheduleStatusActive,
			CreatedAt:    now.Add(-1 * time.Hour),
			UpdatedAt:    now,
		}
		if err := insertSchedule(db, sched); err != nil {
			t.Fatalf("insertSchedule: %v", err)
		}

		sc := newScheduler(s)
		sc.processSlotQueue(context.Background()) // must not panic on the empty-slots continue

		var lastTick sql.NullTime
		if err := db.QueryRow(
			`SELECT last_tick_at FROM smeldr_social_publication_schedules WHERE id=?`, sched.ID,
		).Scan(&lastTick); err != nil {
			t.Fatalf("query last_tick_at: %v", err)
		}
		if !lastTick.Valid {
			t.Error("last_tick_at should still be updated before the zero-slots continue")
		}
	})

	t.Run("outer ctx cancellation bails before touching any schedule", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		sched := seedSchedule(t, db, "cred-outer-cancel", ScheduleStatusActive)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		sc := newScheduler(s)
		sc.processSlotQueue(ctx)

		var lastTick sql.NullTime
		if err := db.QueryRow(
			`SELECT last_tick_at FROM smeldr_social_publication_schedules WHERE id=?`, sched.ID,
		).Scan(&lastTick); err != nil {
			t.Fatalf("query last_tick_at: %v", err)
		}
		if lastTick.Valid {
			t.Error("outer loop must bail on ctx.Err() before updateScheduleLastTick")
		}
	})

	t.Run("inner ctx cancellation bails before dequeue but after last_tick update", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		credID := "cred-inner-cancel"

		// A schedule whose single slot has already fired many times, with a
		// queued post waiting, so the inner dequeue loop would otherwise run.
		now := time.Now().UTC()
		sched := PublicationSchedule{
			ID:           smeldr.NewID(),
			CredentialID: credID,
			Slots:        []Slot{{Weekday: int(now.Weekday()), Time: "00:00", Timezone: "UTC"}},
			Status:       ScheduleStatusActive,
			LastTickAt:   ptrTime(now.Add(-48 * time.Hour)),
			CreatedAt:    now.Add(-72 * time.Hour),
			UpdatedAt:    now,
		}
		if err := insertSchedule(db, sched); err != nil {
			t.Fatalf("insertSchedule: %v", err)
		}
		post := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "queued",
			Status: PostStatusQueued, CreatedAt: now, UpdatedAt: now,
		}
		if err := insertPost(db, post); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		// after=1: 1st Err() call (outer, before the schedule) returns nil;
		// 2nd Err() call (inner, before the first dequeue) returns Canceled.
		ctx := &ctxErrAfterN{Context: context.Background(), after: 1}

		sc := newScheduler(s)
		sc.processSlotQueue(ctx)

		var lastTick sql.NullTime
		if err := db.QueryRow(
			`SELECT last_tick_at FROM smeldr_social_publication_schedules WHERE id=?`, sched.ID,
		).Scan(&lastTick); err != nil {
			t.Fatalf("query last_tick_at: %v", err)
		}
		if !lastTick.Valid {
			t.Error("last_tick_at should have been updated before the inner ctx check")
		}

		remaining, err := listPosts(db, PostStatusQueued)
		if err != nil {
			t.Fatalf("listPosts: %v", err)
		}
		if len(remaining) != 1 {
			t.Errorf("expected the queued post to remain untouched, got %d remaining", len(remaining))
		}
	})

	t.Run("empty queue breaks cleanly on ErrNotFound", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		credID := "cred-empty-queue"
		now := time.Now().UTC()
		sched := PublicationSchedule{
			ID:           smeldr.NewID(),
			CredentialID: credID,
			Slots:        []Slot{{Weekday: int(now.Weekday()), Time: "00:00", Timezone: "UTC"}},
			Status:       ScheduleStatusActive,
			LastTickAt:   ptrTime(now.Add(-48 * time.Hour)),
			CreatedAt:    now.Add(-72 * time.Hour),
			UpdatedAt:    now,
		}
		if err := insertSchedule(db, sched); err != nil {
			t.Fatalf("insertSchedule: %v", err)
		}
		// No queued posts for credID.

		sc := newScheduler(s)
		sc.processSlotQueue(context.Background()) // must not panic; breaks on ErrNotFound

		var lastTick sql.NullTime
		if err := db.QueryRow(
			`SELECT last_tick_at FROM smeldr_social_publication_schedules WHERE id=?`, sched.ID,
		).Scan(&lastTick); err != nil {
			t.Fatalf("query last_tick_at: %v", err)
		}
		if !lastTick.Valid {
			t.Error("last_tick_at should still be updated even with an empty queue")
		}
	})

	t.Run("other dequeue error logs and breaks", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		credID := "cred-dequeue-err"
		now := time.Now().UTC()
		sched := PublicationSchedule{
			ID:           smeldr.NewID(),
			CredentialID: credID,
			Slots:        []Slot{{Weekday: int(now.Weekday()), Time: "00:00", Timezone: "UTC"}},
			Status:       ScheduleStatusActive,
			LastTickAt:   ptrTime(now.Add(-48 * time.Hour)),
			CreatedAt:    now.Add(-72 * time.Hour),
			UpdatedAt:    now,
		}
		if err := insertSchedule(db, sched); err != nil {
			t.Fatalf("insertSchedule: %v", err)
		}
		post := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "queued",
			Status: PostStatusQueued, CreatedAt: now, UpdatedAt: now,
		}
		if err := insertPost(db, post); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		// processSlotQueue's DB call order per schedule is:
		// listActiveSchedules (QueryContext, once overall) →
		// updateScheduleLastTick (ExecContext) →
		// dequeueOldestQueued (QueryRowContext, 1st call in this flow).
		// Failing the 1st QueryRowContext call isolates dequeueOldestQueued
		// without touching the other two calls.
		s.db = &nthQueryRowFailDB{DB: db, fail: 1}

		sc := newScheduler(s)
		sc.processSlotQueue(context.Background()) // must not panic
	})

	t.Run("happy path dequeues and publishes", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"slot-posted-1"}`))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}

		credID := insertMastodonCredential(t, s, srv.URL)
		now := time.Now().UTC()
		sched := PublicationSchedule{
			ID:           smeldr.NewID(),
			CredentialID: credID,
			Slots:        []Slot{{Weekday: int(now.Weekday()), Time: "00:00", Timezone: "UTC"}},
			Status:       ScheduleStatusActive,
			LastTickAt:   ptrTime(now.Add(-48 * time.Hour)),
			CreatedAt:    now.Add(-72 * time.Hour),
			UpdatedAt:    now,
		}
		if err := insertSchedule(db, sched); err != nil {
			t.Fatalf("insertSchedule: %v", err)
		}
		post := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "queued and due",
			Status: PostStatusQueued, CreatedAt: now, UpdatedAt: now,
		}
		if err := insertPost(db, post); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		sc := newScheduler(s)
		sc.processSlotQueue(context.Background())

		got, err := getPost(db, post.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.Status != PostStatusPublished {
			t.Errorf("Status = %q, want published", got.Status)
		}
	})
}

func ptrTime(t time.Time) *time.Time { return &t }

// ─── publishWithRetry ───────────────────────────────────────────────────────

func TestPublishWithRetry(t *testing.T) {
	t.Run("deliveryAttemptCount error logs and returns", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		credID := insertMastodonCredential(t, s, "http://127.0.0.1:1")
		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusScheduled, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		// deliveryAttemptCount is publishWithRetry's first DB call.
		s.db = &nthQueryRowFailDB{DB: db, fail: 1}

		sc := newScheduler(s)
		sc.publishWithRetry(context.Background(), p)

		got, err := getPost(db, p.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.Status != PostStatusScheduled {
			t.Errorf("Status = %q, want unchanged (scheduled)", got.Status)
		}
	})

	t.Run("credential lookup error marks post failed", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: "no-such-credential", Body: "x",
			Status: PostStatusScheduled, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		sc := newScheduler(s)
		sc.publishWithRetry(context.Background(), p)

		got, err := getPost(db, p.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.Status != PostStatusFailed {
			t.Errorf("Status = %q, want failed", got.Status)
		}
		if got.ErrorMsg == "" {
			t.Error("ErrorMsg should be set")
		}
	})

	t.Run("success publishes and logs delivery attempt", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"retry-success-1"}`))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		credID := insertMastodonCredential(t, s, srv.URL)

		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusScheduled, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		sc := newScheduler(s)
		sc.publishWithRetry(context.Background(), p)

		got, err := getPost(db, p.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.Status != PostStatusPublished {
			t.Errorf("Status = %q, want published", got.Status)
		}
		if got.PlatformPostID != "retry-success-1" {
			t.Errorf("PlatformPostID = %q", got.PlatformPostID)
		}
		attempts, err := deliveryAttemptCount(db, p.ID)
		if err != nil {
			t.Fatalf("deliveryAttemptCount: %v", err)
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1", attempts)
		}
	})

	t.Run("markPostPublished error after a successful publish is only logged", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"retry-markpub-fail-1"}`))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		credID := insertMastodonCredential(t, s, srv.URL)

		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusScheduled, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		// Success path makes exactly two ExecContext calls: logDeliveryAttempt
		// (1st) then markPostPublished (2nd). Fail only the 2nd so the delivery
		// log write still succeeds and we isolate markPostPublished's error.
		s.db = &nthExecFailDB{DB: db, fail: 2}

		sc := newScheduler(s)
		sc.publishWithRetry(context.Background(), p) // must not panic; error is only logged

		attempts, err := deliveryAttemptCount(db, p.ID)
		if err != nil {
			t.Fatalf("deliveryAttemptCount: %v", err)
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1 (delivery log write must have succeeded)", attempts)
		}
	})

	t.Run("terminal publish error marks post failed", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		credID := insertMastodonCredential(t, s, srv.URL)

		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusScheduled, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		sc := newScheduler(s)
		sc.publishWithRetry(context.Background(), p)

		got, err := getPost(db, p.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.Status != PostStatusFailed {
			t.Errorf("Status = %q, want failed (terminal)", got.Status)
		}
	})

	t.Run("transient error reschedules with backoff", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("boom"))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		credID := insertMastodonCredential(t, s, srv.URL)

		original := time.Now().UTC().Add(-1 * time.Hour)
		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusScheduled, ScheduledAt: &original,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		sc := newScheduler(s)
		sc.publishWithRetry(context.Background(), p)

		got, err := getPost(db, p.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.Status != PostStatusScheduled {
			t.Errorf("Status = %q, want scheduled (still retrying)", got.Status)
		}
		if got.ScheduledAt == nil {
			t.Fatal("ScheduledAt should be set")
		}
		wantMin := time.Now().UTC().Add(20 * time.Second)
		wantMax := time.Now().UTC().Add(40 * time.Second)
		if got.ScheduledAt.Before(wantMin) || got.ScheduledAt.After(wantMax) {
			t.Errorf("ScheduledAt = %v, want within [%v, %v] (retryDelays[0]=30s)", got.ScheduledAt, wantMin, wantMax)
		}
	})

	// retryDelays' own idx-clamp (`if idx >= len(retryDelays) { idx =
	// len(retryDelays)-1 }`) is unreachable: it is only evaluated once the
	// terminal check above it (`attempts+1 >= len(retryDelays)+1`, i.e.
	// attempts >= len(retryDelays)) has already returned early. So whenever
	// execution reaches the clamp, attempts — and therefore idx — is
	// necessarily < len(retryDelays), making the clamp's own condition
	// provably always false. Not tested for that reason.

	t.Run("updatePost error in the transient-retry path is only logged", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("boom"))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		credID := insertMastodonCredential(t, s, srv.URL)

		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusScheduled, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		// Transient path makes exactly two ExecContext calls: logDeliveryAttempt
		// (1st) then updatePost (2nd). Fail only the 2nd to isolate updatePost.
		s.db = &nthExecFailDB{DB: db, fail: 2}

		sc := newScheduler(s)
		sc.publishWithRetry(context.Background(), p) // must not panic; error is only logged

		attempts, err := deliveryAttemptCount(db, p.ID)
		if err != nil {
			t.Fatalf("deliveryAttemptCount: %v", err)
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1 (delivery log write must have succeeded)", attempts)
		}
	})

	t.Run("rate limit error uses Retry-After, not default backoff", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "600") // 10 minutes — far from the 30s default
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		credID := insertMastodonCredential(t, s, srv.URL)

		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusScheduled, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		sc := newScheduler(s)
		sc.publishWithRetry(context.Background(), p)

		got, err := getPost(db, p.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.ScheduledAt == nil {
			t.Fatal("ScheduledAt should be set")
		}
		wantMin := time.Now().UTC().Add(9 * time.Minute)
		wantMax := time.Now().UTC().Add(11 * time.Minute)
		if got.ScheduledAt.Before(wantMin) || got.ScheduledAt.After(wantMax) {
			t.Errorf("ScheduledAt = %v, want within [%v, %v] (Retry-After=600s)", got.ScheduledAt, wantMin, wantMax)
		}
	})
}

// ─── publishNow ─────────────────────────────────────────────────────────────

func TestPublishNow(t *testing.T) {
	t.Run("credential lookup error", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: "missing", Body: "x",
			Status: PostStatusDraft, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}
		err := s.publishNow(context.Background(), p)
		if !errors.Is(err, smeldr.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("publish error marks post failed and returns error", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		credID := insertMastodonCredential(t, s, srv.URL)

		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusDraft, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		err := s.publishNow(context.Background(), p)
		if err == nil {
			t.Fatal("expected error")
		}
		got, gErr := getPost(db, p.ID)
		if gErr != nil {
			t.Fatalf("getPost: %v", gErr)
		}
		if got.Status != PostStatusFailed {
			t.Errorf("Status = %q, want failed", got.Status)
		}
	})

	t.Run("markPostPublished error surfaced directly", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"ok-1"}`))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		credID := insertMastodonCredential(t, s, srv.URL)

		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusDraft, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		// After setup (which already made its own Exec calls), the next
		// ExecContext call through s.db is markPostPublished's UPDATE —
		// getCredential and the publish HTTP call make no Exec calls.
		s.db = &nthExecFailDB{DB: db, fail: 1}

		err := s.publishNow(context.Background(), p)
		if err == nil {
			t.Fatal("expected error from markPostPublished")
		}
	})

	t.Run("success", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"now-1"}`))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		credID := insertMastodonCredential(t, s, srv.URL)

		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "x",
			Status: PostStatusDraft, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		if err := s.publishNow(context.Background(), p); err != nil {
			t.Fatalf("publishNow: %v", err)
		}
		got, err := getPost(db, p.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.Status != PostStatusPublished {
			t.Errorf("Status = %q, want published", got.Status)
		}
		if got.PlatformPostID != "now-1" {
			t.Errorf("PlatformPostID = %q", got.PlatformPostID)
		}
	})
}

// ─── callPlatformPublish ────────────────────────────────────────────────────

func TestCallPlatformPublish(t *testing.T) {
	t.Run("mastodon not configured", func(t *testing.T) {
		s, _ := newSchedulerTestSocial(t)
		_, err := s.callPlatformPublish(context.Background(), ScheduledPost{Platform: "mastodon"}, PlatformCredential{})
		assertTerminalNotConfigured(t, err, "Mastodon")
	})

	t.Run("linkedin not configured", func(t *testing.T) {
		s, _ := newSchedulerTestSocial(t)
		_, err := s.callPlatformPublish(context.Background(), ScheduledPost{Platform: "linkedin"}, PlatformCredential{})
		assertTerminalNotConfigured(t, err, "LinkedIn")
	})

	t.Run("x not configured", func(t *testing.T) {
		s, _ := newSchedulerTestSocial(t)
		_, err := s.callPlatformPublish(context.Background(), ScheduledPost{Platform: "x"}, PlatformCredential{})
		assertTerminalNotConfigured(t, err, "X")
	})

	t.Run("unknown platform", func(t *testing.T) {
		s, _ := newSchedulerTestSocial(t)
		_, err := s.callPlatformPublish(context.Background(), ScheduledPost{Platform: "myspace"}, PlatformCredential{})
		if err == nil {
			t.Fatal("expected error")
		}
		var pe *publishError
		if !errors.As(err, &pe) {
			t.Fatalf("expected *publishError, got %T", err)
		}
		if !pe.IsTerminal() {
			t.Error("expected terminal error for unknown platform")
		}
	})

	t.Run("mastodon working", func(t *testing.T) {
		s, _ := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"cpp-mastodon-1"}`))
		}))
		defer srv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}

		id, err := s.callPlatformPublish(context.Background(), ScheduledPost{Platform: "mastodon", Body: "hi"}, PlatformCredential{})
		if err != nil {
			t.Fatalf("callPlatformPublish: %v", err)
		}
		if id != "cpp-mastodon-1" {
			t.Errorf("id = %q", id)
		}
	})

	t.Run("linkedin working", func(t *testing.T) {
		s, _ := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"urn:li:share:cpp-1"}`))
		}))
		defer srv.Close()
		s.linkedin = &linkedinClient{
			cfg: LinkedInConfig{},
			hc:  &http.Client{Transport: rewriteTransport{base: srv.URL, inner: http.DefaultTransport}},
		}

		id, err := s.callPlatformPublish(context.Background(),
			ScheduledPost{Platform: "linkedin", Body: "hi"},
			PlatformCredential{ActorID: "urn:li:person:123"},
		)
		if err != nil {
			t.Fatalf("callPlatformPublish: %v", err)
		}
		if id != "urn:li:share:cpp-1" {
			t.Errorf("id = %q", id)
		}
	})

	t.Run("x working", func(t *testing.T) {
		s, _ := newSchedulerTestSocial(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"data":{"id":"cpp-x-1","text":"hi"}}`))
		}))
		defer srv.Close()
		s.twitter = &twitterClient{
			cfg:        xConfig{ClientID: "cid", ClientSecret: "csec"},
			httpClient: &http.Client{Transport: rewriteTransport{base: srv.URL, inner: http.DefaultTransport}},
		}

		id, err := s.callPlatformPublish(context.Background(), ScheduledPost{Platform: "x", Body: "hi"}, PlatformCredential{})
		if err != nil {
			t.Fatalf("callPlatformPublish: %v", err)
		}
		if id != "cpp-x-1" {
			t.Errorf("id = %q", id)
		}
	})
}

func assertTerminalNotConfigured(t *testing.T, err error, platform string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *publishError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *publishError, got %T: %v", err, err)
	}
	if !pe.IsTerminal() {
		t.Errorf("expected terminal error for unconfigured %s", platform)
	}
}

// ─── nextInterval ───────────────────────────────────────────────────────────

func TestSchedulerNextInterval(t *testing.T) {
	t.Run("DB error falls back", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_posts"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		sc := newScheduler(s)
		got := sc.nextInterval()
		if got != schedulerFallbackInterval {
			t.Errorf("got %v, want fallback %v", got, schedulerFallbackInterval)
		}
	})

	t.Run("no scheduled posts falls back", func(t *testing.T) {
		s, _ := newSchedulerTestSocial(t)
		sc := newScheduler(s)
		got := sc.nextInterval()
		if got != schedulerFallbackInterval {
			t.Errorf("got %v, want fallback %v", got, schedulerFallbackInterval)
		}
	})

	// nextScheduledAt (post.go) runs `SELECT MIN(scheduled_at) FROM ...`.
	// With modernc.org/sqlite, an aggregate MIN() over a DATETIME column
	// loses the column's type affinity and comes back as a plain string,
	// which fails to Scan into the sql.NullTime nextScheduledAt uses —
	// confirmed directly while writing this test (see the task report).
	// In production this means nextScheduledAt errors, and nextInterval()
	// falls back to schedulerFallbackInterval, on *every* call where a
	// scheduled post actually exists — not only when the DB is unusable.
	// This test documents that real, currently-shipped behavior rather
	// than the originally-intended "returns time.Until" behavior, since
	// fixing nextScheduledAt (a post.go, not scheduler.go, function) is a
	// production-file change out of scope for this coverage-only task.
	t.Run("existing scheduled post currently still falls back (nextScheduledAt MIN() scan bug)", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		credID := insertMastodonCredential(t, s, "http://127.0.0.1:1")
		future := time.Now().UTC().Add(10 * time.Second)
		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "soon",
			Status: PostStatusScheduled, ScheduledAt: &future,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}

		sc := newScheduler(s)
		got := sc.nextInterval()
		if got != schedulerFallbackInterval {
			t.Errorf("got %v, want fallback %v (see MIN() scan bug note above)", got, schedulerFallbackInterval)
		}
	})

	// The two tests below exercise nextInterval()'s own d>0 / d<=0 /
	// d>fallback arithmetic — logic scheduler.go itself owns — by routing
	// nextScheduledAt's query through minScanBugWorkaroundDB, which rewrites
	// the exact MIN(...) query to an equivalent ORDER BY ... LIMIT 1 query
	// against the same real DB (sidestepping the driver bug documented
	// above without touching any production file). This is the only way to
	// reach these branches at all with the current driver.

	t.Run("upcoming post returns time.Until", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		credID := insertMastodonCredential(t, s, "http://127.0.0.1:1")
		future := time.Now().UTC().Add(10 * time.Second)
		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "soon",
			Status: PostStatusScheduled, ScheduledAt: &future,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}
		s.db = minScanBugWorkaroundDB{DB: db}

		sc := newScheduler(s)
		got := sc.nextInterval()
		if got <= 0 || got > 10*time.Second {
			t.Errorf("got %v, want roughly up to 10s", got)
		}
	})

	t.Run("past due post returns zero", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		credID := insertMastodonCredential(t, s, "http://127.0.0.1:1")
		past := time.Now().UTC().Add(-10 * time.Second)
		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "overdue",
			Status: PostStatusScheduled, ScheduledAt: &past,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}
		s.db = minScanBugWorkaroundDB{DB: db}

		sc := newScheduler(s)
		got := sc.nextInterval()
		if got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})

	t.Run("far future post is capped at the fallback interval", func(t *testing.T) {
		s, db := newSchedulerTestSocial(t)
		credID := insertMastodonCredential(t, s, "http://127.0.0.1:1")
		farFuture := time.Now().UTC().Add(1 * time.Hour) // > schedulerFallbackInterval (60s)
		p := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "far off",
			Status: PostStatusScheduled, ScheduledAt: &farFuture,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost: %v", err)
		}
		s.db = minScanBugWorkaroundDB{DB: db}

		sc := newScheduler(s)
		got := sc.nextInterval()
		if got != schedulerFallbackInterval {
			t.Errorf("got %v, want capped fallback %v", got, schedulerFallbackInterval)
		}
	})
}

// minScanBugWorkaroundDB is used only by the nextInterval tests above — see
// the comment there for why it exists.
type minScanBugWorkaroundDB struct {
	smeldr.DB
}

func (d minScanBugWorkaroundDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	if strings.Contains(q, "MIN(scheduled_at)") {
		return d.DB.QueryRowContext(ctx, `
			SELECT scheduled_at FROM smeldr_social_posts
			WHERE status='scheduled' AND scheduled_at IS NOT NULL
			ORDER BY scheduled_at ASC LIMIT 1`)
	}
	return d.DB.QueryRowContext(ctx, q, args...)
}

// ─── run ─────────────────────────────────────────────────────────────────

// TestSchedulerRun_ProcessesDuePost exercises run()'s own loop body —
// specifically the call site `sc.processDue(ctx)` inside run(), which is
// otherwise never reached: every other test in this file calls processDue
// directly. Reaching it through run() requires nextInterval() to return a
// short sleep, which (see the nextInterval tests above) requires routing
// around the documented MIN() scan bug via minScanBugWorkaroundDB — with the
// real production nextScheduledAt, run() always sleeps a full
// schedulerFallbackInterval (60s) before its first processDue call, which
// would make this test impractically slow.
func TestSchedulerRun_ProcessesDuePost(t *testing.T) {
	s, db := newSchedulerTestSocial(t)
	// run()'s own goroutine and this test's polling loop below query the DB
	// concurrently. modernc.org/sqlite's ":memory:" DSN gives each new
	// connection its own separate in-process database, so without pinning
	// the pool to a single connection a second connection could see a
	// database with no tables at all — the same issue route_worker_test.go's
	// TestRouter_RealWorkerDeliversAndStops documents and works around.
	db.SetMaxOpenConns(1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"run-loop-1"}`))
	}))
	defer srv.Close()
	s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
	credID := insertMastodonCredential(t, s, srv.URL)

	past := time.Now().UTC().Add(-1 * time.Hour)
	p := ScheduledPost{
		ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "due via run()",
		Status: PostStatusScheduled, ScheduledAt: &past,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := insertPost(db, p); err != nil {
		t.Fatalf("insertPost: %v", err)
	}
	s.db = minScanBugWorkaroundDB{DB: db}

	sc := newScheduler(s)
	sc.start()
	defer sc.stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := getPost(db, p.ID)
		if err != nil {
			t.Fatalf("getPost: %v", err)
		}
		if got.Status == PostStatusPublished {
			return // run() reached processDue and published the post
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run() did not publish the due post within 5 seconds")
}

// ─── maybeRefreshXCredential — remaining branches ──────────────────────────
//
// Most of maybeRefreshXCredential's branches are already covered by
// scheduler_refresh_test.go's TestMaybeRefreshXCredential_* suite (same
// package, same file's rewriteTransport/insertXCredential/tokenHandler
// helpers reused below). These two are not: the twitter client being nil,
// and the token-persist failure path.

func TestMaybeRefreshXCredential_TwitterClientNil(t *testing.T) {
	s, _ := newSchedulerTestSocial(t)
	// s.twitter is nil — New() with no X platform config leaves it unset.
	expiry := time.Now().Add(1 * time.Minute)
	cred := PlatformCredential{
		ID: "cred-nil-tc", Platform: "x",
		accessToken: "old-access", refreshToken: "old-refresh", ExpiresAt: &expiry,
	}
	s.maybeRefreshXCredential(context.Background(), &cred)
	if cred.accessToken != "old-access" {
		t.Error("access token must not change when twitter client is nil")
	}
}

func TestMaybeRefreshXCredential_PersistFails(t *testing.T) {
	newTokenResp := xTokenResponse{
		AccessToken:  "persist-fail-new-access",
		RefreshToken: "persist-fail-new-refresh",
		ExpiresIn:    7200,
	}
	srv := httptest.NewServer(tokenHandler(newTokenResp, http.StatusOK))
	defer srv.Close()

	tc := &twitterClient{
		cfg:        xConfig{ClientID: "cid", ClientSecret: "csec"},
		httpClient: &http.Client{Transport: rewriteTransport{base: srv.URL, inner: http.DefaultTransport}},
	}

	s, db := newSchedulerTestSocial(t)
	s.mu.Lock()
	s.twitter = tc
	s.mu.Unlock()

	expiry := time.Now().Add(1 * time.Minute)
	id := insertXCredential(t, s, "old-access", "old-refresh", &expiry)
	cred, err := s.creds.getCredential(id)
	if err != nil {
		t.Fatalf("getCredential: %v", err)
	}

	if _, err := db.Exec("DROP TABLE smeldr_social_credentials"); err != nil {
		t.Fatalf("drop table: %v", err)
	}

	s.maybeRefreshXCredential(context.Background(), &cred) // must not panic

	if cred.accessToken != "old-access" {
		t.Errorf("access token must be unchanged after persist failure, got %q", cred.accessToken)
	}
}

// ─── start / stop ───────────────────────────────────────────────────────────

func TestSchedulerStartStop(t *testing.T) {
	s, _ := newSchedulerTestSocial(t)
	sc := newScheduler(s)
	sc.start()

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.stop()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop() did not return within 5 seconds")
	}
}

func TestSchedulerStop_NeverStarted(t *testing.T) {
	s, _ := newSchedulerTestSocial(t)
	sc := newScheduler(s)
	sc.stop() // must be a no-op, must not hang or panic
}

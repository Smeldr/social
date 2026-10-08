//go:build integration

package social

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"smeldr.dev/core"
)

// pgSchemaDB gives the test a Postgres schema of its own (search_path),
// dropped on cleanup. Needs DATABASE_URL and the integration build tag.
func pgSchemaDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	schema := "t_" + strings.ReplaceAll(strings.ToLower(smeldr.NewID()), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("pgx", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("open in schema: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return db
}

const pgSecret = "test-secret-32-bytes-long-padded!"

// Every social store on Postgres, with the foreign keys Postgres enforces.
func TestPG_SocialStores(t *testing.T) {
	db := pgSchemaDB(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := CreateTables(db); err != nil {
			t.Fatalf("CreateTables call %d: %v", i+1, err)
		}
	}

	// Credentials.
	cs := newCredentialStore(db, []byte(pgSecret))
	exp := time.Now().UTC().Add(time.Hour)
	credID, err := cs.upsertCredentialByInstance("mastodon", "https://m.example", "me", "tok", "ref", "actor-1", &exp)
	if err != nil {
		t.Fatalf("upsert credential: %v", err)
	}
	if again, err := cs.upsertCredentialByInstance("mastodon", "https://m.example", "me2", "tok2", "", "actor-1", nil); err != nil || again != credID {
		t.Fatalf("second upsert = %q, %v; want the same id", again, err)
	}
	if c, err := cs.getCredential(credID); err != nil || c.Name != "me2" {
		t.Fatalf("getCredential = %+v, %v", c, err)
	}
	if list, err := cs.listCredentials(); err != nil || len(list) != 1 {
		t.Fatalf("listCredentials = %d, %v", len(list), err)
	}

	// Posts, including the IN list and the due query.
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	posts := []ScheduledPost{
		{ID: "p1", Platform: "mastodon", CredentialID: credID, Body: "a", Status: PostStatusScheduled, ScheduledAt: &past, CreatedAt: now, UpdatedAt: now},
		{ID: "p2", Platform: "mastodon", CredentialID: credID, Body: "b", Status: PostStatusDraft, CreatedAt: now, UpdatedAt: now},
		{ID: "p3", Platform: "mastodon", CredentialID: credID, Body: "c", Status: PostStatusQueued, CreatedAt: now, UpdatedAt: now},
	}
	for _, p := range posts {
		if err := insertPost(db, p); err != nil {
			t.Fatalf("insertPost %s: %v", p.ID, err)
		}
	}
	if got, err := listPosts(db, PostStatusScheduled, PostStatusDraft); err != nil || len(got) != 2 {
		t.Fatalf("listPosts(scheduled, draft) = %d, %v", len(got), err)
	}
	if due, err := duePosts(db); err != nil || len(due) != 1 || due[0].ID != "p1" {
		t.Fatalf("duePosts = %+v, %v", due, err)
	}
	if next, err := nextScheduledAt(db); err != nil || next == nil {
		t.Fatalf("nextScheduledAt = %v, %v", next, err)
	}
	p2 := posts[1]
	p2.Body = "edited"
	if err := updatePost(db, p2); err != nil {
		t.Fatal(err)
	}
	if err := markPostPublished(db, "p1", "remote-1"); err != nil {
		t.Fatal(err)
	}
	if err := markPostFailed(db, "p2", "nope"); err != nil {
		t.Fatal(err)
	}
	if got, err := getPost(db, "p2"); err != nil || got.Body != "edited" || got.Status != PostStatusFailed {
		t.Fatalf("getPost = %+v, %v", got, err)
	}
	if err := logDeliveryAttempt(db, "p1", 1, 200, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := deliveryAttemptCount(db, "p1"); err != nil || n != 1 {
		t.Fatalf("deliveryAttemptCount = %d, %v", n, err)
	}

	// Schedules.
	s := PublicationSchedule{ID: "s1", CredentialID: credID, Slots: []Slot{{Weekday: 1, Time: "09:00", Timezone: "UTC"}},
		Status: ScheduleStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := insertSchedule(db, s); err != nil {
		t.Fatalf("insertSchedule: %v", err)
	}
	s.Status = ScheduleStatusPaused
	if err := updateSchedule(db, s); err != nil {
		t.Fatal(err)
	}
	if err := updateScheduleLastTick(db, "s1", now); err != nil {
		t.Fatal(err)
	}
	if got, err := getSchedule(db, "s1"); err != nil || got.Status != ScheduleStatusPaused || got.LastTickAt == nil {
		t.Fatalf("getSchedule = %+v, %v", got, err)
	}
	if list, err := listSchedules(db); err != nil || len(list) != 1 {
		t.Fatalf("listSchedules = %d, %v", len(list), err)
	}
	if active, err := listActiveSchedules(db); err != nil || len(active) != 0 {
		t.Fatalf("listActiveSchedules = %d, %v", len(active), err)
	}
	if q, err := dequeueOldestQueued(db, credID); err != nil || q.ID != "p3" {
		t.Fatalf("dequeueOldestQueued = %+v, %v", q, err)
	}
	if err := deleteSchedule(db, "s1"); err != nil {
		t.Fatal(err)
	}

	// OAuth states.
	if err := insertOAuthState(db, "st1", "x", "verifier"); err != nil {
		t.Fatalf("insertOAuthState: %v", err)
	}
	if platform, verifier, err := consumeOAuthState(db, "st1"); err != nil || platform != "x" || verifier != "verifier" {
		t.Fatalf("consumeOAuthState = %q %q %v", platform, verifier, err)
	}
	if err := purgeExpiredOAuthStates(db); err != nil {
		t.Fatal(err)
	}

	// Platform config: upsert twice.
	pcs := newPlatformConfigStore(db, []byte(pgSecret))
	for _, id := range []string{"a", "b"} {
		if err := pcs.save("mastodon", PlatformConfig{ClientID: id}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if cfg, ok, err := pcs.load("mastodon"); err != nil || !ok || cfg.ClientID != "b" {
		t.Fatalf("load = %+v %v %v", cfg, ok, err)
	}

	// Route jobs.
	rs := &routeJobStore{db: db}
	rs.enqueue(Route{Signal: smeldr.AfterPublish, ContentType: "Post", AgentURL: "https://agent.example"}, smeldr.AfterPublish, smeldr.SignalEvent{})
	jobs, err := rs.dueJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("dueJobs = %d, %v", len(jobs), err)
	}
	rs.logAttempt(ctx, jobs[0].ID, 1, 500, "down")
	if err := rs.scheduleRetry(ctx, jobs[0].ID, 1, time.Now().UTC().Add(time.Hour), "down"); err != nil {
		t.Fatal(err)
	}
	if err := rs.markFailed(ctx, jobs[0].ID, "gave up"); err != nil {
		t.Fatal(err)
	}
	if err := rs.markDelivered(ctx, jobs[0].ID); err != nil {
		t.Fatal(err)
	}

	// Foreign keys: the credential is refused while posts use it; a post goes
	// with its delivery log.
	if err := cs.deleteCredential(credID); !errors.Is(err, smeldr.ErrConflict) {
		t.Fatalf("deleteCredential with posts = %v; want ErrConflict", err)
	}
	for _, id := range []string{"p1", "p2", "p3"} {
		if err := deletePost(db, id); err != nil {
			t.Fatalf("deletePost %s: %v", id, err)
		}
	}
	if err := cs.deleteCredential(credID); err != nil {
		t.Fatalf("deleteCredential after the posts: %v", err)
	}
}

// Legacy forge_social_* tables are renamed on Postgres, rows and all.
func TestPG_SocialLegacyRename(t *testing.T) {
	db := pgSchemaDB(t)
	if _, err := db.Exec(`CREATE TABLE forge_social_platform_config (platform TEXT PRIMARY KEY, config TEXT NOT NULL, updated_at TIMESTAMP NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO forge_social_platform_config VALUES ('x', 'enc', now())`); err != nil {
		t.Fatal(err)
	}
	if err := CreateTables(db); err != nil {
		t.Fatalf("CreateTables: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM smeldr_social_platform_config`).Scan(&n); err != nil || n != 1 {
		t.Errorf("renamed rows = %d (%v); want 1", n, err)
	}
}

package social_test

import (
	"database/sql"
	"testing"
	"time"

	social "smeldr.dev/social"
)

// — orderClauseForStatuses (t143) ————————————————————————————————————————————

func TestOrderClauseForStatuses(t *testing.T) {
	const descClause = " ORDER BY created_at DESC"
	const soonestClause = " ORDER BY scheduled_at IS NULL, scheduled_at ASC, created_at ASC"

	cases := []struct {
		name     string
		statuses []social.PostStatus
		want     string
	}{
		{"empty", nil, descClause},
		{"scheduled only", []social.PostStatus{social.PostStatusScheduled}, soonestClause},
		{"queued only", []social.PostStatus{social.PostStatusQueued}, soonestClause},
		{"scheduled and queued", []social.PostStatus{social.PostStatusScheduled, social.PostStatusQueued}, soonestClause},
		{"draft only", []social.PostStatus{social.PostStatusDraft}, descClause},
		{"mixed scheduled and draft", []social.PostStatus{social.PostStatusScheduled, social.PostStatusDraft}, descClause},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := social.OrderClauseForStatusesForTest(tc.statuses)
			if got != tc.want {
				t.Errorf("orderClauseForStatuses(%v) = %q, want %q", tc.statuses, got, tc.want)
			}
		})
	}
}

// — listPosts ordering (t143) ————————————————————————————————————————————————

func seedPost(t *testing.T, db *sql.DB, id string, status social.PostStatus, scheduledAt *time.Time, createdAt time.Time) {
	t.Helper()
	p := social.ScheduledPost{
		ID:          id,
		Platform:    "mastodon",
		Body:        "test",
		Status:      status,
		ScheduledAt: scheduledAt,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
	if err := social.InsertPostForTest(db, p); err != nil {
		t.Fatalf("InsertPostForTest(%s): %v", id, err)
	}
}

func TestListPosts_ScheduledQueued_SoonestFirst(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := base.Add(2 * time.Hour)
	middle := base.Add(1 * time.Hour)

	seedPost(t, db, "p-later", social.PostStatusScheduled, &later, base)
	seedPost(t, db, "p-base", social.PostStatusScheduled, &base, base)
	seedPost(t, db, "p-middle", social.PostStatusScheduled, &middle, base)
	seedPost(t, db, "p-queued", social.PostStatusQueued, nil, base.Add(30*time.Minute))

	posts, err := social.ListPostsForTest(db, social.PostStatusScheduled, social.PostStatusQueued)
	if err != nil {
		t.Fatalf("ListPostsForTest: %v", err)
	}
	if len(posts) != 4 {
		t.Fatalf("got %d posts, want 4", len(posts))
	}
	wantOrder := []string{"p-base", "p-middle", "p-later", "p-queued"}
	for i, want := range wantOrder {
		if posts[i].ID != want {
			t.Errorf("posts[%d].ID = %q, want %q (order: %v)", i, posts[i].ID, want, postIDs(posts))
		}
	}
}

func TestListPosts_NoFilter_NewestFirst(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedPost(t, db, "p-old", social.PostStatusPublished, nil, base)
	seedPost(t, db, "p-new", social.PostStatusPublished, nil, base.Add(time.Hour))

	posts, err := social.ListPostsForTest(db, social.PostStatusPublished)
	if err != nil {
		t.Fatalf("ListPostsForTest: %v", err)
	}
	if len(posts) != 2 || posts[0].ID != "p-new" || posts[1].ID != "p-old" {
		t.Errorf("order = %v, want [p-new p-old] (created_at DESC unchanged)", postIDs(posts))
	}
}

func postIDs(posts []social.ScheduledPost) []string {
	ids := make([]string, len(posts))
	for i, p := range posts {
		ids[i] = p.ID
	}
	return ids
}

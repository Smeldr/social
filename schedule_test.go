package social

// schedule_test.go covers schedule.go's DB helpers (insertSchedule,
// updateSchedule, updateScheduleLastTick, getSchedule, listSchedules,
// listActiveSchedules, deleteSchedule, dequeueOldestQueued), its pure
// slot-firing logic (firedSlotsBetween), and its scan helpers (scanSchedule,
// scanScheduleRow — exercised indirectly through getSchedule/listSchedules).
//
// Two error paths are intentionally left untested:
//   - rows.Err() (the terminal error from Rows iteration itself, distinct
//     from a per-row Scan error): modernc.org/sqlite does not expose a way
//     to make Rows.Next() itself fail after successful rows without also
//     failing an earlier Scan, so forcing it would require a custom driver —
//     disproportionate effort for one branch already exercised by the
//     scan-error tests below via the same code path.
//   - insertSchedule's and updateSchedule's json.Marshal(s.Slots) error
//     branch: Slot is built only from a string, an int, and a string, so
//     json.Marshal cannot fail for it — the same reasoning mastodon_test.go
//     documents for postStatus's json.Marshal call. The branch is
//     unreachable via the public API.

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"smeldr.dev/core"
)

// ─── GetSlug ────────────────────────────────────────────────────────────────

func TestPublicationSchedule_GetSlug(t *testing.T) {
	ps := PublicationSchedule{ID: "sched-1"}
	if got := ps.GetSlug(); got != "sched-1" {
		t.Errorf("GetSlug() = %q, want %q", got, "sched-1")
	}
}

// ─── insertSchedule ────────────────────────────────────────────────────────

func TestInsertSchedule(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db := openRefreshTestDB(t)
		now := time.Now().UTC()
		s := PublicationSchedule{
			ID:           smeldr.NewID(),
			CredentialID: "cred-insert-1",
			Slots:        []Slot{{Weekday: 1, Time: "09:00", Timezone: "Europe/Copenhagen"}},
			Status:       ScheduleStatusActive,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := insertSchedule(db, s); err != nil {
			t.Fatalf("insertSchedule: %v", err)
		}
		got, err := getSchedule(db, s.ID)
		if err != nil {
			t.Fatalf("getSchedule: %v", err)
		}
		if got.CredentialID != s.CredentialID {
			t.Errorf("CredentialID = %q, want %q", got.CredentialID, s.CredentialID)
		}
		if len(got.Slots) != 1 {
			t.Fatalf("Slots len = %d, want 1", len(got.Slots))
		}
	})

	t.Run("exec error", func(t *testing.T) {
		db := openRefreshTestDB(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		now := time.Now().UTC()
		s := PublicationSchedule{ID: smeldr.NewID(), CredentialID: "cred-x", CreatedAt: now, UpdatedAt: now}
		if err := insertSchedule(db, s); err == nil {
			t.Fatal("expected error")
		}
	})
}

// ─── updateSchedule ────────────────────────────────────────────────────────

func TestUpdateSchedule(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db := openRefreshTestDB(t)
		now := time.Now().UTC()
		s := PublicationSchedule{
			ID:           smeldr.NewID(),
			CredentialID: "cred-update-1",
			Slots:        []Slot{{Weekday: 1, Time: "09:00", Timezone: "UTC"}},
			Status:       ScheduleStatusActive,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := insertSchedule(db, s); err != nil {
			t.Fatalf("insertSchedule: %v", err)
		}

		s.Slots = []Slot{{Weekday: 2, Time: "10:30", Timezone: "UTC"}}
		s.Status = ScheduleStatusPaused
		if err := updateSchedule(db, s); err != nil {
			t.Fatalf("updateSchedule: %v", err)
		}

		got, err := getSchedule(db, s.ID)
		if err != nil {
			t.Fatalf("getSchedule: %v", err)
		}
		if got.Status != ScheduleStatusPaused {
			t.Errorf("Status = %q, want %q", got.Status, ScheduleStatusPaused)
		}
		if len(got.Slots) != 1 || got.Slots[0].Weekday != 2 {
			t.Errorf("Slots = %+v", got.Slots)
		}
	})

	t.Run("exec error", func(t *testing.T) {
		db := openRefreshTestDB(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		s := PublicationSchedule{ID: "nonexistent", Slots: []Slot{}}
		if err := updateSchedule(db, s); err == nil {
			t.Fatal("expected error")
		}
	})
}

// ─── updateScheduleLastTick ────────────────────────────────────────────────

func TestUpdateScheduleLastTick(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db := openRefreshTestDB(t)
		now := time.Now().UTC()
		s := PublicationSchedule{ID: smeldr.NewID(), CredentialID: "cred-tick-1", CreatedAt: now, UpdatedAt: now}
		if err := insertSchedule(db, s); err != nil {
			t.Fatalf("insertSchedule: %v", err)
		}
		tick := now.Add(1 * time.Hour)
		if err := updateScheduleLastTick(db, s.ID, tick); err != nil {
			t.Fatalf("updateScheduleLastTick: %v", err)
		}
		got, err := getSchedule(db, s.ID)
		if err != nil {
			t.Fatalf("getSchedule: %v", err)
		}
		if got.LastTickAt == nil {
			t.Fatal("LastTickAt is nil")
		}
		if !got.LastTickAt.Equal(tick) {
			t.Errorf("LastTickAt = %v, want %v", got.LastTickAt, tick)
		}
	})

	t.Run("exec error", func(t *testing.T) {
		db := openRefreshTestDB(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		if err := updateScheduleLastTick(db, "nonexistent", time.Now()); err == nil {
			t.Fatal("expected error")
		}
	})
}

// ─── listSchedules / listActiveSchedules ───────────────────────────────────

func seedSchedule(t *testing.T, db *sql.DB, credentialID string, status ScheduleStatus) PublicationSchedule {
	t.Helper()
	now := time.Now().UTC()
	s := PublicationSchedule{
		ID:           smeldr.NewID(),
		CredentialID: credentialID,
		Slots:        []Slot{{Weekday: 1, Time: "09:00", Timezone: "UTC"}},
		Status:       status,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := insertSchedule(db, s); err != nil {
		t.Fatalf("seedSchedule insertSchedule: %v", err)
	}
	return s
}

func TestListSchedules(t *testing.T) {
	t.Run("success with multiple rows", func(t *testing.T) {
		db := openRefreshTestDB(t)
		seedSchedule(t, db, "cred-list-1", ScheduleStatusActive)
		seedSchedule(t, db, "cred-list-2", ScheduleStatusPaused)

		got, err := listSchedules(db)
		if err != nil {
			t.Fatalf("listSchedules: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
	})

	t.Run("query error", func(t *testing.T) {
		db := openRefreshTestDB(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		if _, err := listSchedules(db); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("scan error via malformed slots JSON", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := seedSchedule(t, db, "cred-list-bad", ScheduleStatusActive)
		if _, err := db.Exec(
			`UPDATE smeldr_social_publication_schedules SET slots='not-json' WHERE id=?`, s.ID,
		); err != nil {
			t.Fatalf("corrupt slots: %v", err)
		}
		if _, err := listSchedules(db); err == nil {
			t.Fatal("expected error from malformed slots JSON")
		}
	})

	t.Run("rows.Scan itself fails on a malformed DATETIME column", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := seedSchedule(t, db, "cred-list-baddate", ScheduleStatusActive)
		if _, err := db.Exec(
			`UPDATE smeldr_social_publication_schedules SET last_tick_at='not-a-date' WHERE id=?`, s.ID,
		); err != nil {
			t.Fatalf("corrupt last_tick_at: %v", err)
		}
		_, err := listSchedules(db)
		if err == nil {
			t.Fatal("expected a genuine rows.Scan error")
		}
		if strings.Contains(err.Error(), "unmarshal slots") {
			t.Errorf("expected a Scan-level error, not the unmarshal-slots error: %v", err)
		}
	})

	t.Run("nil slots JSON column becomes an empty non-nil slice", func(t *testing.T) {
		db := openRefreshTestDB(t)
		// insertSchedule marshals a nil []Slot as JSON "null"; scanScheduleRow
		// must normalize that back to an empty, non-nil slice.
		now := time.Now().UTC()
		s := PublicationSchedule{
			ID: smeldr.NewID(), CredentialID: "cred-list-nilslots", Slots: nil,
			Status: ScheduleStatusActive, CreatedAt: now, UpdatedAt: now,
		}
		if err := insertSchedule(db, s); err != nil {
			t.Fatalf("insertSchedule: %v", err)
		}

		got, err := listSchedules(db)
		if err != nil {
			t.Fatalf("listSchedules: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0].Slots == nil {
			t.Error("Slots should be normalized to an empty non-nil slice, not nil")
		}
		if len(got[0].Slots) != 0 {
			t.Errorf("Slots = %+v, want empty", got[0].Slots)
		}
	})
}

func TestListActiveSchedules(t *testing.T) {
	t.Run("returns only active", func(t *testing.T) {
		db := openRefreshTestDB(t)
		seedSchedule(t, db, "cred-active-1", ScheduleStatusActive)
		seedSchedule(t, db, "cred-active-2", ScheduleStatusPaused)
		seedSchedule(t, db, "cred-active-3", ScheduleStatusActive)

		got, err := listActiveSchedules(db)
		if err != nil {
			t.Fatalf("listActiveSchedules: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		for _, s := range got {
			if s.Status != ScheduleStatusActive {
				t.Errorf("unexpected status %q in active list", s.Status)
			}
		}
	})

	t.Run("query error", func(t *testing.T) {
		db := openRefreshTestDB(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		if _, err := listActiveSchedules(db); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("scan error via malformed slots JSON", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := seedSchedule(t, db, "cred-active-bad", ScheduleStatusActive)
		if _, err := db.Exec(
			`UPDATE smeldr_social_publication_schedules SET slots='not-json' WHERE id=?`, s.ID,
		); err != nil {
			t.Fatalf("corrupt slots: %v", err)
		}
		if _, err := listActiveSchedules(db); err == nil {
			t.Fatal("expected error from malformed slots JSON")
		}
	})
}

// ─── deleteSchedule ─────────────────────────────────────────────────────────

func TestDeleteSchedule(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := seedSchedule(t, db, "cred-delete-1", ScheduleStatusActive)
		if err := deleteSchedule(db, s.ID); err != nil {
			t.Fatalf("deleteSchedule: %v", err)
		}
		if _, err := getSchedule(db, s.ID); !errors.Is(err, smeldr.ErrNotFound) {
			t.Errorf("getSchedule after delete: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("exec error", func(t *testing.T) {
		db := openRefreshTestDB(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		if err := deleteSchedule(db, "any-id"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("not found", func(t *testing.T) {
		db := openRefreshTestDB(t)
		if err := deleteSchedule(db, "does-not-exist"); !errors.Is(err, smeldr.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// ─── dequeueOldestQueued ────────────────────────────────────────────────────

func TestDequeueOldestQueued(t *testing.T) {
	t.Run("success returns oldest", func(t *testing.T) {
		db := openRefreshTestDB(t)
		credID := "cred-dequeue-1"
		olderScheduledAt := time.Now().UTC().Add(2 * time.Hour)
		older := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "older",
			Status: PostStatusQueued, ScheduledAt: &olderScheduledAt,
			CreatedAt: time.Now().UTC().Add(-1 * time.Hour), UpdatedAt: time.Now().UTC(),
		}
		newer := ScheduledPost{
			ID: smeldr.NewID(), Platform: "mastodon", CredentialID: credID, Body: "newer",
			Status: PostStatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := insertPost(db, newer); err != nil {
			t.Fatalf("insertPost newer: %v", err)
		}
		if err := insertPost(db, older); err != nil {
			t.Fatalf("insertPost older: %v", err)
		}

		got, err := dequeueOldestQueued(db, credID)
		if err != nil {
			t.Fatalf("dequeueOldestQueued: %v", err)
		}
		if got.ID != older.ID {
			t.Errorf("got ID %q, want oldest %q", got.ID, older.ID)
		}
		if got.ScheduledAt == nil || !got.ScheduledAt.Equal(olderScheduledAt) {
			t.Errorf("ScheduledAt = %v, want %v", got.ScheduledAt, olderScheduledAt)
		}
	})

	t.Run("empty queue returns ErrNotFound", func(t *testing.T) {
		db := openRefreshTestDB(t)
		_, err := dequeueOldestQueued(db, "cred-empty")
		if !errors.Is(err, smeldr.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("scan error", func(t *testing.T) {
		db := openRefreshTestDB(t)
		credID := "cred-dequeue-bad"
		now := time.Now().UTC()
		// Seed a queued row with a malformed scheduled_at value directly,
		// bypassing insertPost's *time.Time typing, so Scan fails.
		if _, err := db.Exec(`
			INSERT INTO smeldr_social_posts
				(id, platform, credential_id, body, media_url, alt_text,
				 scheduled_at, status, platform_post_id, error_msg, created_at, updated_at)
			VALUES (?, ?, ?, ?, '', '', 'not-a-date', 'queued', '', '', ?, ?)`,
			smeldr.NewID(), "mastodon", credID, "bad row", now, now,
		); err != nil {
			t.Fatalf("seed malformed row: %v", err)
		}
		_, err := dequeueOldestQueued(db, credID)
		if err == nil {
			t.Fatal("expected error")
		}
		if errors.Is(err, smeldr.ErrNotFound) {
			t.Error("expected a scan error, not ErrNotFound")
		}
	})
}

// ─── firedSlotsBetween ──────────────────────────────────────────────────────

func TestFiredSlotsBetween(t *testing.T) {
	t.Run("invalid timezone is skipped", func(t *testing.T) {
		s := PublicationSchedule{Slots: []Slot{{Weekday: 1, Time: "09:00", Timezone: "Not/ARealZone"}}}
		from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		now := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC)
		got := firedSlotsBetween(s, from, now)
		if len(got) != 1 || got[0] != 0 {
			t.Errorf("got %v, want [0]", got)
		}
	})

	t.Run("malformed HH:MM is skipped", func(t *testing.T) {
		s := PublicationSchedule{Slots: []Slot{{Weekday: 1, Time: "not-a-time", Timezone: "UTC"}}}
		from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		now := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC)
		got := firedSlotsBetween(s, from, now)
		if len(got) != 1 || got[0] != 0 {
			t.Errorf("got %v, want [0]", got)
		}
	})

	t.Run("fires expected number of times", func(t *testing.T) {
		// 2024-01-01 is a Monday. Weekday=1 (Monday), 09:00 UTC.
		s := PublicationSchedule{Slots: []Slot{{Weekday: 1, Time: "09:00", Timezone: "UTC"}}}
		from := time.Date(2023, 12, 31, 0, 0, 0, 0, time.UTC) // Sunday before
		now := time.Date(2024, 1, 22, 10, 0, 0, 0, time.UTC)  // 4th Monday, after 09:00
		got := firedSlotsBetween(s, from, now)
		if len(got) != 1 || got[0] != 4 {
			t.Errorf("got %v, want [4] (Mondays 1/1, 1/8, 1/15, 1/22)", got)
		}
	})

	t.Run("half-open window excludes the from boundary itself", func(t *testing.T) {
		s := PublicationSchedule{Slots: []Slot{{Weekday: 1, Time: "09:00", Timezone: "UTC"}}}
		from := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC) // exactly at the fire instant
		now := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
		got := firedSlotsBetween(s, from, now)
		if len(got) != 1 || got[0] != 0 {
			t.Errorf("got %v, want [0] (from is exclusive)", got)
		}
	})
}

// ─── getSchedule / scanSchedule ─────────────────────────────────────────────

func TestGetSchedule(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		db := openRefreshTestDB(t)
		_, err := getSchedule(db, "does-not-exist")
		if !errors.Is(err, smeldr.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := seedSchedule(t, db, "cred-get-1", ScheduleStatusActive)
		got, err := getSchedule(db, s.ID)
		if err != nil {
			t.Fatalf("getSchedule: %v", err)
		}
		if got.ID != s.ID || got.CredentialID != s.CredentialID {
			t.Errorf("got = %+v", got)
		}
	})

	t.Run("malformed slots JSON", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := seedSchedule(t, db, "cred-get-bad", ScheduleStatusActive)
		if _, err := db.Exec(
			`UPDATE smeldr_social_publication_schedules SET slots='not-json' WHERE id=?`, s.ID,
		); err != nil {
			t.Fatalf("corrupt slots: %v", err)
		}
		_, err := getSchedule(db, s.ID)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "unmarshal slots") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

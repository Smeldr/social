package forgesocial

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	forge "forge-cms.dev/forge"
)

// ScheduleStatus represents whether a PublicationSchedule is actively firing.
type ScheduleStatus string

const (
	// ScheduleStatusActive means the schedule fires slots and dequeues posts.
	ScheduleStatusActive ScheduleStatus = "active"
	// ScheduleStatusPaused means the schedule exists but does not fire.
	ScheduleStatusPaused ScheduleStatus = "paused"
)

// Slot defines a recurring publication time within a PublicationSchedule.
type Slot struct {
	// Weekday is the day of the week (0=Sunday … 6=Saturday, matches time.Weekday).
	Weekday int `json:"weekday"`
	// Time is the wall-clock time in 24-hour HH:MM format, e.g. "09:00".
	Time string `json:"time"`
	// Timezone is an IANA timezone name, e.g. "Europe/Copenhagen".
	Timezone string `json:"timezone"`
}

// PublicationSchedule defines a recurring set of time slots for automatically
// publishing queued posts for a specific credential.
//
// Each credential may have at most one PublicationSchedule.
// Use [Social.ScheduleModule] to expose this type via MCP tools.
type PublicationSchedule struct {
	ID           string         `json:"id"`
	CredentialID string         `json:"credential_id"`
	Slots        []Slot         `json:"slots"`
	Status       ScheduleStatus `json:"status"`
	LastTickAt   *time.Time     `json:"last_tick_at,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// GetSlug satisfies the slugger interface used by forge-mcp's allResources.
func (ps PublicationSchedule) GetSlug() string { return ps.ID }

// ─── DB helpers ───────────────────────────────────────────────────────────────

func insertSchedule(db forge.DB, s PublicationSchedule) error {
	slotsJSON, err := json.Marshal(s.Slots)
	if err != nil {
		return fmt.Errorf("forgesocial: marshal slots: %w", err)
	}
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO forge_social_publication_schedules
			(id, credential_id, slots, status, last_tick_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.CredentialID, string(slotsJSON), string(s.Status),
		nullTime(s.LastTickAt), s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func updateSchedule(db forge.DB, s PublicationSchedule) error {
	slotsJSON, err := json.Marshal(s.Slots)
	if err != nil {
		return fmt.Errorf("forgesocial: marshal slots: %w", err)
	}
	_, err = db.ExecContext(context.Background(), `
		UPDATE forge_social_publication_schedules
		SET slots=?, status=?, updated_at=?
		WHERE id=?`,
		string(slotsJSON), string(s.Status), time.Now().UTC(), s.ID,
	)
	return err
}

// updateScheduleLastTick records the last time the scheduler ticked this schedule.
func updateScheduleLastTick(db forge.DB, id string, t time.Time) error {
	_, err := db.ExecContext(context.Background(), `
		UPDATE forge_social_publication_schedules
		SET last_tick_at=?, updated_at=?
		WHERE id=?`,
		t, t, id,
	)
	return err
}

func getSchedule(db forge.DB, id string) (PublicationSchedule, error) {
	return scanSchedule(db.QueryRowContext(context.Background(), `
		SELECT id, credential_id, slots, status, last_tick_at, created_at, updated_at
		FROM forge_social_publication_schedules WHERE id=?`, id))
}

// getScheduleByCredential returns the schedule for the given credential_id.
func getScheduleByCredential(db forge.DB, credentialID string) (PublicationSchedule, error) {
	return scanSchedule(db.QueryRowContext(context.Background(), `
		SELECT id, credential_id, slots, status, last_tick_at, created_at, updated_at
		FROM forge_social_publication_schedules WHERE credential_id=?`, credentialID))
}

func listSchedules(db forge.DB) ([]PublicationSchedule, error) {
	rows, err := db.QueryContext(context.Background(), `
		SELECT id, credential_id, slots, status, last_tick_at, created_at, updated_at
		FROM forge_social_publication_schedules
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PublicationSchedule
	for rows.Next() {
		s, err := scanScheduleRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// listActiveSchedules returns all schedules with status='active'.
func listActiveSchedules(db forge.DB) ([]PublicationSchedule, error) {
	rows, err := db.QueryContext(context.Background(), `
		SELECT id, credential_id, slots, status, last_tick_at, created_at, updated_at
		FROM forge_social_publication_schedules
		WHERE status='active'
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PublicationSchedule
	for rows.Next() {
		s, err := scanScheduleRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func deleteSchedule(db forge.DB, id string) error {
	res, err := db.ExecContext(context.Background(),
		`DELETE FROM forge_social_publication_schedules WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return forge.ErrNotFound
	}
	return nil
}

// dequeueOldestQueued returns the oldest queued post for credentialID, or
// forge.ErrNotFound if the queue is empty.
func dequeueOldestQueued(db forge.DB, credentialID string) (ScheduledPost, error) {
	var p ScheduledPost
	var scheduledAt sql.NullTime
	err := db.QueryRowContext(context.Background(), `
		SELECT id, platform, credential_id, body, media_url, alt_text,
		       scheduled_at, status, platform_post_id, error_msg, created_at, updated_at
		FROM forge_social_posts
		WHERE credential_id=? AND status='queued'
		ORDER BY created_at ASC
		LIMIT 1`, credentialID,
	).Scan(
		&p.ID, &p.Platform, &p.CredentialID, &p.Body, &p.MediaURL, &p.AltText,
		&scheduledAt, &p.Status, &p.PlatformPostID, &p.ErrorMsg,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return p, forge.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if scheduledAt.Valid {
		t := scheduledAt.Time
		p.ScheduledAt = &t
	}
	return p, nil
}

// ─── slot-firing logic ────────────────────────────────────────────────────────

// firedSlotsBetween returns the number of times each slot in the schedule has
// fired in the half-open window (from, now]. A slot fires once per occurrence
// of its (weekday, HH:MM) in the given timezone window.
//
// Returns a slice parallel to s.Slots where each value is the fire count (0 or more).
func firedSlotsBetween(s PublicationSchedule, from, now time.Time) []int {
	counts := make([]int, len(s.Slots))

	for i, slot := range s.Slots {
		loc, err := time.LoadLocation(slot.Timezone)
		if err != nil {
			continue // invalid timezone — skip slot
		}
		var hour, min int
		if _, err := fmt.Sscanf(slot.Time, "%d:%d", &hour, &min); err != nil {
			continue // malformed HH:MM — skip slot
		}

		// Walk day by day from `from` to `now` in the slot's local timezone.
		// For each day where weekday matches, check if the slot time falls in (from, now].
		fromLocal := from.In(loc)
		nowLocal := now.In(loc)

		// Start at the beginning of the day containing `from` in local time.
		cursor := time.Date(fromLocal.Year(), fromLocal.Month(), fromLocal.Day(), 0, 0, 0, 0, loc)
		for !cursor.After(nowLocal) {
			if int(cursor.Weekday()) == slot.Weekday {
				fire := time.Date(cursor.Year(), cursor.Month(), cursor.Day(), hour, min, 0, 0, loc)
				if fire.After(from) && !fire.After(now) {
					counts[i]++
				}
			}
			cursor = cursor.AddDate(0, 0, 1)
		}
	}
	return counts
}

// ─── scan helpers ─────────────────────────────────────────────────────────────

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSchedule(row *sql.Row) (PublicationSchedule, error) {
	var s PublicationSchedule
	var slotsJSON string
	var lastTick sql.NullTime
	err := row.Scan(&s.ID, &s.CredentialID, &slotsJSON, &s.Status, &lastTick, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, forge.ErrNotFound
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal([]byte(slotsJSON), &s.Slots); err != nil {
		return s, fmt.Errorf("forgesocial: unmarshal slots: %w", err)
	}
	if s.Slots == nil {
		s.Slots = []Slot{}
	}
	if lastTick.Valid {
		t := lastTick.Time
		s.LastTickAt = &t
	}
	return s, nil
}

func scanScheduleRow(rows *sql.Rows) (PublicationSchedule, error) {
	var s PublicationSchedule
	var slotsJSON string
	var lastTick sql.NullTime
	if err := rows.Scan(&s.ID, &s.CredentialID, &slotsJSON, &s.Status, &lastTick, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return s, err
	}
	if err := json.Unmarshal([]byte(slotsJSON), &s.Slots); err != nil {
		return s, fmt.Errorf("forgesocial: unmarshal slots: %w", err)
	}
	if s.Slots == nil {
		s.Slots = []Slot{}
	}
	if lastTick.Valid {
		t := lastTick.Time
		s.LastTickAt = &t
	}
	return s, nil
}

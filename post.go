package forgesocial

import (
	"context"
	"database/sql"
	"errors"
	"time"

	forge "smeldr.dev/forge"
)

// PostStatus represents the lifecycle state of a ScheduledPost.
type PostStatus string

const (
	// PostStatusDraft is the initial state. scheduled_at may be nil.
	PostStatusDraft PostStatus = "draft"
	// PostStatusScheduled means the post has a scheduled_at in the future
	// and will be published by the internal scheduler.
	PostStatusScheduled PostStatus = "scheduled"
	// PostStatusPublished means the post has been successfully sent to the platform.
	PostStatusPublished PostStatus = "published"
	// PostStatusFailed means a terminal publish error occurred.
	PostStatusFailed PostStatus = "failed"
	// PostStatusArchived means the post has been manually archived.
	PostStatusArchived PostStatus = "archived"
	// PostStatusQueued means the post is waiting for the next available slot
	// in a PublicationSchedule. It has no scheduled_at and will be published
	// by the slot-queue scheduler when a slot fires for its credential.
	PostStatusQueued PostStatus = "queued"
)

// ScheduledPost represents a piece of content to be published to a social platform.
// Use [Social.PostModule] to expose ScheduledPost via MCP tools.
type ScheduledPost struct {
	ID             string     `json:"id"`
	Platform       string     `json:"platform"`
	CredentialID   string     `json:"credential_id"`
	Body           string     `json:"body"`
	MediaURL       string     `json:"media_url,omitempty"`
	AltText        string     `json:"alt_text,omitempty"`
	ScheduledAt    *time.Time `json:"scheduled_at,omitempty"`
	Status         PostStatus `json:"status"`
	PlatformPostID string     `json:"platform_post_id,omitempty"`
	ErrorMsg       string     `json:"error_msg,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// GetSlug satisfies the slugger interface used by forge-mcp's allResources.
func (p ScheduledPost) GetSlug() string { return p.ID }

// insertPost inserts a new ScheduledPost row.
func insertPost(db forge.DB, p ScheduledPost) error {
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO forge_social_posts
			(id, platform, credential_id, body, media_url, alt_text,
			 scheduled_at, status, platform_post_id, error_msg, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Platform, p.CredentialID, p.Body, p.MediaURL, p.AltText,
		nullTime(p.ScheduledAt), string(p.Status), p.PlatformPostID, p.ErrorMsg,
		p.CreatedAt, p.UpdatedAt,
	)
	return err
}

// updatePost applies a full row update (all mutable fields) to the post with p.ID.
func updatePost(db forge.DB, p ScheduledPost) error {
	_, err := db.ExecContext(context.Background(), `
		UPDATE forge_social_posts
		SET body=?, media_url=?, alt_text=?, scheduled_at=?, status=?,
		    platform_post_id=?, error_msg=?, updated_at=?
		WHERE id=?`,
		p.Body, p.MediaURL, p.AltText,
		nullTime(p.ScheduledAt), string(p.Status),
		p.PlatformPostID, p.ErrorMsg, time.Now().UTC(), p.ID,
	)
	return err
}

// markPostPublished sets status=published, platform_post_id, and updated_at.
func markPostPublished(db forge.DB, id, platformPostID string) error {
	_, err := db.ExecContext(context.Background(), `
		UPDATE forge_social_posts
		SET status='published', platform_post_id=?, error_msg='', updated_at=?
		WHERE id=?`,
		platformPostID, time.Now().UTC(), id,
	)
	return err
}

// markPostFailed sets status=failed and error_msg.
func markPostFailed(db forge.DB, id, errMsg string) error {
	_, err := db.ExecContext(context.Background(), `
		UPDATE forge_social_posts
		SET status='failed', error_msg=?, updated_at=?
		WHERE id=?`,
		errMsg, time.Now().UTC(), id,
	)
	return err
}

// getPost returns the ScheduledPost with the given id.
// Returns forge.ErrNotFound when no row exists.
func getPost(db forge.DB, id string) (ScheduledPost, error) {
	var p ScheduledPost
	var scheduledAt sql.NullTime
	err := db.QueryRowContext(context.Background(), `
		SELECT id, platform, credential_id, body, media_url, alt_text,
		       scheduled_at, status, platform_post_id, error_msg, created_at, updated_at
		FROM forge_social_posts WHERE id=?`, id,
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

// listPosts returns all ScheduledPost rows, ordered by created_at DESC.
// If statuses is non-empty, only rows with a matching status are returned.
func listPosts(db forge.DB, statuses ...PostStatus) ([]ScheduledPost, error) {
	query := `
		SELECT id, platform, credential_id, body, media_url, alt_text,
		       scheduled_at, status, platform_post_id, error_msg, created_at, updated_at
		FROM forge_social_posts`
	var args []any
	if len(statuses) > 0 {
		query += " WHERE status IN ("
		for i, s := range statuses {
			if i > 0 {
				query += ","
			}
			query += "?"
			args = append(args, string(s))
		}
		query += ")"
	}
	query += " ORDER BY created_at DESC"

	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScheduledPost
	for rows.Next() {
		var p ScheduledPost
		var scheduledAt sql.NullTime
		if err := rows.Scan(
			&p.ID, &p.Platform, &p.CredentialID, &p.Body, &p.MediaURL, &p.AltText,
			&scheduledAt, &p.Status, &p.PlatformPostID, &p.ErrorMsg,
			&p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if scheduledAt.Valid {
			t := scheduledAt.Time
			p.ScheduledAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// deletePost permanently removes the post with the given id.
// Returns forge.ErrNotFound when no row exists.
func deletePost(db forge.DB, id string) error {
	res, err := db.ExecContext(context.Background(),
		`DELETE FROM forge_social_posts WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return forge.ErrNotFound
	}
	return nil
}

// duePosts returns all scheduled posts whose scheduled_at is in the past.
// These are the posts the scheduler should publish next.
func duePosts(db forge.DB) ([]ScheduledPost, error) {
	rows, err := db.QueryContext(context.Background(), `
		SELECT id, platform, credential_id, body, media_url, alt_text,
		       scheduled_at, status, platform_post_id, error_msg, created_at, updated_at
		FROM forge_social_posts
		WHERE status='scheduled' AND scheduled_at IS NOT NULL AND scheduled_at <= ?
		ORDER BY scheduled_at ASC`,
		time.Now().UTC(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScheduledPost
	for rows.Next() {
		var p ScheduledPost
		var scheduledAt sql.NullTime
		if err := rows.Scan(
			&p.ID, &p.Platform, &p.CredentialID, &p.Body, &p.MediaURL, &p.AltText,
			&scheduledAt, &p.Status, &p.PlatformPostID, &p.ErrorMsg,
			&p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if scheduledAt.Valid {
			t := scheduledAt.Time
			p.ScheduledAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// nextScheduledAt returns the earliest scheduled_at among all scheduled posts,
// or nil if no scheduled posts exist.
func nextScheduledAt(db forge.DB) (*time.Time, error) {
	var t sql.NullTime
	err := db.QueryRowContext(context.Background(), `
		SELECT MIN(scheduled_at)
		FROM forge_social_posts
		WHERE status='scheduled' AND scheduled_at IS NOT NULL`,
	).Scan(&t)
	if err != nil {
		return nil, err
	}
	if !t.Valid {
		return nil, nil
	}
	v := t.Time
	return &v, nil
}

// logDeliveryAttempt records a publish attempt in forge_social_delivery_log.
func logDeliveryAttempt(db forge.DB, postID string, attempt, statusCode int, errMsg string) error {
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO forge_social_delivery_log
			(id, post_id, attempt, status_code, error, attempted_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		forge.NewID(), postID, attempt, statusCode, errMsg, time.Now().UTC(),
	)
	return err
}

// deliveryAttemptCount returns the number of previous delivery attempts for postID.
func deliveryAttemptCount(db forge.DB, postID string) (int, error) {
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM forge_social_delivery_log WHERE post_id=?`, postID,
	).Scan(&n)
	return n, err
}

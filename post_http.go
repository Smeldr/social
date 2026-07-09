package social

import (
	"encoding/json"
	"net/http"
	"time"

	"smeldr.dev/core"
)

// handlePostCreate handles POST /social/posts — creates a new ScheduledPost.
// Requires Bearer token. Returns 201 + the created post as JSON.
func (s *Social) handlePostCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireBearer(w, r); !ok {
		return
	}

	var body struct {
		Platform     string     `json:"platform"`
		CredentialID string     `json:"credential_id"`
		Body         string     `json:"body"`
		MediaURL     string     `json:"media_url"`
		AltText      string     `json:"alt_text"`
		ScheduledAt  *time.Time `json:"scheduled_at"`
		Status       PostStatus `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		smeldr.WriteError(w, r, smeldr.ErrBadRequest)
		return
	}
	if body.CredentialID == "" {
		smeldr.WriteError(w, r, smeldr.ErrBadRequest)
		return
	}
	if body.Body == "" {
		smeldr.WriteError(w, r, smeldr.ErrBadRequest)
		return
	}
	if body.Platform == "" {
		body.Platform = "mastodon"
	}
	if body.Status == "" {
		body.Status = PostStatusDraft
	}

	now := time.Now().UTC()
	p := ScheduledPost{
		ID:           smeldr.NewID(),
		Platform:     body.Platform,
		CredentialID: body.CredentialID,
		Body:         body.Body,
		MediaURL:     body.MediaURL,
		AltText:      body.AltText,
		ScheduledAt:  body.ScheduledAt,
		Status:       body.Status,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := insertPost(s.db, p); err != nil {
		smeldr.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// handlePostList handles GET /social/posts — returns all posts, optionally
// filtered by the ?status= query parameter.
func (s *Social) handlePostList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireBearer(w, r); !ok {
		return
	}

	var statuses []PostStatus
	if v := r.URL.Query().Get("status"); v != "" {
		statuses = []PostStatus{PostStatus(v)}
	}
	posts, err := listPosts(s.db, statuses...)
	if err != nil {
		smeldr.WriteError(w, r, err)
		return
	}
	if posts == nil {
		posts = []ScheduledPost{}
	}
	writeJSON(w, http.StatusOK, posts)
}

// handlePostGet handles GET /social/posts/{id} — returns one ScheduledPost.
// Returns 404 when the id is not found.
func (s *Social) handlePostGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireBearer(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		smeldr.WriteError(w, r, smeldr.ErrBadRequest)
		return
	}
	p, err := getPost(s.db, id)
	if err != nil {
		smeldr.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handlePostUpdate handles PUT /social/posts/{id} — patch-merges only the
// fields present in the request body onto the existing post. Returns the
// updated post. Returns 404 when the id is not found.
func (s *Social) handlePostUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireBearer(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	p, err := getPost(s.db, id)
	if err != nil {
		smeldr.WriteError(w, r, err)
		return
	}

	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		smeldr.WriteError(w, r, smeldr.ErrBadRequest)
		return
	}
	if v, ok := patch["platform"].(string); ok && v != "" {
		p.Platform = v
	}
	if v, ok := patch["body"].(string); ok {
		p.Body = v
	}
	if v, ok := patch["media_url"].(string); ok {
		p.MediaURL = v
	}
	if v, ok := patch["alt_text"].(string); ok {
		p.AltText = v
	}
	if v, ok := patch["status"].(string); ok && v != "" {
		p.Status = PostStatus(v)
	}
	if _, has := patch["scheduled_at"]; has {
		if v, ok := patch["scheduled_at"].(string); ok && v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				smeldr.WriteError(w, r, smeldr.ErrBadRequest)
				return
			}
			p.ScheduledAt = &t
		} else {
			p.ScheduledAt = nil
		}
	}

	if err := updatePost(s.db, p); err != nil {
		smeldr.WriteError(w, r, err)
		return
	}
	// Re-fetch to return the updated_at timestamp written by updatePost.
	p, err = getPost(s.db, id)
	if err != nil {
		smeldr.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handlePostDelete handles DELETE /social/posts/{id} — permanently removes
// the post. Returns 204 on success, 404 when the id is not found.
func (s *Social) handlePostDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireBearer(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if err := deletePost(s.db, id); err != nil {
		smeldr.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireBearer verifies the Bearer token in the Authorization header against
// the token store. Returns the authenticated User and true on success; writes
// 401 and returns false on failure.
func (s *Social) requireBearer(w http.ResponseWriter, r *http.Request) (smeldr.User, bool) {
	user, ok := smeldr.VerifyBearerToken(r, s.cfg.Secret, s.tokens)
	if !ok {
		smeldr.WriteError(w, r, smeldr.ErrUnauth)
		return smeldr.User{}, false
	}
	return user, true
}

// writeJSON writes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

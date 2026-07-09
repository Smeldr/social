package social_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"smeldr.dev/core"
	"smeldr.dev/social"
)

const testSecret = "test-secret-32-bytes-long-padded!"

// createTokenTable creates the smeldr_tokens table required by TokenStore.
func createTokenTable(t *testing.T, db smeldr.DB) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS smeldr_tokens (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			role       TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			revoked_at TEXT,
			created_at TEXT NOT NULL
		)`)
	if err != nil {
		t.Fatalf("createTokenTable: %v", err)
	}
}

// makeTestToken creates a bearer token for use in test requests.
func makeTestToken(t *testing.T, db smeldr.DB) string {
	t.Helper()
	createTokenTable(t, db)
	ts := smeldr.NewTokenStore(db, testSecret)
	tok, err := ts.Create(context.Background(), "test-user", "admin", time.Hour)
	if err != nil {
		t.Fatalf("makeTestToken: %v", err)
	}
	return tok
}

// newPostServer returns a Social, its test HTTP server, and a valid bearer token.
func newPostServer(t *testing.T) (*social.Social, *httptest.Server, string) {
	t.Helper()
	db := openTestDB(t)
	cfg := social.Config{Secret: []byte(testSecret)}
	svc := social.New(db, cfg)
	tok := makeTestToken(t, db)
	srv := httptest.NewServer(svc.PostHandlerForTest())
	t.Cleanup(srv.Close)
	return svc, srv, tok
}

// doRequest sends a JSON HTTP request to the test server and returns the response.
func doRequest(t *testing.T, srv *httptest.Server, method, path string, body any, tok string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// decodeJSON decodes the response body into v.
func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// ─── POST /social/posts ───────────────────────────────────────────────────────

func TestPostCreate_NoToken(t *testing.T) {
	_, srv, _ := newPostServer(t)
	resp := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "hello",
	}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

func TestPostCreate_BadToken(t *testing.T) {
	_, srv, _ := newPostServer(t)
	resp := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "hello",
	}, "not-a-valid-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

func TestPostCreate_BadJSON(t *testing.T) {
	_, srv, tok := newPostServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/social/posts", bytes.NewBufferString("not-json"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestPostCreate_MissingCredential(t *testing.T) {
	_, srv, tok := newPostServer(t)
	resp := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"body": "hello",
	}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestPostCreate_MissingBody(t *testing.T) {
	_, srv, tok := newPostServer(t)
	resp := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
	}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestPostCreate_OK(t *testing.T) {
	_, srv, tok := newPostServer(t)
	resp := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "hello world",
		"platform":      "mastodon",
	}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	var p map[string]any
	decodeJSON(t, resp, &p)
	if p["id"] == "" {
		t.Error("response missing id")
	}
	if p["body"] != "hello world" {
		t.Errorf("want body=hello world, got %v", p["body"])
	}
	if p["status"] != "draft" {
		t.Errorf("want status=draft, got %v", p["status"])
	}
}

// ─── GET /social/posts ────────────────────────────────────────────────────────

func TestPostList_NoToken(t *testing.T) {
	_, srv, _ := newPostServer(t)
	resp := doRequest(t, srv, http.MethodGet, "/social/posts", nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

func TestPostList_Empty(t *testing.T) {
	_, srv, tok := newPostServer(t)
	resp := doRequest(t, srv, http.MethodGet, "/social/posts", nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var items []any
	decodeJSON(t, resp, &items)
	if len(items) != 0 {
		t.Errorf("want empty list, got %d items", len(items))
	}
}

func TestPostList_OK(t *testing.T) {
	_, srv, tok := newPostServer(t)
	// Create two posts.
	for _, b := range []string{"post one", "post two"} {
		r := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
			"credential_id": "cred-1",
			"body":          b,
		}, tok)
		r.Body.Close()
	}
	resp := doRequest(t, srv, http.MethodGet, "/social/posts", nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var items []any
	decodeJSON(t, resp, &items)
	if len(items) != 2 {
		t.Errorf("want 2 items, got %d", len(items))
	}
}

func TestPostList_StatusFilter(t *testing.T) {
	_, srv, tok := newPostServer(t)
	// Create one draft post.
	doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "draft post",
		"status":        "draft",
	}, tok).Body.Close()
	// Filter by draft — should return 1.
	resp := doRequest(t, srv, http.MethodGet, "/social/posts?status=draft", nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var items []any
	decodeJSON(t, resp, &items)
	if len(items) != 1 {
		t.Errorf("want 1 item, got %d", len(items))
	}
	// Filter by scheduled — should return 0.
	resp2 := doRequest(t, srv, http.MethodGet, "/social/posts?status=scheduled", nil, tok)
	defer resp2.Body.Close()
	var items2 []any
	decodeJSON(t, resp2, &items2)
	if len(items2) != 0 {
		t.Errorf("want 0 items for status=scheduled, got %d", len(items2))
	}
}

// ─── GET /social/posts/{id} ───────────────────────────────────────────────────

func TestPostGet_NoToken(t *testing.T) {
	_, srv, _ := newPostServer(t)
	resp := doRequest(t, srv, http.MethodGet, "/social/posts/some-id", nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

func TestPostGet_NotFound(t *testing.T) {
	_, srv, tok := newPostServer(t)
	resp := doRequest(t, srv, http.MethodGet, "/social/posts/no-such-id", nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", resp.StatusCode)
	}
}

func TestPostGet_OK(t *testing.T) {
	_, srv, tok := newPostServer(t)
	// Create a post.
	cr := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "get me",
	}, tok)
	defer cr.Body.Close()
	var created map[string]any
	decodeJSON(t, cr, &created)
	id, _ := created["id"].(string)

	resp := doRequest(t, srv, http.MethodGet, "/social/posts/"+id, nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var p map[string]any
	decodeJSON(t, resp, &p)
	if p["id"] != id {
		t.Errorf("want id=%s, got %v", id, p["id"])
	}
}

// ─── PUT /social/posts/{id} ───────────────────────────────────────────────────

func TestPostUpdate_NoToken(t *testing.T) {
	_, srv, _ := newPostServer(t)
	resp := doRequest(t, srv, http.MethodPut, "/social/posts/some-id", map[string]any{"body": "x"}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

func TestPostUpdate_NotFound(t *testing.T) {
	_, srv, tok := newPostServer(t)
	resp := doRequest(t, srv, http.MethodPut, "/social/posts/no-such-id", map[string]any{"body": "x"}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", resp.StatusCode)
	}
}

func TestPostUpdate_OK(t *testing.T) {
	_, srv, tok := newPostServer(t)
	cr := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "original",
	}, tok)
	defer cr.Body.Close()
	var created map[string]any
	decodeJSON(t, cr, &created)
	id, _ := created["id"].(string)

	resp := doRequest(t, srv, http.MethodPut, "/social/posts/"+id, map[string]any{
		"body": "updated",
	}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var p map[string]any
	decodeJSON(t, resp, &p)
	if p["body"] != "updated" {
		t.Errorf("want body=updated, got %v", p["body"])
	}
}

func TestPostUpdate_StatusQueued(t *testing.T) {
	_, srv, tok := newPostServer(t)
	cr := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "queue me",
	}, tok)
	defer cr.Body.Close()
	var created map[string]any
	decodeJSON(t, cr, &created)
	id, _ := created["id"].(string)

	resp := doRequest(t, srv, http.MethodPut, "/social/posts/"+id, map[string]any{
		"status": "queued",
	}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var p map[string]any
	decodeJSON(t, resp, &p)
	if p["status"] != "queued" {
		t.Errorf("want status=queued, got %v", p["status"])
	}
}

func TestPostUpdate_BadJSON(t *testing.T) {
	_, srv, tok := newPostServer(t)
	cr := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "target",
	}, tok)
	defer cr.Body.Close()
	var created map[string]any
	decodeJSON(t, cr, &created)
	id, _ := created["id"].(string)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/social/posts/"+id, bytes.NewBufferString("not-json"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestPostUpdate_ScheduledAt(t *testing.T) {
	_, srv, tok := newPostServer(t)
	cr := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "will be scheduled",
	}, tok)
	defer cr.Body.Close()
	var created map[string]any
	decodeJSON(t, cr, &created)
	id, _ := created["id"].(string)

	at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	resp := doRequest(t, srv, http.MethodPut, "/social/posts/"+id, map[string]any{
		"scheduled_at": at,
		"status":       "scheduled",
	}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var p map[string]any
	decodeJSON(t, resp, &p)
	if p["scheduled_at"] == nil {
		t.Error("want scheduled_at to be set")
	}
}

func TestPostUpdate_BadScheduledAt(t *testing.T) {
	_, srv, tok := newPostServer(t)
	cr := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "target",
	}, tok)
	defer cr.Body.Close()
	var created map[string]any
	decodeJSON(t, cr, &created)
	id, _ := created["id"].(string)

	resp := doRequest(t, srv, http.MethodPut, "/social/posts/"+id, map[string]any{
		"scheduled_at": "not-a-date",
	}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestPostUpdate_ClearScheduledAt(t *testing.T) {
	_, srv, tok := newPostServer(t)
	// Create post with a scheduled_at.
	at := time.Now().UTC().Add(time.Hour)
	cr := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "will be cleared",
		"scheduled_at":  at.Format(time.RFC3339),
	}, tok)
	defer cr.Body.Close()
	var created map[string]any
	decodeJSON(t, cr, &created)
	id, _ := created["id"].(string)

	// Clear it by sending scheduled_at: null.
	resp := doRequest(t, srv, http.MethodPut, "/social/posts/"+id, map[string]any{
		"scheduled_at": nil,
	}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var p map[string]any
	decodeJSON(t, resp, &p)
	if _, has := p["scheduled_at"]; has {
		t.Errorf("want scheduled_at absent after clear, got %v", p["scheduled_at"])
	}
}

// ─── DELETE /social/posts/{id} ────────────────────────────────────────────────

func TestPostDelete_NoToken(t *testing.T) {
	_, srv, _ := newPostServer(t)
	resp := doRequest(t, srv, http.MethodDelete, "/social/posts/some-id", nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

func TestPostDelete_NotFound(t *testing.T) {
	_, srv, tok := newPostServer(t)
	resp := doRequest(t, srv, http.MethodDelete, "/social/posts/no-such-id", nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", resp.StatusCode)
	}
}

func TestPostDelete_OK(t *testing.T) {
	_, srv, tok := newPostServer(t)
	cr := doRequest(t, srv, http.MethodPost, "/social/posts", map[string]any{
		"credential_id": "cred-1",
		"body":          "delete me",
	}, tok)
	defer cr.Body.Close()
	var created map[string]any
	decodeJSON(t, cr, &created)
	id, _ := created["id"].(string)

	resp := doRequest(t, srv, http.MethodDelete, "/social/posts/"+id, nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}

	// Confirm it's gone.
	get := doRequest(t, srv, http.MethodGet, "/social/posts/"+id, nil, tok)
	defer get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 after delete, got %d", get.StatusCode)
	}
}

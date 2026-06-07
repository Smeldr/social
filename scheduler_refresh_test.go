package social

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openRefreshTestDB opens an in-memory SQLite DB for scheduler refresh tests.
func openRefreshTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open refresh test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	social := New(db, Config{Secret: refreshTestSecret})
	_ = social // tables are created by New
	return db
}

var refreshTestSecret = []byte("refresh-test-secret-32-bytes-ok!")

// newRefreshSocial creates a Social backed by db with tc wired as the twitter client.
func newRefreshSocial(t *testing.T, db *sql.DB, tc *twitterClient) *Social {
	t.Helper()
	s := New(db, Config{Secret: refreshTestSecret})
	s.mu.Lock()
	s.twitter = tc
	s.mu.Unlock()
	return s
}

// newRefreshTwitterClient builds a twitterClient that routes HTTP through hc.
func newRefreshTwitterClient(hc *http.Client) *twitterClient {
	return &twitterClient{
		cfg:        xConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"},
		httpClient: hc,
	}
}

// insertXCredential inserts an X credential into db and returns its ID.
func insertXCredential(t *testing.T, s *Social, accessToken, refreshToken string, expiresAt *time.Time) string {
	t.Helper()
	id, err := s.creds.upsertCredentialByInstance("x", xAPIBase, "test-cred",
		accessToken, refreshToken, "", expiresAt)
	if err != nil {
		t.Fatalf("insert x credential: %v", err)
	}
	return id
}

// tokenHandler returns an httptest.Handler that serves a fixed xTokenResponse.
func tokenHandler(resp xTokenResponse, statusCode int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func TestMaybeRefreshXCredential_NotX(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	db := openRefreshTestDB(t)
	s := newRefreshSocial(t, db, newRefreshTwitterClient(srv.Client()))

	expiry := time.Now().Add(1 * time.Minute)
	cred := PlatformCredential{
		ID:           "cred-1",
		Platform:     "mastodon",
		accessToken:  "old-access",
		refreshToken: "old-refresh",
		ExpiresAt:    &expiry,
	}

	s.maybeRefreshXCredential(context.Background(), &cred)

	if called {
		t.Error("expected no HTTP call for non-X credential")
	}
	if cred.accessToken != "old-access" {
		t.Error("access token must not change for non-X credential")
	}
}

func TestMaybeRefreshXCredential_NotExpiring(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	db := openRefreshTestDB(t)
	s := newRefreshSocial(t, db, newRefreshTwitterClient(srv.Client()))

	expiry := time.Now().Add(10 * time.Minute) // well outside 5-min buffer
	cred := PlatformCredential{
		ID:           "cred-2",
		Platform:     "x",
		accessToken:  "old-access",
		refreshToken: "old-refresh",
		ExpiresAt:    &expiry,
	}

	s.maybeRefreshXCredential(context.Background(), &cred)

	if called {
		t.Error("expected no HTTP call when token is not expiring soon")
	}
	if cred.accessToken != "old-access" {
		t.Error("access token must not change when not expiring")
	}
}

func TestMaybeRefreshXCredential_NilExpiry(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	db := openRefreshTestDB(t)
	s := newRefreshSocial(t, db, newRefreshTwitterClient(srv.Client()))

	cred := PlatformCredential{
		ID:           "cred-3",
		Platform:     "x",
		accessToken:  "old-access",
		refreshToken: "old-refresh",
		ExpiresAt:    nil,
	}

	s.maybeRefreshXCredential(context.Background(), &cred)

	if called {
		t.Error("expected no HTTP call when ExpiresAt is nil")
	}
	if cred.accessToken != "old-access" {
		t.Error("access token must not change when ExpiresAt is nil")
	}
}

func TestMaybeRefreshXCredential_Expiring(t *testing.T) {
	newTokenResp := xTokenResponse{
		AccessToken:  "new-access-token",
		RefreshToken: "new-refresh-token",
		ExpiresIn:    7200,
	}
	srv := httptest.NewServer(tokenHandler(newTokenResp, http.StatusOK))
	defer srv.Close()

	// Point the twitter client's base URL at the test server by overriding the
	// httpClient with one that redirects to the test server.
	tc := &twitterClient{
		cfg: xConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
		},
		httpClient: &http.Client{
			Transport: rewriteTransport{base: srv.URL, inner: http.DefaultTransport},
		},
	}

	db := openRefreshTestDB(t)
	s := newRefreshSocial(t, db, tc)

	expiry := time.Now().Add(2 * time.Minute) // within 5-min buffer
	id := insertXCredential(t, s, "old-access", "old-refresh", &expiry)

	cred, err := s.creds.getCredential(id)
	if err != nil {
		t.Fatalf("get credential: %v", err)
	}

	s.maybeRefreshXCredential(context.Background(), &cred)

	// In-memory cred must be updated.
	if cred.accessToken != "new-access-token" {
		t.Errorf("expected accessToken %q, got %q", "new-access-token", cred.accessToken)
	}
	if cred.refreshToken != "new-refresh-token" {
		t.Errorf("expected refreshToken %q, got %q", "new-refresh-token", cred.refreshToken)
	}
	if cred.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be set after refresh")
	}

	// DB must also be updated.
	fetched, err := s.creds.getCredential(id)
	if err != nil {
		t.Fatalf("get credential after refresh: %v", err)
	}
	if fetched.accessToken != "new-access-token" {
		t.Errorf("DB accessToken: expected %q, got %q", "new-access-token", fetched.accessToken)
	}
	if fetched.refreshToken != "new-refresh-token" {
		t.Errorf("DB refreshToken: expected %q, got %q", "new-refresh-token", fetched.refreshToken)
	}
}

func TestMaybeRefreshXCredential_RefreshFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	tc := &twitterClient{
		cfg:        xConfig{ClientID: "cid", ClientSecret: "csec"},
		httpClient: &http.Client{Transport: rewriteTransport{base: srv.URL, inner: http.DefaultTransport}},
	}

	db := openRefreshTestDB(t)
	s := newRefreshSocial(t, db, tc)

	expiry := time.Now().Add(1 * time.Minute)
	id := insertXCredential(t, s, "old-access", "old-refresh", &expiry)
	cred, _ := s.creds.getCredential(id)

	// Must not panic; cred must remain unchanged.
	s.maybeRefreshXCredential(context.Background(), &cred)

	if cred.accessToken != "old-access" {
		t.Errorf("access token must be unchanged after refresh failure, got %q", cred.accessToken)
	}
	if cred.refreshToken != "old-refresh" {
		t.Errorf("refresh token must be unchanged after refresh failure, got %q", cred.refreshToken)
	}
}

func TestMaybeRefreshXCredential_NoNewRefreshToken(t *testing.T) {
	newTokenResp := xTokenResponse{
		AccessToken:  "new-access-token",
		RefreshToken: "", // omitted — X does not always rotate
		ExpiresIn:    7200,
	}
	srv := httptest.NewServer(tokenHandler(newTokenResp, http.StatusOK))
	defer srv.Close()

	tc := &twitterClient{
		cfg:        xConfig{ClientID: "cid", ClientSecret: "csec"},
		httpClient: &http.Client{Transport: rewriteTransport{base: srv.URL, inner: http.DefaultTransport}},
	}

	db := openRefreshTestDB(t)
	s := newRefreshSocial(t, db, tc)

	expiry := time.Now().Add(1 * time.Minute)
	id := insertXCredential(t, s, "old-access", "old-refresh", &expiry)
	cred, _ := s.creds.getCredential(id)

	s.maybeRefreshXCredential(context.Background(), &cred)

	// Access token updated, existing refresh token retained.
	if cred.accessToken != "new-access-token" {
		t.Errorf("expected new access token, got %q", cred.accessToken)
	}
	if cred.refreshToken != "old-refresh" {
		t.Errorf("expected retained refresh token %q, got %q", "old-refresh", cred.refreshToken)
	}

	// Verify DB also retained the old refresh token.
	fetched, err := s.creds.getCredential(id)
	if err != nil {
		t.Fatalf("get credential after refresh: %v", err)
	}
	if fetched.refreshToken != "old-refresh" {
		t.Errorf("DB: expected retained refresh token %q, got %q", "old-refresh", fetched.refreshToken)
	}
}

// rewriteTransport redirects all requests to a test server base URL while
// preserving path, query, and headers. Used to intercept calls to xAPIBase.
type rewriteTransport struct {
	base  string
	inner http.RoundTripper
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Host = req.URL.Host
	// Replace scheme+host with the test server's base.
	srv, _ := http.NewRequest(http.MethodGet, rt.base, nil)
	clone.URL.Scheme = srv.URL.Scheme
	clone.URL.Host = srv.URL.Host
	return rt.inner.RoundTrip(clone)
}

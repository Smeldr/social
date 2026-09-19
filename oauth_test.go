package social

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// — handleXCallback ———————————————————————————————————————————————————————————
//
// consumeOAuthState's own internals (unknown-state / expired-state parsing)
// are exercised by the Mastodon callback tests in mastodon_test.go; these
// tests focus on handleXCallback's own branches, using insertOAuthState
// (oauth.go) to seed whatever state row each branch needs.

func TestHandleXCallback(t *testing.T) {
	t.Run("oauth error query param returns 400", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := New(db, Config{Secret: refreshTestSecret})

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?error=access_denied", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing code returns 400", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := New(db, Config{Secret: refreshTestSecret})

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?state=some-state", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing state returns 400", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := New(db, Config{Secret: refreshTestSecret})

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?code=some-code", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("unknown state returns 400", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := New(db, Config{Secret: refreshTestSecret})

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?code=abc&state=does-not-exist", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("state belongs to a different platform returns 400", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := New(db, Config{Secret: refreshTestSecret})

		if err := insertOAuthState(db, "state-mastodon", "mastodon", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?code=abc&state=state-mastodon", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("X not configured returns 503", func(t *testing.T) {
		db := openRefreshTestDB(t)
		s := New(db, Config{Secret: refreshTestSecret}) // s.twitter stays nil — no platform config loaded

		if err := insertOAuthState(db, "state-x-1", "x", "verifier"); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?code=abc&state=state-x-1", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})

	t.Run("exchangeCode failure returns 502", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		tc := &twitterClient{
			cfg:        xConfig{ClientID: "cid", ClientSecret: "csec"},
			httpClient: &http.Client{Transport: &xAPIRedirectTransport{apiBase: srv.URL}},
		}
		s := newRefreshSocial(t, db, tc)

		if err := insertOAuthState(db, "state-x-2", "x", "verifier"); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?code=abc&state=state-x-2", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
		}
	})

	t.Run("credential upsert failure returns 500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(xTokenResponse{
				AccessToken:  "tok",
				RefreshToken: "reftok",
				ExpiresIn:    7200,
			})
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		tc := &twitterClient{
			cfg:        xConfig{ClientID: "cid", ClientSecret: "csec"},
			httpClient: &http.Client{Transport: &xAPIRedirectTransport{apiBase: srv.URL}},
		}
		s := newRefreshSocial(t, db, tc)

		if err := insertOAuthState(db, "state-x-3", "x", "verifier"); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		// Force upsertCredentialByInstance to fail without a custom smeldr.DB
		// implementation (smeldr.DB is documented as "do not implement
		// directly" — see storage.go): drop the table it writes to. The
		// oauth_states table it doesn't touch stays intact, so state
		// consumption and token exchange above still succeed.
		if _, err := db.Exec("DROP TABLE smeldr_social_credentials"); err != nil {
			t.Fatalf("drop smeldr_social_credentials: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?code=abc&state=state-x-3", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("success with SuccessURL redirects", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(xTokenResponse{
				AccessToken:  "tok",
				RefreshToken: "reftok",
				ExpiresIn:    7200,
			})
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		tc := &twitterClient{
			cfg: xConfig{
				ClientID:     "cid",
				ClientSecret: "csec",
				SuccessURL:   "https://example.com/connected",
			},
			httpClient: &http.Client{Transport: &xAPIRedirectTransport{apiBase: srv.URL}},
		}
		s := newRefreshSocial(t, db, tc)

		if err := insertOAuthState(db, "state-x-4", "x", "verifier"); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?code=abc&state=state-x-4", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
		}
		if loc := rec.Header().Get("Location"); loc != "https://example.com/connected" {
			t.Errorf("Location = %q, want %q", loc, "https://example.com/connected")
		}
	})

	t.Run("success without SuccessURL writes HTML and persists credential", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(xTokenResponse{
				AccessToken: "tok",
				ExpiresIn:   0, // omitted expiry — exercises the expiresAt == nil branch too
			})
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		tc := &twitterClient{
			cfg:        xConfig{ClientID: "cid", ClientSecret: "csec"},
			httpClient: &http.Client{Transport: &xAPIRedirectTransport{apiBase: srv.URL}},
		}
		s := newRefreshSocial(t, db, tc)

		if err := insertOAuthState(db, "state-x-5", "x", "verifier"); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/x/callback?code=abc&state=state-x-5", nil)
		rec := httptest.NewRecorder()
		s.handleXCallback(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if !strings.Contains(rec.Body.String(), "Connected to X successfully") {
			t.Errorf("body missing success message: %s", rec.Body.String())
		}

		creds, err := s.creds.listCredentials()
		if err != nil {
			t.Fatalf("listCredentials: %v", err)
		}
		found := false
		for _, c := range creds {
			if c.Platform == "x" {
				found = true
			}
		}
		if !found {
			t.Error("expected an x credential to be persisted")
		}
	})
}

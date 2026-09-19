package social

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// — test helpers ————————————————————————————————————————————————————————————

// erroringTransport is an http.RoundTripper that always fails, simulating a
// network-level error from hc.Do (as opposed to a non-2xx HTTP response).
type erroringTransport struct{}

func (erroringTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("simulated network failure")
}

// newTestLinkedinClient builds a linkedinClient whose http.Client routes every
// outbound request to srv, regardless of the hardcoded www.linkedin.com /
// api.linkedin.com host in the request URL. Reuses the rewriteTransport
// already defined in scheduler_refresh_test.go (same package) rather than
// introducing a second host-rewriting RoundTripper — it already redirects
// all outbound requests to a single test server while preserving path and
// query, which is exactly what's needed here since LinkedIn's three
// endpoints differ only by path.
func newTestLinkedinClient(srv *httptest.Server) *linkedinClient {
	return &linkedinClient{
		cfg: LinkedInConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			RedirectURL:  "https://example.com/oauth/linkedin/callback",
		},
		hc: &http.Client{Transport: rewriteTransport{base: srv.URL, inner: http.DefaultTransport}},
	}
}

// — exchangeCode ——————————————————————————————————————————————————————————————

func TestLinkedinExchangeCode(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/oauth/v2/accessToken" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"tok-abc","expires_in":5184000}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		tr, err := lc.exchangeCode(context.Background(), "auth-code")
		if err != nil {
			t.Fatalf("exchangeCode: %v", err)
		}
		if tr.AccessToken != "tok-abc" {
			t.Errorf("AccessToken = %q, want %q", tr.AccessToken, "tok-abc")
		}
		if tr.ExpiresIn != 5184000 {
			t.Errorf("ExpiresIn = %d, want %d", tr.ExpiresIn, 5184000)
		}
	})

	t.Run("request build error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("server should not be called when request build fails")
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		//nolint:staticcheck // intentionally passing nil context to force http.NewRequestWithContext to fail
		_, err := lc.exchangeCode(nil, "auth-code")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "linkedin token request") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("Do error", func(t *testing.T) {
		lc := &linkedinClient{
			cfg: LinkedInConfig{ClientID: "cid", ClientSecret: "csec"},
			hc:  &http.Client{Transport: erroringTransport{}},
		}
		_, err := lc.exchangeCode(context.Background(), "auth-code")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "linkedin token exchange") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		_, err := lc.exchangeCode(context.Background(), "auth-code")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "HTTP 400") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("decode error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`not json`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		_, err := lc.exchangeCode(context.Background(), "auth-code")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "linkedin token decode") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// — fetchPersonURN ————————————————————————————————————————————————————————————

func TestLinkedinFetchPersonURN(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v2/userinfo" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer tok-abc" {
				t.Errorf("Authorization header = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"sub":"abcDEF123"}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		urn, err := lc.fetchPersonURN(context.Background(), "tok-abc")
		if err != nil {
			t.Fatalf("fetchPersonURN: %v", err)
		}
		if urn != "urn:li:person:abcDEF123" {
			t.Errorf("urn = %q, want %q", urn, "urn:li:person:abcDEF123")
		}
	})

	t.Run("request build error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("server should not be called when request build fails")
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		//nolint:staticcheck // intentionally passing nil context to force http.NewRequestWithContext to fail
		_, err := lc.fetchPersonURN(nil, "tok-abc")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "linkedin userinfo request") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("Do error", func(t *testing.T) {
		lc := &linkedinClient{hc: &http.Client{Transport: erroringTransport{}}}
		_, err := lc.fetchPersonURN(context.Background(), "tok-abc")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "linkedin userinfo:") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid_token"}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		_, err := lc.fetchPersonURN(context.Background(), "tok-abc")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "HTTP 401") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("decode error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`not json`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		_, err := lc.fetchPersonURN(context.Background(), "tok-abc")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "linkedin userinfo decode") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("empty sub field", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"sub":""}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		_, err := lc.fetchPersonURN(context.Background(), "tok-abc")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "sub field is empty") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// — publish ————————————————————————————————————————————————————————————————————

func TestLinkedinPublish(t *testing.T) {
	t.Run("missing ActorID returns terminal publishError without HTTP call", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("server should not be called when ActorID is missing")
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		_, err := lc.publish(context.Background(), ScheduledPost{Body: "hello"}, PlatformCredential{})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		pe, ok := err.(*publishError)
		if !ok {
			t.Fatalf("expected *publishError, got %T: %v", err, err)
		}
		if !pe.terminal {
			t.Errorf("expected terminal=true for missing ActorID")
		}
		if !strings.Contains(pe.msg, "actor_id") {
			t.Errorf("unexpected message: %s", pe.msg)
		}
	})

	t.Run("request build error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("server should not be called when request build fails")
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		cred := PlatformCredential{ActorID: "urn:li:person:123", accessToken: "tok"}
		//nolint:staticcheck // intentionally passing nil context to force http.NewRequestWithContext to fail
		_, err := lc.publish(nil, ScheduledPost{Body: "hello"}, cred)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "linkedin post request") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("Do error", func(t *testing.T) {
		lc := &linkedinClient{hc: &http.Client{Transport: erroringTransport{}}}
		cred := PlatformCredential{ActorID: "urn:li:person:123", accessToken: "tok"}
		_, err := lc.publish(context.Background(), ScheduledPost{Body: "hello"}, cred)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "linkedin post:") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("429 returns rateLimitError honouring Retry-After", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate_limited"}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		cred := PlatformCredential{ActorID: "urn:li:person:123", accessToken: "tok"}
		_, err := lc.publish(context.Background(), ScheduledPost{Body: "hello"}, cred)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		rle, ok := err.(*rateLimitError)
		if !ok {
			t.Fatalf("expected *rateLimitError, got %T: %v", err, err)
		}
		if rle.retryAfter != 120*time.Second {
			t.Errorf("retryAfter = %s, want %s", rle.retryAfter, 120*time.Second)
		}
	})

	t.Run("default non-2xx returns publishError (terminal for 403)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		cred := PlatformCredential{ActorID: "urn:li:person:123", accessToken: "tok"}
		_, err := lc.publish(context.Background(), ScheduledPost{Body: "hello"}, cred)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		pe, ok := err.(*publishError)
		if !ok {
			t.Fatalf("expected *publishError, got %T: %v", err, err)
		}
		if !pe.terminal {
			t.Errorf("expected terminal=true for 403")
		}
		if pe.statusCode != http.StatusForbidden {
			t.Errorf("statusCode = %d, want %d", pe.statusCode, http.StatusForbidden)
		}
	})

	t.Run("default non-2xx returns publishError (non-terminal for 500)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"server_error"}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		cred := PlatformCredential{ActorID: "urn:li:person:123", accessToken: "tok"}
		_, err := lc.publish(context.Background(), ScheduledPost{Body: "hello"}, cred)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		pe, ok := err.(*publishError)
		if !ok {
			t.Fatalf("expected *publishError, got %T: %v", err, err)
		}
		if pe.terminal {
			t.Errorf("expected terminal=false for 500")
		}
	})

	t.Run("success 201 with valid ID", func(t *testing.T) {
		var gotAuth, gotContentType, gotRestli string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v2/ugcPosts" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			gotAuth = r.Header.Get("Authorization")
			gotContentType = r.Header.Get("Content-Type")
			gotRestli = r.Header.Get("X-Restli-Protocol-Version")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"urn:li:share:12345"}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		cred := PlatformCredential{ActorID: "urn:li:person:123", accessToken: "tok-xyz"}
		id, err := lc.publish(context.Background(), ScheduledPost{Body: "hello world"}, cred)
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if id != "urn:li:share:12345" {
			t.Errorf("id = %q, want %q", id, "urn:li:share:12345")
		}
		if gotAuth != "Bearer tok-xyz" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if gotContentType != "application/json" {
			t.Errorf("Content-Type = %q", gotContentType)
		}
		if gotRestli != "2.0.0" {
			t.Errorf("X-Restli-Protocol-Version = %q", gotRestli)
		}
	})

	// This asserts EXISTING behavior, not a fix: when the LinkedIn API returns
	// a 200/201 response whose body fails to JSON-decode (or decodes with an
	// empty "id"), publish swallows the decode error and returns ("", nil)
	// rather than surfacing an error. The post was in fact created on
	// LinkedIn's side, so publish treats it as a (quiet) success. This is a
	// known quirk, already flagged to the architect — out of scope to change
	// here.
	t.Run("200 with malformed body is swallowed as (empty, nil) — known quirk", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`not json`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		cred := PlatformCredential{ActorID: "urn:li:person:123", accessToken: "tok"}
		id, err := lc.publish(context.Background(), ScheduledPost{Body: "hello"}, cred)
		if err != nil {
			t.Fatalf("expected nil error for malformed 200 body (existing swallow-quirk), got: %v", err)
		}
		if id != "" {
			t.Errorf("expected empty platformID for malformed 200 body, got %q", id)
		}
	})

	t.Run("200 with empty id field is also swallowed as (empty, nil)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":""}`))
		}))
		defer srv.Close()

		lc := newTestLinkedinClient(srv)
		cred := PlatformCredential{ActorID: "urn:li:person:123", accessToken: "tok"}
		id, err := lc.publish(context.Background(), ScheduledPost{Body: "hello"}, cred)
		if err != nil {
			t.Fatalf("expected nil error for empty id field (existing swallow-quirk), got: %v", err)
		}
		if id != "" {
			t.Errorf("expected empty platformID, got %q", id)
		}
	})
}

// — linkedinDisplayName ——————————————————————————————————————————————————————

func TestLinkedinDisplayName(t *testing.T) {
	tests := []struct {
		name      string
		personURN string
		want      string
	}{
		{"person URN", "urn:li:person:12345", "linkedin:12345"},
		{"person URN with empty sub", "urn:li:person:", "linkedin:"},
		{"non-person URN falls back", "urn:li:organization:999", "linkedin"},
		{"empty string falls back", "", "linkedin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := linkedinDisplayName(tc.personURN)
			if got != tc.want {
				t.Errorf("linkedinDisplayName(%q) = %q, want %q", tc.personURN, got, tc.want)
			}
		})
	}
}

// — linkedinExpiresAt ————————————————————————————————————————————————————————

func TestLinkedinExpiresAt(t *testing.T) {
	t.Run("zero returns nil", func(t *testing.T) {
		if got := linkedinExpiresAt(0); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
	t.Run("negative returns nil", func(t *testing.T) {
		if got := linkedinExpiresAt(-100); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
	t.Run("positive returns time roughly expiresIn seconds from now", func(t *testing.T) {
		before := time.Now().UTC()
		got := linkedinExpiresAt(3600)
		after := time.Now().UTC()
		if got == nil {
			t.Fatal("expected non-nil time")
		}
		wantMin := before.Add(3600 * time.Second)
		wantMax := after.Add(3600 * time.Second)
		if got.Before(wantMin) || got.After(wantMax) {
			t.Errorf("expiresAt = %v, want between %v and %v", got, wantMin, wantMax)
		}
	})
}

// — handleLinkedInCallback ————————————————————————————————————————————————————

// newLinkedInCallbackTestSocial creates a Social backed by db with a
// linkedinClient wired to route outbound HTTP through srv.
func newLinkedInCallbackTestSocial(t *testing.T, db *sql.DB, srv *httptest.Server, successURL string) *Social {
	t.Helper()
	s := New(db, Config{Secret: refreshTestSecret})
	s.linkedin = &linkedinClient{
		cfg: LinkedInConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			RedirectURL:  "https://example.com/oauth/linkedin/callback",
			SuccessURL:   successURL,
		},
		hc: &http.Client{Transport: rewriteTransport{base: srv.URL, inner: http.DefaultTransport}},
	}
	return s
}

func TestHandleLinkedInCallback(t *testing.T) {
	t.Run("oauthError query param present returns 400", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("server should not be called")
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		s := newLinkedInCallbackTestSocial(t, db, srv, "")

		req := httptest.NewRequest(http.MethodGet, "/oauth/linkedin/callback?error=access_denied", nil)
		w := httptest.NewRecorder()
		s.handleLinkedInCallback(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if !strings.Contains(w.Body.String(), "access_denied") {
			t.Errorf("body = %q, want it to contain %q", w.Body.String(), "access_denied")
		}
	})

	t.Run("missing code or state returns 400", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("server should not be called")
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		s := newLinkedInCallbackTestSocial(t, db, srv, "")

		req := httptest.NewRequest(http.MethodGet, "/oauth/linkedin/callback", nil)
		w := httptest.NewRecorder()
		s.handleLinkedInCallback(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if !strings.Contains(w.Body.String(), "missing code or state") {
			t.Errorf("body = %q", w.Body.String())
		}
	})

	t.Run("unknown state returns 400", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("server should not be called")
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		s := newLinkedInCallbackTestSocial(t, db, srv, "")

		req := httptest.NewRequest(http.MethodGet, "/oauth/linkedin/callback?code=auth-code&state=does-not-exist", nil)
		w := httptest.NewRecorder()
		s.handleLinkedInCallback(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if !strings.Contains(w.Body.String(), "invalid or expired OAuth state") {
			t.Errorf("body = %q", w.Body.String())
		}
	})

	t.Run("state belongs to a different platform returns 400", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("server should not be called")
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		s := newLinkedInCallbackTestSocial(t, db, srv, "")

		if err := insertOAuthState(s.creds.db, "wrong-platform-state", "mastodon", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/linkedin/callback?code=auth-code&state=wrong-platform-state", nil)
		w := httptest.NewRecorder()
		s.handleLinkedInCallback(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if !strings.Contains(w.Body.String(), "unexpected platform: mastodon") {
			t.Errorf("body = %q", w.Body.String())
		}
	})

	t.Run("exchangeCode failure returns 502", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/oauth/v2/accessToken" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			t.Errorf("unexpected call to %s", r.URL.Path)
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		s := newLinkedInCallbackTestSocial(t, db, srv, "")

		if err := insertOAuthState(s.creds.db, "exchange-fail-state", "linkedin", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/linkedin/callback?code=auth-code&state=exchange-fail-state", nil)
		w := httptest.NewRecorder()
		s.handleLinkedInCallback(w, req)

		if w.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
		}
		if !strings.Contains(w.Body.String(), "token exchange failed") {
			t.Errorf("body = %q", w.Body.String())
		}
	})

	t.Run("fetchPersonURN failure returns 502", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/oauth/v2/accessToken":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"access_token":"tok-abc","expires_in":3600}`))
			case "/v2/userinfo":
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":"server_error"}`))
			default:
				t.Errorf("unexpected call to %s", r.URL.Path)
			}
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		s := newLinkedInCallbackTestSocial(t, db, srv, "")

		if err := insertOAuthState(s.creds.db, "userinfo-fail-state", "linkedin", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/linkedin/callback?code=auth-code&state=userinfo-fail-state", nil)
		w := httptest.NewRecorder()
		s.handleLinkedInCallback(w, req)

		if w.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
		}
		if !strings.Contains(w.Body.String(), "failed to fetch LinkedIn person URN") {
			t.Errorf("body = %q", w.Body.String())
		}
	})

	t.Run("credential upsert failure returns 500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/oauth/v2/accessToken":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"access_token":"tok-abc","expires_in":3600}`))
			case "/v2/userinfo":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"sub":"abc123"}`))
			default:
				t.Errorf("unexpected call to %s", r.URL.Path)
			}
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		s := newLinkedInCallbackTestSocial(t, db, srv, "")

		if err := insertOAuthState(s.creds.db, "upsert-fail-state", "linkedin", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		// Force upsertCredentialByInstance to fail while leaving the OAuth
		// states table (already consumed by this point in the flow) intact:
		// drop the credentials table out from under the store.
		if _, err := db.Exec(`DROP TABLE smeldr_social_credentials`); err != nil {
			t.Fatalf("drop credentials table: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/linkedin/callback?code=auth-code&state=upsert-fail-state", nil)
		w := httptest.NewRecorder()
		s.handleLinkedInCallback(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
		}
		if !strings.Contains(w.Body.String(), "failed to save credential") {
			t.Errorf("body = %q", w.Body.String())
		}
	})

	t.Run("success without SuccessURL returns 200 HTML confirmation", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/oauth/v2/accessToken":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"access_token":"tok-abc","expires_in":3600}`))
			case "/v2/userinfo":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"sub":"abc123"}`))
			default:
				t.Errorf("unexpected call to %s", r.URL.Path)
			}
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		s := newLinkedInCallbackTestSocial(t, db, srv, "")

		if err := insertOAuthState(s.creds.db, "success-state", "linkedin", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/linkedin/callback?code=auth-code&state=success-state", nil)
		w := httptest.NewRecorder()
		s.handleLinkedInCallback(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if !strings.Contains(w.Body.String(), "Connected to LinkedIn successfully") {
			t.Errorf("body = %q", w.Body.String())
		}

		creds, err := s.creds.listCredentials()
		if err != nil {
			t.Fatalf("listCredentials: %v", err)
		}
		if len(creds) != 1 {
			t.Fatalf("expected 1 credential, got %d", len(creds))
		}
		c := creds[0]
		if c.Platform != "linkedin" {
			t.Errorf("Platform = %q, want %q", c.Platform, "linkedin")
		}
		if c.ActorID != "urn:li:person:abc123" {
			t.Errorf("ActorID = %q, want %q", c.ActorID, "urn:li:person:abc123")
		}
		if c.Name != "linkedin:abc123" {
			t.Errorf("Name = %q, want %q", c.Name, "linkedin:abc123")
		}
		if c.InstanceURL != linkedinInstanceURL {
			t.Errorf("InstanceURL = %q, want %q", c.InstanceURL, linkedinInstanceURL)
		}
		if c.ExpiresAt == nil {
			t.Error("expected ExpiresAt to be set")
		}
	})

	t.Run("success with SuccessURL redirects", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/oauth/v2/accessToken":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"access_token":"tok-abc","expires_in":3600}`))
			case "/v2/userinfo":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"sub":"xyz789"}`))
			default:
				t.Errorf("unexpected call to %s", r.URL.Path)
			}
		}))
		defer srv.Close()

		db := openRefreshTestDB(t)
		s := newLinkedInCallbackTestSocial(t, db, srv, "https://example.com/connected")

		if err := insertOAuthState(s.creds.db, "success-redirect-state", "linkedin", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/oauth/linkedin/callback?code=auth-code&state=success-redirect-state", nil)
		w := httptest.NewRecorder()
		s.handleLinkedInCallback(w, req)

		if w.Code != http.StatusFound {
			t.Errorf("status = %d, want %d", w.Code, http.StatusFound)
		}
		if got := w.Header().Get("Location"); got != "https://example.com/connected" {
			t.Errorf("Location = %q, want %q", got, "https://example.com/connected")
		}
	})
}

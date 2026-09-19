package social

// This file adds coverage for mastodon.go's HTTP-calling functions
// (exchangeCode, uploadMedia, postStatus, publish), its standalone helpers
// (parseRetryAfter, effectiveScope, isTerminalStatus, truncate), and three
// oauth.go functions (consumeOAuthState, purgeExpiredOAuthStates,
// handleMastodonCallback).
//
// Two error paths are intentionally left untested — noted again at their
// call sites below:
//   - uploadMedia's CreateFormFile / io.Copy / request-build errors: these
//     only fail on OS-level I/O or buffer-allocation failures and are not
//     independently forceable without disproportionate effort.
//   - postStatus's json.Marshal error: the payload is built only from a
//     plain string body and, optionally, a []string media ID slice, so
//     json.Marshal cannot fail for it. The branch is unreachable via the
//     public API.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// — shared fault-injection helpers ——————————————————————————————————————————
//
// errTransport / errReadCloser / errBodyTransport are also used by
// twitter_test.go (same package) — defined once here since this patch lands
// first, reused there rather than redeclared.

// errTransport always fails at the RoundTrip stage, simulating a network
// error before any response is received (covers the httpClient.Do(req)
// error branch in exchangeCode/fetchPersonURN/publish-style functions).
type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("simulated network error")
}

// errReadCloser is an io.ReadCloser whose Read always fails. Paired with
// errBodyTransport to simulate a response body read error after a
// successful HTTP round-trip.
type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errors.New("simulated read error") }
func (errReadCloser) Close() error             { return nil }

// errBodyTransport returns a synthetic response with statusCode and a body
// that fails on Read, without making any real network call.
type errBodyTransport struct{ statusCode int }

func (tr errBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: tr.statusCode,
		Body:       errReadCloser{},
		Header:     make(http.Header),
	}, nil
}

// postErrTransport lets GET requests through to the real transport but
// fails every POST. Used to force the *second* HTTP call in a two-step flow
// (uploadMedia's upload step) to fail while the first (the GET fetch) still
// succeeds. Not defined elsewhere in the package: exchangeCode and postStatus
// only ever make one call, so errTransport (which fails every call) is
// enough for them.
type postErrTransport struct{}

func (postErrTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost {
		return nil, fmt.Errorf("forced upload transport error")
	}
	return http.DefaultTransport.RoundTrip(req)
}

// — exchangeCode ——————————————————————————————————————————————————————————————

func TestMastodonExchangeCode(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotContentType, gotVerifier string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotContentType = r.Header.Get("Content-Type")
			_ = r.ParseForm()
			gotVerifier = r.PostForm.Get("code_verifier")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"tok123","token_type":"Bearer","scope":"write:statuses write:media","created_at":1690000000,"refresh_token":"rt1","expires_in":3600}`))
		}))
		defer srv.Close()

		c := &mastodonClient{
			cfg: MastodonConfig{
				InstanceURL:  srv.URL,
				ClientID:     "cid",
				ClientSecret: "csec",
				RedirectURL:  "http://app.example/callback",
			},
			httpClient: srv.Client(),
		}
		tr, err := c.exchangeCode(context.Background(), "code123", "verifier1")
		if err != nil {
			t.Fatalf("exchangeCode: %v", err)
		}
		if tr.AccessToken != "tok123" {
			t.Errorf("AccessToken = %q, want %q", tr.AccessToken, "tok123")
		}
		if tr.RefreshToken != "rt1" {
			t.Errorf("RefreshToken = %q, want %q", tr.RefreshToken, "rt1")
		}
		if tr.ExpiresIn != 3600 {
			t.Errorf("ExpiresIn = %d, want 3600", tr.ExpiresIn)
		}
		if gotContentType != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", gotContentType)
		}
		if gotVerifier != "verifier1" {
			t.Errorf("code_verifier = %q, want %q", gotVerifier, "verifier1")
		}
	})

	t.Run("request build error", func(t *testing.T) {
		// An invalid port makes url.Parse (called inside
		// http.NewRequestWithContext) fail deterministically.
		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: "http://foo:bar"}, httpClient: &http.Client{}}
		_, err := c.exchangeCode(context.Background(), "code", "")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("Do error", func(t *testing.T) {
		c := &mastodonClient{
			cfg:        MastodonConfig{InstanceURL: "http://127.0.0.1:1"},
			httpClient: &http.Client{Transport: errTransport{}},
		}
		_, err := c.exchangeCode(context.Background(), "code", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "token exchange:") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("read body error", func(t *testing.T) {
		c := &mastodonClient{
			cfg:        MastodonConfig{InstanceURL: "http://127.0.0.1:1"},
			httpClient: &http.Client{Transport: errBodyTransport{statusCode: http.StatusOK}},
		}
		_, err := c.exchangeCode(context.Background(), "code", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "token exchange read") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
		}))
		defer srv.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		_, err := c.exchangeCode(context.Background(), "code", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "HTTP 400") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("malformed JSON response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`not-json`))
		}))
		defer srv.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		_, err := c.exchangeCode(context.Background(), "code", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "token exchange parse") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// — uploadMedia ——————————————————————————————————————————————————————————————

func TestUploadMedia(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("fake-image-bytes"))
		}))
		defer origin.Close()

		var gotAuth, gotDescription string
		instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			gotDescription = r.FormValue("description")
			if _, _, err := r.FormFile("file"); err != nil {
				t.Errorf("expected file field: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"m1","type":"image"}`))
		}))
		defer instance.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: instance.URL}, httpClient: &http.Client{}}
		id, err := c.uploadMedia(context.Background(), "tok", origin.URL+"/img.png", "alt text")
		if err != nil {
			t.Fatalf("uploadMedia: %v", err)
		}
		if id != "m1" {
			t.Errorf("id = %q, want %q", id, "m1")
		}
		if gotAuth != "Bearer tok" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if gotDescription != "alt text" {
			t.Errorf("description = %q, want %q", gotDescription, "alt text")
		}
	})

	t.Run("fetch non-200", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer origin.Close()

		instanceCalled := false
		instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			instanceCalled = true
			w.WriteHeader(http.StatusOK)
		}))
		defer instance.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: instance.URL}, httpClient: &http.Client{}}
		_, err := c.uploadMedia(context.Background(), "tok", origin.URL+"/missing.png", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "fetch media") || !strings.Contains(err.Error(), "404") {
			t.Errorf("unexpected error: %v", err)
		}
		if instanceCalled {
			t.Error("instance media endpoint should not be called when fetch fails")
		}
	})

	t.Run("fetch Do error", func(t *testing.T) {
		c := &mastodonClient{
			cfg:        MastodonConfig{InstanceURL: "http://127.0.0.1:1"},
			httpClient: &http.Client{Transport: errTransport{}},
		}
		_, err := c.uploadMedia(context.Background(), "tok", "http://127.0.0.1:1/img.png", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "fetch media") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	// uploadMedia's CreateFormFile / io.Copy / request-build error paths are
	// skipped here (see file header comment): they are not independently
	// forceable without disproportionate effort.

	t.Run("upload non-2xx returns publishError", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("img"))
		}))
		defer origin.Close()

		instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
		}))
		defer instance.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: instance.URL}, httpClient: &http.Client{}}
		_, err := c.uploadMedia(context.Background(), "tok", origin.URL+"/img.png", "")
		if err == nil {
			t.Fatal("expected error")
		}
		pe, ok := err.(*publishError)
		if !ok {
			t.Fatalf("expected *publishError, got %T: %v", err, err)
		}
		if !pe.IsTerminal() {
			t.Error("expected IsTerminal()=true for 403")
		}
	})

	t.Run("upload Do error", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("img"))
		}))
		defer origin.Close()

		c := &mastodonClient{
			cfg:        MastodonConfig{InstanceURL: "http://127.0.0.1:1"},
			httpClient: &http.Client{Transport: postErrTransport{}},
		}
		_, err := c.uploadMedia(context.Background(), "tok", origin.URL+"/img.png", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "upload media") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("malformed response JSON", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("img"))
		}))
		defer origin.Close()

		instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("not-json"))
		}))
		defer instance.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: instance.URL}, httpClient: &http.Client{}}
		_, err := c.uploadMedia(context.Background(), "tok", origin.URL+"/img.png", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "upload media parse") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// — postStatus ————————————————————————————————————————————————————————————————

func TestPostStatus(t *testing.T) {
	// postStatus's json.Marshal error path is skipped here (see file header
	// comment): the payload is built only from a plain string and,
	// optionally, a []string, so json.Marshal cannot fail for it.

	t.Run("success without media", func(t *testing.T) {
		var gotAuth, gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"s1","url":"https://example.social/@u/s1"}`))
		}))
		defer srv.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		id, err := c.postStatus(context.Background(), "tok", "hello world", "")
		if err != nil {
			t.Fatalf("postStatus: %v", err)
		}
		if id != "s1" {
			t.Errorf("id = %q, want %q", id, "s1")
		}
		if gotAuth != "Bearer tok" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if strings.Contains(gotBody, "media_ids") {
			t.Errorf("unexpected media_ids in body: %s", gotBody)
		}
	})

	t.Run("success with media", func(t *testing.T) {
		var gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"s2","url":"https://example.social/@u/s2"}`))
		}))
		defer srv.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		id, err := c.postStatus(context.Background(), "tok", "hi", "m1")
		if err != nil {
			t.Fatalf("postStatus: %v", err)
		}
		if id != "s2" {
			t.Errorf("id = %q, want %q", id, "s2")
		}
		if !strings.Contains(gotBody, `"media_ids":["m1"]`) {
			t.Errorf("expected media_ids in body, got: %s", gotBody)
		}
	})

	t.Run("request build error", func(t *testing.T) {
		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: "http://foo:bar"}, httpClient: &http.Client{}}
		_, err := c.postStatus(context.Background(), "tok", "hi", "")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("Do error", func(t *testing.T) {
		c := &mastodonClient{
			cfg:        MastodonConfig{InstanceURL: "http://127.0.0.1:1"},
			httpClient: &http.Client{Transport: errTransport{}},
		}
		_, err := c.postStatus(context.Background(), "tok", "hi", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "post status:") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("429 returns rateLimitError", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		_, err := c.postStatus(context.Background(), "tok", "hi", "")
		if err == nil {
			t.Fatal("expected error")
		}
		rle, ok := err.(*rateLimitError)
		if !ok {
			t.Fatalf("expected *rateLimitError, got %T: %v", err, err)
		}
		if rle.retryAfter != 120*time.Second {
			t.Errorf("retryAfter = %v, want 120s", rle.retryAfter)
		}
		if !strings.Contains(rle.Error(), "rate limited") {
			t.Errorf("unexpected Error(): %s", rle.Error())
		}
	})

	t.Run("non-200 returns publishError", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("boom"))
		}))
		defer srv.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		_, err := c.postStatus(context.Background(), "tok", "hi", "")
		if err == nil {
			t.Fatal("expected error")
		}
		pe, ok := err.(*publishError)
		if !ok {
			t.Fatalf("expected *publishError, got %T: %v", err, err)
		}
		if pe.terminal {
			t.Error("expected terminal=false for 500")
		}
		if !strings.Contains(pe.Error(), "post status") {
			t.Errorf("unexpected message: %s", pe.Error())
		}
	})

	t.Run("response unmarshal error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("not-json"))
		}))
		defer srv.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		_, err := c.postStatus(context.Background(), "tok", "hi", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "post status parse") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// — publish ———————————————————————————————————————————————————————————————————

func TestMastodonPublish(t *testing.T) {
	t.Run("success without media", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/statuses" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"p1"}`))
		}))
		defer srv.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: srv.URL}, httpClient: srv.Client()}
		id, err := c.publish(context.Background(), ScheduledPost{Body: "hello"}, PlatformCredential{})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if id != "p1" {
			t.Errorf("id = %q, want %q", id, "p1")
		}
	})

	t.Run("success with media", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("img"))
		}))
		defer origin.Close()

		instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v2/media":
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"id":"m1"}`))
			case "/api/v1/statuses":
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"id":"p2"}`))
			default:
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
		}))
		defer instance.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: instance.URL}, httpClient: &http.Client{}}
		id, err := c.publish(context.Background(),
			ScheduledPost{Body: "hi", MediaURL: origin.URL + "/img.png", AltText: "alt"},
			PlatformCredential{},
		)
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if id != "p2" {
			t.Errorf("id = %q, want %q", id, "p2")
		}
	})

	t.Run("uploadMedia error propagates without calling postStatus", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer origin.Close()

		statusesCalled := false
		instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/statuses" {
				statusesCalled = true
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer instance.Close()

		c := &mastodonClient{cfg: MastodonConfig{InstanceURL: instance.URL}, httpClient: &http.Client{}}
		_, err := c.publish(context.Background(),
			ScheduledPost{Body: "hi", MediaURL: origin.URL + "/missing.png"},
			PlatformCredential{},
		)
		if err == nil {
			t.Fatal("expected error")
		}
		if statusesCalled {
			t.Error("postStatus must not be called when uploadMedia fails")
		}
	})
}

// — parseRetryAfter ——————————————————————————————————————————————————————————

func TestParseRetryAfter(t *testing.T) {
	t.Run("empty returns fallback", func(t *testing.T) {
		if got := parseRetryAfter(""); got != 5*time.Minute {
			t.Errorf("got %v, want 5m", got)
		}
	})

	t.Run("numeric seconds", func(t *testing.T) {
		if got := parseRetryAfter("120"); got != 120*time.Second {
			t.Errorf("got %v, want 120s", got)
		}
	})

	t.Run("HTTP-date in the future", func(t *testing.T) {
		future := time.Now().Add(2 * time.Hour).UTC()
		got := parseRetryAfter(future.Format(http.TimeFormat))
		if got <= time.Hour+58*time.Minute || got > 2*time.Hour+2*time.Minute {
			t.Errorf("got %v, want ~2h", got)
		}
	})

	t.Run("HTTP-date in the past falls back", func(t *testing.T) {
		past := time.Now().Add(-2 * time.Hour).UTC()
		if got := parseRetryAfter(past.Format(http.TimeFormat)); got != 5*time.Minute {
			t.Errorf("got %v, want 5m fallback", got)
		}
	})

	t.Run("malformed value falls back", func(t *testing.T) {
		if got := parseRetryAfter("not-a-valid-retry-after"); got != 5*time.Minute {
			t.Errorf("got %v, want 5m fallback", got)
		}
	})
}

// — effectiveScope (mastodon) ————————————————————————————————————————————————

func TestMastodonEffectiveScope(t *testing.T) {
	t.Run("empty scopes returns default", func(t *testing.T) {
		c := &mastodonClient{}
		if got := c.effectiveScope(); got != "write:statuses write:media" {
			t.Errorf("got %q, want %q", got, "write:statuses write:media")
		}
	})

	t.Run("custom scopes are joined with a space", func(t *testing.T) {
		c := &mastodonClient{cfg: MastodonConfig{Scopes: []string{"read", "write:media"}}}
		if got := c.effectiveScope(); got != "read write:media" {
			t.Errorf("got %q, want %q", got, "read write:media")
		}
	})
}

// — isTerminalStatus —————————————————————————————————————————————————————————

func TestIsTerminalStatus(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusOK, false},
		{http.StatusNotFound, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			if got := isTerminalStatus(tc.code); got != tc.want {
				t.Errorf("isTerminalStatus(%d) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// — truncate ——————————————————————————————————————————————————————————————————

func TestTruncate(t *testing.T) {
	t.Run("shorter than limit returns unchanged", func(t *testing.T) {
		if got := truncate("hello", 10); got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("exact boundary returns unchanged", func(t *testing.T) {
		s := strings.Repeat("a", 8)
		if got := truncate(s, 8); got != s {
			t.Errorf("got %q, want %q", got, s)
		}
	})

	t.Run("longer than limit is truncated with ellipsis", func(t *testing.T) {
		s := strings.Repeat("a", 10)
		got := truncate(s, 8)
		want := strings.Repeat("a", 8) + "…"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// — oauth.go: consumeOAuthState ————————————————————————————————————————————

func TestConsumeOAuthState(t *testing.T) {
	t.Run("generic query error propagates", func(t *testing.T) {
		db := openRefreshTestDB(t)
		// Drop the table so the initial QueryRowContext fails with a real
		// driver error that is not sql.ErrNoRows, exercising the "err != nil"
		// branch distinct from the unknown-state (ErrNoRows) branch below.
		if _, err := db.Exec("DROP TABLE smeldr_social_oauth_states"); err != nil {
			t.Fatalf("drop smeldr_social_oauth_states: %v", err)
		}
		_, _, err := consumeOAuthState(db, "any-state")
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), "unknown OAuth state") {
			t.Errorf("expected a generic driver error, not the unknown-state error: %v", err)
		}
	})

	t.Run("unknown state", func(t *testing.T) {
		db := openRefreshTestDB(t)
		_, _, err := consumeOAuthState(db, "does-not-exist")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "unknown OAuth state") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("expired state", func(t *testing.T) {
		db := openRefreshTestDB(t)
		old := time.Now().UTC().Add(-oauthStateTTL - time.Minute)
		if _, err := db.ExecContext(context.Background(), `
			INSERT INTO smeldr_social_oauth_states (state, platform, code_verifier, created_at)
			VALUES (?, ?, ?, ?)`,
			"expired-state", "mastodon", "", old,
		); err != nil {
			t.Fatalf("seed expired state: %v", err)
		}

		_, _, err := consumeOAuthState(db, "expired-state")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "expired") {
			t.Errorf("unexpected error: %v", err)
		}

		// State is single-use: it must have been deleted even though it was
		// rejected as expired.
		_, _, err = consumeOAuthState(db, "expired-state")
		if err == nil || !strings.Contains(err.Error(), "unknown OAuth state") {
			t.Errorf("expected second consume to see the state as unknown, got: %v", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		db := openRefreshTestDB(t)
		if err := insertOAuthState(db, "good-state", "mastodon", "verifier-x"); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}
		platform, verifier, err := consumeOAuthState(db, "good-state")
		if err != nil {
			t.Fatalf("consumeOAuthState: %v", err)
		}
		if platform != "mastodon" {
			t.Errorf("platform = %q, want %q", platform, "mastodon")
		}
		if verifier != "verifier-x" {
			t.Errorf("verifier = %q, want %q", verifier, "verifier-x")
		}
	})
}

// — oauth.go: purgeExpiredOAuthStates ————————————————————————————————————————

func TestPurgeExpiredOAuthStates(t *testing.T) {
	t.Run("success removes only expired rows", func(t *testing.T) {
		db := openRefreshTestDB(t)
		ctx := context.Background()
		old := time.Now().UTC().Add(-oauthStateTTL - time.Minute)
		fresh := time.Now().UTC()

		if _, err := db.ExecContext(ctx, `
			INSERT INTO smeldr_social_oauth_states (state, platform, code_verifier, created_at)
			VALUES (?, ?, ?, ?)`,
			"old-state", "mastodon", "", old,
		); err != nil {
			t.Fatalf("seed old state: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO smeldr_social_oauth_states (state, platform, code_verifier, created_at)
			VALUES (?, ?, ?, ?)`,
			"fresh-state", "mastodon", "", fresh,
		); err != nil {
			t.Fatalf("seed fresh state: %v", err)
		}

		if err := purgeExpiredOAuthStates(db); err != nil {
			t.Fatalf("purgeExpiredOAuthStates: %v", err)
		}

		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM smeldr_social_oauth_states WHERE state='old-state'`,
		).Scan(&count); err != nil {
			t.Fatalf("count old-state: %v", err)
		}
		if count != 0 {
			t.Errorf("expected old-state purged, got count=%d", count)
		}

		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM smeldr_social_oauth_states WHERE state='fresh-state'`,
		).Scan(&count); err != nil {
			t.Fatalf("count fresh-state: %v", err)
		}
		if count != 1 {
			t.Errorf("expected fresh-state retained, got count=%d", count)
		}
	})

	t.Run("exec error propagates", func(t *testing.T) {
		// Force the DELETE to fail with a real driver error, without a
		// custom smeldr.DB implementation (smeldr.DB is documented as "do
		// not implement directly" — see storage.go, and this file's own
		// handleMastodonCallback credential-upsert-failure test below):
		// drop the table the statement targets.
		db := openRefreshTestDB(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_oauth_states"); err != nil {
			t.Fatalf("drop smeldr_social_oauth_states: %v", err)
		}
		if err := purgeExpiredOAuthStates(db); err == nil {
			t.Fatal("expected error from Exec failure")
		}
	})
}

// — oauth.go: handleMastodonCallback ————————————————————————————————————————

// newCallbackSocial creates a Social instance backed by a fresh in-memory DB
// for handleMastodonCallback tests.
func newCallbackSocial(t *testing.T) (*Social, *sql.DB) {
	t.Helper()
	db := openRefreshTestDB(t)
	s := New(db, Config{Secret: refreshTestSecret})
	return s, db
}

func TestHandleMastodonCallback(t *testing.T) {
	t.Run("oauth error query param", func(t *testing.T) {
		s, _ := newCallbackSocial(t)
		req := httptest.NewRequest(http.MethodGet, "/oauth/mastodon/callback?error=access_denied", nil)
		rec := httptest.NewRecorder()
		s.handleMastodonCallback(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("missing code and state", func(t *testing.T) {
		s, _ := newCallbackSocial(t)
		req := httptest.NewRequest(http.MethodGet, "/oauth/mastodon/callback", nil)
		rec := httptest.NewRecorder()
		s.handleMastodonCallback(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("unknown state", func(t *testing.T) {
		s, _ := newCallbackSocial(t)
		req := httptest.NewRequest(http.MethodGet, "/oauth/mastodon/callback?code=abc&state=does-not-exist", nil)
		rec := httptest.NewRecorder()
		s.handleMastodonCallback(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("state belongs to a different platform", func(t *testing.T) {
		s, _ := newCallbackSocial(t)
		if err := insertOAuthState(s.creds.db, "linkedin-state", "linkedin", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth/mastodon/callback?code=abc&state=linkedin-state", nil)
		rec := httptest.NewRecorder()
		s.handleMastodonCallback(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "unexpected platform") {
			t.Errorf("body = %q", rec.Body.String())
		}
	})

	t.Run("exchangeCode failure returns 502", func(t *testing.T) {
		s, _ := newCallbackSocial(t)
		badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
		}))
		defer badSrv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: badSrv.URL}, httpClient: badSrv.Client()}

		if err := insertOAuthState(s.creds.db, "state-exchange-fail", "mastodon", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth/mastodon/callback?code=abc&state=state-exchange-fail", nil)
		rec := httptest.NewRecorder()
		s.handleMastodonCallback(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", rec.Code)
		}
	})

	t.Run("credential upsert failure returns 500", func(t *testing.T) {
		s, db := newCallbackSocial(t)
		okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","created_at":1,"expires_in":3600}`))
		}))
		defer okSrv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: okSrv.URL}, httpClient: okSrv.Client()}

		// Force upsertCredentialByInstance to fail without a custom smeldr.DB
		// implementation (smeldr.DB is documented as "do not implement
		// directly" — see storage.go, and oauth_test.go's handleXCallback
		// equivalent): drop the table it writes to. The oauth_states table it
		// doesn't touch stays intact, so state consumption and token exchange
		// above still succeed.
		if _, err := db.Exec("DROP TABLE smeldr_social_credentials"); err != nil {
			t.Fatalf("drop smeldr_social_credentials: %v", err)
		}

		if err := insertOAuthState(s.creds.db, "state-cred-fail", "mastodon", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth/mastodon/callback?code=abc&state=state-cred-fail", nil)
		rec := httptest.NewRecorder()
		s.handleMastodonCallback(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("success", func(t *testing.T) {
		s, _ := newCallbackSocial(t)
		okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"tok-success","token_type":"Bearer","created_at":1,"expires_in":3600}`))
		}))
		defer okSrv.Close()
		s.mastodon = &mastodonClient{cfg: MastodonConfig{InstanceURL: okSrv.URL}, httpClient: okSrv.Client()}

		if err := insertOAuthState(s.creds.db, "state-success", "mastodon", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth/mastodon/callback?code=abc&state=state-success", nil)
		rec := httptest.NewRecorder()
		s.handleMastodonCallback(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Connected to Mastodon") {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}

		creds, err := s.creds.listCredentials()
		if err != nil {
			t.Fatalf("listCredentials: %v", err)
		}
		if len(creds) != 1 || creds[0].Platform != "mastodon" {
			t.Errorf("expected one mastodon credential, got %+v", creds)
		}
	})

	t.Run("success with SuccessURL redirects", func(t *testing.T) {
		s, _ := newCallbackSocial(t)
		okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"tok-redirect","token_type":"Bearer","created_at":1,"expires_in":3600}`))
		}))
		defer okSrv.Close()
		s.mastodon = &mastodonClient{
			cfg: MastodonConfig{
				InstanceURL: okSrv.URL,
				SuccessURL:  "https://example.com/connected",
			},
			httpClient: okSrv.Client(),
		}

		if err := insertOAuthState(s.creds.db, "state-redirect", "mastodon", ""); err != nil {
			t.Fatalf("insertOAuthState: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth/mastodon/callback?code=abc&state=state-redirect", nil)
		rec := httptest.NewRecorder()
		s.handleMastodonCallback(rec, req)
		if rec.Code != http.StatusFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
		}
		if loc := rec.Header().Get("Location"); loc != "https://example.com/connected" {
			t.Errorf("Location = %q, want %q", loc, "https://example.com/connected")
		}
	})
}

package social

import (
	"context"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// — uploadXMedia ——————————————————————————————————————————————————————————————

func TestUploadXMedia(t *testing.T) {
	t.Run("happy path returns media ID", func(t *testing.T) {
		// Media origin server.
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("fake-image-bytes"))
		}))
		defer origin.Close()

		// X upload endpoint.
		var gotCategory, gotContentType string
		upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if !strings.HasPrefix(ct, "multipart/") {
				t.Errorf("expected multipart Content-Type, got %q", ct)
			}
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("multipart read: %v", err)
				}
				data, _ := io.ReadAll(part)
				switch part.FormName() {
				case "media_category":
					gotCategory = string(data)
				case "media":
					gotContentType = part.Header.Get("Content-Type")
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"data":{"id":"123456789"}}`))
		}))
		defer upload.Close()

		// Patch the upload URL for this test.
		origURL := xMediaUploadURL
		defer func() { _ = origURL }() // xMediaUploadURL is a const; we test via helper below

		mediaID, err := uploadXMediaTo(t, upload.URL, origin.URL+"/img.jpg", "tok")
		if err != nil {
			t.Fatalf("uploadXMedia: %v", err)
		}
		if mediaID != "123456789" {
			t.Errorf("media ID = %q, want %q", mediaID, "123456789")
		}
		if gotCategory != "tweet_image" {
			t.Errorf("media_category = %q, want %q", gotCategory, "tweet_image")
		}
		if gotContentType != "image/jpeg" {
			t.Errorf("Content-Type in media part = %q, want %q", gotContentType, "image/jpeg")
		}
	})

	t.Run("fetch 404 returns error", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer origin.Close()

		upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("upload endpoint should not be called on fetch failure")
		}))
		defer upload.Close()

		_, err := uploadXMediaTo(t, upload.URL, origin.URL+"/missing.jpg", "tok")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "HTTP 404") {
			t.Errorf("expected HTTP 404 in error, got: %v", err)
		}
	})

	t.Run("upload 403 returns terminal publishError", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("img"))
		}))
		defer origin.Close()

		upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
		}))
		defer upload.Close()

		_, err := uploadXMediaTo(t, upload.URL, origin.URL+"/img.png", "tok")
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
	})
}

// uploadXMediaTo calls uploadXMedia with the upload URL replaced by uploadURL.
// Used in tests to redirect the upload to a local httptest server.
func uploadXMediaTo(t *testing.T, uploadURL, mediaURL, accessToken string) (string, error) {
	t.Helper()
	// We call the internal helper directly using a redirecting transport so that
	// the const xMediaUploadURL is overridden via the http.Client's transport.
	client := &http.Client{
		Transport: &uploadRedirectTransport{
			real:      http.DefaultTransport,
			uploadURL: uploadURL,
		},
	}
	return uploadXMedia(context.Background(), client, accessToken, mediaURL)
}

// uploadRedirectTransport rewrites requests to xMediaUploadURL to uploadURL.
type uploadRedirectTransport struct {
	real      http.RoundTripper
	uploadURL string
}

func (tr *uploadRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() == xMediaUploadURL {
		target, _ := url.Parse(tr.uploadURL)
		newReq := req.Clone(req.Context())
		newReq.URL = target
		return tr.real.RoundTrip(newReq)
	}
	return tr.real.RoundTrip(req)
}

// — xWeightedBodyLen ——————————————————————————————————————————————————————————

func TestXWeightedBodyLen(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "no URL",
			body: strings.Repeat("a", 10),
			want: 10,
		},
		{
			name: "short URL expands to 23",
			// "https://x.co" = 12 chars < xTcoURLLen (23); body raw = 6 + 12 = 18,
			// weighted = 6 + 23 = 29.
			body: "Hello https://x.co",
			want: len([]rune("Hello ")) + 23,
		},
		{
			name: "long URL shrinks to 23",
			// "https://" + 52 "x"s = 60 chars > 23; prefix "Pre: " = 5 chars.
			// raw = 65, weighted = 5 + 23 = 28.
			body: "Pre: https://" + strings.Repeat("x", 52),
			want: 5 + 23,
		},
		{
			name: "two long URLs each weighted to 23",
			// "a " (2) + URL1 (29) + " b " (3) + URL2 (29) = 63 raw;
			// weighted = 2 + 23 + 3 + 23 = 51.
			body: "a https://long1.example.com/foo b https://long2.example.com/bar",
			want: 2 + 23 + 3 + 23,
		},
		{
			name: "body weighted exactly at limit",
			// 256 "a"s + " " + 24-char URL = 281 raw;
			// weighted = 256 + 1 + 23 = 280.
			body: strings.Repeat("a", 256) + " " + "https://x.co/abc12345678",
			want: 280,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := xWeightedBodyLen(tc.body)
			if got != tc.want {
				t.Errorf("xWeightedBodyLen(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

// — publish body length (t.co weighting) ————————————————————————————————————

func TestPublishBodyLen_tcoWeighting(t *testing.T) {
	// longURL is 60 runes — well above xTcoURLLen (23).
	longURL := "https://" + strings.Repeat("x", 52)

	// acceptedBody: raw = 249 + 1 + 60 = 310 (> 280), weighted = 249 + 1 + 23 = 273 (≤ 280).
	acceptedBody := strings.Repeat("a", 249) + " " + longURL

	// rejectedBody: raw = 258 + 1 + 60 = 319 (> 280), weighted = 258 + 1 + 23 = 282 (> 280).
	rejectedBody := strings.Repeat("a", 258) + " " + longURL

	t.Run("raw-over-limit body with long URL is accepted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"data":{"id":"tweet-1"}}`))
		}))
		defer srv.Close()

		tc := &twitterClient{
			httpClient: &http.Client{
				Transport: &xAPIRedirectTransport{apiBase: srv.URL},
			},
		}
		_, err := tc.publish(context.Background(), ScheduledPost{Body: acceptedBody}, PlatformCredential{accessToken: "tok"})
		if err != nil {
			t.Errorf("publish with weighted-valid body should succeed, got: %v", err)
		}
	})

	t.Run("weighted-over-limit body is rejected before HTTP call", func(t *testing.T) {
		tc := &twitterClient{httpClient: &http.Client{}}
		_, err := tc.publish(context.Background(), ScheduledPost{Body: rejectedBody}, PlatformCredential{})
		if err == nil {
			t.Fatal("expected publishError, got nil")
		}
		pe, ok := err.(*publishError)
		if !ok {
			t.Fatalf("expected *publishError, got %T: %v", err, err)
		}
		if !pe.terminal {
			t.Errorf("expected terminal=true for body-length error")
		}
		if !strings.Contains(pe.msg, "character limit") {
			t.Errorf("unexpected error message: %s", pe.msg)
		}
	})
}

// xAPIRedirectTransport rewrites requests aimed at xAPIBase to apiBase.
// Used in tests to avoid real network calls to api.twitter.com.
type xAPIRedirectTransport struct {
	apiBase string
}

func (tr *xAPIRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), xAPIBase) {
		target, _ := url.Parse(tr.apiBase + req.URL.RequestURI())
		newReq := req.Clone(req.Context())
		newReq.URL = target
		newReq.Host = target.Host
		return http.DefaultTransport.RoundTrip(newReq)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// — publish debug logging ——————————————————————————————————————————————————

// TestPublish_logsWarnOnNonSuccess verifies that a non-2xx response from X
// triggers a WARN-level slog record containing the HTTP status.
func TestPublish_logsWarnOnNonSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-abc-123")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()

	cap := &slogCapture{}
	orig := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(orig) })

	tc := &twitterClient{
		httpClient: &http.Client{
			Transport: &xAPIRedirectTransport{apiBase: srv.URL},
		},
	}
	_, err := tc.publish(context.Background(), ScheduledPost{Body: "Hello"}, PlatformCredential{accessToken: "tok"})
	if err == nil {
		t.Fatal("expected error for 403 response, got nil")
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	for _, r := range cap.records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, "non-2xx") {
			return // found — test passes
		}
	}
	t.Errorf("expected WARN log containing 'non-2xx', got %d records", len(cap.records))
}

// slogCapture is a minimal slog.Handler that collects all records.
type slogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *slogCapture) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (c *slogCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.records = append(c.records, r.Clone())
	c.mu.Unlock()
	return nil
}
func (c *slogCapture) WithAttrs(_ []slog.Attr) slog.Handler  { return c }
func (c *slogCapture) WithGroup(_ string) slog.Handler       { return c }

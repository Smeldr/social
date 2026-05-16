package forgesocial

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

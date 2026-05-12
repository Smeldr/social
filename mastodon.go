package forgesocial

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MastodonConfig holds the OAuth 2.0 client credentials for a Mastodon instance.
// Register your application once on the Mastodon instance admin panel and
// supply the resulting client_id and client_secret here.
type MastodonConfig struct {
	// ClientID is the OAuth 2.0 client_id issued by the Mastodon instance.
	ClientID string
	// ClientSecret is the OAuth 2.0 client_secret issued by the Mastodon instance.
	ClientSecret string
	// InstanceURL is the base URL of the Mastodon instance, e.g. "https://mastodon.social".
	InstanceURL string
	// RedirectURL is the OAuth callback URL registered with the Mastodon instance.
	// Set this to your app's BaseURL + "/oauth/mastodon/callback".
	RedirectURL string
	// SuccessURL is an optional URL to redirect the browser to after a
	// successful OAuth callback. If empty, a plain HTML confirmation is shown.
	SuccessURL string
}

// mastodonTokenResponse is the JSON payload returned by Mastodon's /oauth/token endpoint.
type mastodonTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	CreatedAt    int64  `json:"created_at"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

// mastodonStatusResponse is the partial JSON payload returned by POST /api/v1/statuses.
type mastodonStatusResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// mastodonMediaResponse is the partial JSON payload returned by POST /api/v2/media.
type mastodonMediaResponse struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// mastodonClient performs OAuth token exchange and API calls against a
// Mastodon instance. All operations require a valid access token stored in
// a PlatformCredential — mastodonClient never persists tokens itself.
type mastodonClient struct {
	cfg        MastodonConfig
	httpClient *http.Client
}

func newMastodonClient(cfg MastodonConfig) *mastodonClient {
	return &mastodonClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// authURL builds the Mastodon OAuth 2.0 authorization URL for the given state token.
// The user must visit this URL in a browser to authorise the application.
func (c *mastodonClient) authURL(state string) string {
	params := url.Values{
		"client_id":     {c.cfg.ClientID},
		"redirect_uri":  {c.cfg.RedirectURL},
		"response_type": {"code"},
		"scope":         {"write:statuses write:media"},
		"state":         {state},
	}
	return strings.TrimRight(c.cfg.InstanceURL, "/") + "/oauth/authorize?" + strings.ReplaceAll(params.Encode(), "+", "%20")
}

// exchangeCode exchanges an authorization code for an access token.
// Returns the token response on success.
func (c *mastodonClient) exchangeCode(ctx context.Context, code string) (mastodonTokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"redirect_uri":  {c.cfg.RedirectURL},
		"scope":         {"write:statuses write:media"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.InstanceURL, "/")+"/oauth/token",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return mastodonTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return mastodonTokenResponse{}, fmt.Errorf("forgesocial: token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return mastodonTokenResponse{}, fmt.Errorf("forgesocial: token exchange read: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return mastodonTokenResponse{}, fmt.Errorf("forgesocial: token exchange: HTTP %d: %s",
			resp.StatusCode, truncate(string(body), 256))
	}

	var tr mastodonTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return mastodonTokenResponse{}, fmt.Errorf("forgesocial: token exchange parse: %w", err)
	}
	return tr, nil
}

// uploadMedia uploads a single image to the Mastodon v2 media API and returns
// the media attachment ID. mediaURL must be an accessible HTTP(S) URL; the
// image is fetched by forge-social and forwarded as a multipart upload.
func (c *mastodonClient) uploadMedia(ctx context.Context, accessToken, mediaURL, altText string) (string, error) {
	// Fetch the image bytes.
	imgReq, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return "", fmt.Errorf("forgesocial: fetch media: %w", err)
	}
	imgResp, err := c.httpClient.Do(imgReq)
	if err != nil {
		return "", fmt.Errorf("forgesocial: fetch media: %w", err)
	}
	defer imgResp.Body.Close()
	if imgResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("forgesocial: fetch media: HTTP %d", imgResp.StatusCode)
	}
	imgBytes, err := io.ReadAll(io.LimitReader(imgResp.Body, 10<<20)) // 10 MB cap
	if err != nil {
		return "", fmt.Errorf("forgesocial: fetch media read: %w", err)
	}

	// Build multipart body.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "upload")
	if err != nil {
		return "", fmt.Errorf("forgesocial: media multipart: %w", err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(imgBytes)); err != nil {
		return "", err
	}
	if altText != "" {
		_ = mw.WriteField("description", altText)
	}
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/v2/media",
		&buf,
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("forgesocial: upload media: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return "", &publishError{
			statusCode: resp.StatusCode,
			msg:        fmt.Sprintf("upload media: HTTP %d: %s", resp.StatusCode, truncate(string(body), 256)),
			terminal:   isTerminalStatus(resp.StatusCode),
		}
	}

	var mr mastodonMediaResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return "", fmt.Errorf("forgesocial: upload media parse: %w", err)
	}
	return mr.ID, nil
}

// postStatus publishes a status update to Mastodon.
// If mediaID is non-empty, the media attachment is included.
// Returns the platform post ID (Mastodon status ID) on success.
func (c *mastodonClient) postStatus(ctx context.Context, accessToken, body, mediaID string) (string, error) {
	payload := map[string]any{
		"status": body,
	}
	if mediaID != "" {
		payload["media_ids"] = []string{mediaID}
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.InstanceURL, "/")+"/api/v1/statuses",
		bytes.NewReader(payloadBytes),
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("forgesocial: post status: %w", err)
	}
	defer resp.Body.Close()

	// Handle 429 separately to extract Retry-After.
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return "", &rateLimitError{retryAfter: retryAfter}
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", &publishError{
			statusCode: resp.StatusCode,
			msg:        fmt.Sprintf("post status: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 256)),
			terminal:   isTerminalStatus(resp.StatusCode),
		}
	}

	var sr mastodonStatusResponse
	if err := json.Unmarshal(respBody, &sr); err != nil {
		return "", fmt.Errorf("forgesocial: post status parse: %w", err)
	}
	return sr.ID, nil
}

// publish publishes post p using the credentials in cred.
// It uploads media (if post.MediaURL is set) and then posts the status.
// Returns the platform post ID on success.
func (c *mastodonClient) publish(ctx context.Context, p ScheduledPost, cred PlatformCredential) (string, error) {
	var mediaID string
	if p.MediaURL != "" {
		var err error
		mediaID, err = c.uploadMedia(ctx, cred.accessToken, p.MediaURL, p.AltText)
		if err != nil {
			return "", err
		}
	}
	return c.postStatus(ctx, cred.accessToken, p.Body, mediaID)
}

// ─── error types ─────────────────────────────────────────────────────────────

// publishError is returned when the Mastodon API responds with a non-success
// status code (other than 429).
type publishError struct {
	statusCode int
	msg        string
	terminal   bool
}

func (e *publishError) Error() string { return e.msg }

// IsTerminal reports whether the error is unrecoverable (e.g. 401, 403, 422).
// Terminal errors should set the post status to "failed" without retrying.
func (e *publishError) IsTerminal() bool { return e.terminal }

// rateLimitError is returned when the Mastodon API responds with 429.
type rateLimitError struct {
	retryAfter time.Duration
}

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("forgesocial: rate limited; retry after %s", e.retryAfter)
}

// isTerminalStatus returns true for HTTP status codes that indicate a
// non-retryable client error.
func isTerminalStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, // 401
		http.StatusForbidden,           // 403
		http.StatusUnprocessableEntity: // 422
		return true
	}
	return false
}

// parseRetryAfter parses the Retry-After header value.
// Supports both delta-seconds and HTTP-date formats.
// Returns 5 minutes as the fallback.
func parseRetryAfter(v string) time.Duration {
	const fallback = 5 * time.Minute
	if v == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return fallback
}

// truncate shortens s to at most n runes for safe inclusion in error messages.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

package forgesocial

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"
)

const (
	xAPIBase        = "https://api.twitter.com"
	xMediaUploadURL = "https://api.x.com/2/media/upload"
	xAuthBase       = "https://x.com"
	xMaxBodyLength  = 280

	// xTokenExpiryBuffer is the window before expiry within which an X access
	// token is proactively refreshed. X tokens expire after 2 hours; refreshing
	// 5 minutes early prevents failures on long-running publish retries.
	xTokenExpiryBuffer = 5 * time.Minute
)

// xConfig holds the OAuth 2.0 app credentials for X (Twitter).
// Used internally — operators configure X via the configure_platform MCP tool.
type xConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	SuccessURL   string
}

// twitterClient performs OAuth 2.0 PKCE token exchange and API calls against X.
// All operations require a valid access token stored in a PlatformCredential —
// twitterClient never persists tokens itself.
type twitterClient struct {
	cfg        xConfig
	httpClient *http.Client
}

func newTwitterClient(cfg xConfig) *twitterClient {
	return &twitterClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// generatePKCE returns a PKCE verifier and its SHA-256 challenge.
// The verifier is 32 random bytes base64url-encoded (43 chars) per RFC 7636.
func generatePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, raw); err != nil {
		return "", "", fmt.Errorf("forgesocial: generate PKCE: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// authURL builds the X OAuth 2.0 authorization URL for the given state and
// code challenge. The user must visit this URL in a browser to authorise.
func (c *twitterClient) authURL(state, codeChallenge string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", c.cfg.ClientID)
	v.Set("redirect_uri", c.cfg.RedirectURL)
	v.Set("scope", "tweet.read users.read tweet.write offline.access")
	v.Set("state", state)
	v.Set("code_challenge", codeChallenge)
	v.Set("code_challenge_method", "S256")
	return xAuthBase + "/i/oauth2/authorize?" + v.Encode()
}

// xTokenResponse is the JSON payload returned by the X /2/oauth2/token endpoint.
type xTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
}

// xTweetResponse is the partial JSON payload returned by POST /2/tweets.
type xTweetResponse struct {
	Data struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	} `json:"data"`
}

// xTweetPayload is the JSON body for POST /2/tweets.
type xTweetPayload struct {
	Text  string       `json:"text"`
	Media *xTweetMedia `json:"media,omitempty"`
}

// xTweetMedia carries the media_ids attachment for a tweet.
type xTweetMedia struct {
	MediaIDs []string `json:"media_ids"`
}

// xMediaUploadResponse is the partial JSON payload returned by POST /2/media/upload.
type xMediaUploadResponse struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

// uploadXMedia fetches the image at mediaURL and uploads it to the X v1.1 media
// upload endpoint. Returns the media ID string to attach to a tweet.
func uploadXMedia(ctx context.Context, client *http.Client, accessToken, mediaURL string) (string, error) {
	// Fetch the image bytes.
	imgReq, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return "", fmt.Errorf("forgesocial: X fetch media: %w", err)
	}
	imgResp, err := client.Do(imgReq)
	if err != nil {
		return "", fmt.Errorf("forgesocial: X fetch media: %w", err)
	}
	defer imgResp.Body.Close()
	if imgResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("forgesocial: X fetch media: HTTP %d", imgResp.StatusCode)
	}
	contentType := imgResp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	imgBytes, err := io.ReadAll(io.LimitReader(imgResp.Body, 10<<20)) // 10 MB cap
	if err != nil {
		return "", fmt.Errorf("forgesocial: X fetch media read: %w", err)
	}

	// Build multipart body.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("media_category", "tweet_image"); err != nil {
		return "", fmt.Errorf("forgesocial: X media multipart: %w", err)
	}
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="media"; filename="upload"`}
	h["Content-Type"] = []string{contentType}
	fw, err := mw.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("forgesocial: X media multipart: %w", err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(imgBytes)); err != nil {
		return "", fmt.Errorf("forgesocial: X media multipart write: %w", err)
	}
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, xMediaUploadURL, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("forgesocial: X upload media: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", &publishError{
			statusCode: resp.StatusCode,
			msg:        fmt.Sprintf("X upload media: HTTP %d: %s", resp.StatusCode, truncate(string(body), 256)),
			terminal:   isTerminalStatus(resp.StatusCode),
		}
	}

	var mr xMediaUploadResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return "", fmt.Errorf("forgesocial: X upload media parse: %w", err)
	}
	return mr.Data.ID, nil
}

// exchangeCode exchanges an authorization code for an access token using PKCE.
// codeVerifier must match the code_challenge used to initiate the flow.
func (c *twitterClient) exchangeCode(ctx context.Context, code, codeVerifier string) (xTokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.cfg.RedirectURL},
		"code_verifier": {codeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		xAPIBase+"/2/oauth2/token",
		bytes.NewBufferString(form.Encode()),
	)
	if err != nil {
		return xTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// X requires Basic auth with client_id:client_secret for confidential clients.
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return xTokenResponse{}, fmt.Errorf("forgesocial: X token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return xTokenResponse{}, fmt.Errorf("forgesocial: X token exchange read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return xTokenResponse{}, fmt.Errorf("forgesocial: X token exchange: HTTP %d: %s",
			resp.StatusCode, truncate(string(body), 256))
	}

	var tr xTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return xTokenResponse{}, fmt.Errorf("forgesocial: X token exchange parse: %w", err)
	}
	return tr, nil
}

// refreshXToken exchanges a refresh token for a new access+refresh token pair.
// If the response omits the refresh token, the caller must retain the existing one.
func (c *twitterClient) refreshXToken(ctx context.Context, refreshTok string) (xTokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		xAPIBase+"/2/oauth2/token",
		bytes.NewBufferString(form.Encode()),
	)
	if err != nil {
		return xTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return xTokenResponse{}, fmt.Errorf("forgesocial: X token refresh: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return xTokenResponse{}, fmt.Errorf("forgesocial: X token refresh read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return xTokenResponse{}, fmt.Errorf("forgesocial: X token refresh: HTTP %d: %s",
			resp.StatusCode, truncate(string(body), 256))
	}

	var tr xTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return xTokenResponse{}, fmt.Errorf("forgesocial: X token refresh parse: %w", err)
	}
	return tr, nil
}

// publish posts a tweet to X. The post body must not exceed xMaxBodyLength (280)
// characters. If p.MediaURL is set, the image is uploaded first via uploadXMedia
// and attached to the tweet.
// Returns the platform tweet ID on success.
func (c *twitterClient) publish(ctx context.Context, p ScheduledPost, cred PlatformCredential) (string, error) {
	if len([]rune(p.Body)) > xMaxBodyLength {
		return "", &publishError{
			msg:      fmt.Sprintf("post body exceeds X character limit (%d/%d)", len([]rune(p.Body)), xMaxBodyLength),
			terminal: true,
		}
	}

	tweetPayload := xTweetPayload{Text: p.Body}
	if p.MediaURL != "" {
		mediaID, err := uploadXMedia(ctx, c.httpClient, cred.accessToken, p.MediaURL)
		if err != nil {
			return "", fmt.Errorf("forgesocial: X media upload: %w", err)
		}
		tweetPayload.Media = &xTweetMedia{MediaIDs: []string{mediaID}}
	}

	payload, err := json.Marshal(tweetPayload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		xAPIBase+"/2/tweets",
		bytes.NewReader(payload),
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cred.accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("forgesocial: X publish: %w", err)
	}
	defer resp.Body.Close()

	// Handle 429 rate limit.
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return "", &rateLimitError{retryAfter: retryAfter}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("forgesocial: X publish read: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", &publishError{
			statusCode: resp.StatusCode,
			msg:        fmt.Sprintf("X publish: HTTP %d: %s", resp.StatusCode, truncate(string(body), 256)),
			terminal:   isTerminalStatus(resp.StatusCode),
		}
	}

	var tr xTweetResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("forgesocial: X publish parse: %w", err)
	}
	return tr.Data.ID, nil
}

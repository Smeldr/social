package social

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

const (
	xAPIBase        = "https://api.twitter.com"
	xMediaUploadURL = "https://api.x.com/2/media/upload"
	xAuthBase       = "https://x.com"
	xMaxBodyLength  = 280
	// xTcoURLLen is the character count X assigns to any URL after t.co wrapping,
	// regardless of the URL's actual length.
	// See: https://developer.twitter.com/en/docs/counting-characters
	xTcoURLLen = 23

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
		return "", "", fmt.Errorf("social: generate PKCE: %w", err)
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

// uploadXMedia fetches the image at mediaURL and uploads it to the X v2 media
// upload endpoint using the mandatory three-step chunked protocol:
// INIT → APPEND → FINALIZE. Returns the media ID string to attach to a tweet.
func uploadXMedia(ctx context.Context, client *http.Client, accessToken, mediaURL string) (string, error) {
	// Fetch the image bytes.
	imgReq, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return "", fmt.Errorf("social: X fetch media: %w", err)
	}
	imgResp, err := client.Do(imgReq)
	if err != nil {
		return "", fmt.Errorf("social: X fetch media: %w", err)
	}
	defer imgResp.Body.Close()
	if imgResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("social: X fetch media: HTTP %d", imgResp.StatusCode)
	}
	contentType := imgResp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	imgBytes, err := io.ReadAll(io.LimitReader(imgResp.Body, 10<<20)) // 10 MB cap
	if err != nil {
		return "", fmt.Errorf("social: X fetch media read: %w", err)
	}

	// Step 1 — INIT: declare media type, size, and category; receive media_id.
	var initBuf bytes.Buffer
	initMW := multipart.NewWriter(&initBuf)
	for _, kv := range [][2]string{
		{"command", "INIT"},
		{"media_type", contentType},
		{"total_bytes", strconv.Itoa(len(imgBytes))},
		{"media_category", "tweet_image"},
	} {
		if err := initMW.WriteField(kv[0], kv[1]); err != nil {
			return "", fmt.Errorf("social: X media INIT multipart: %w", err)
		}
	}
	initMW.Close()

	initReq, err := http.NewRequestWithContext(ctx, http.MethodPost, xMediaUploadURL, &initBuf)
	if err != nil {
		return "", fmt.Errorf("social: X media INIT request: %w", err)
	}
	initReq.Header.Set("Authorization", "Bearer "+accessToken)
	initReq.Header.Set("Content-Type", initMW.FormDataContentType())

	slog.Debug("social: X upload media INIT", "url", xMediaUploadURL)
	initResp, err := client.Do(initReq)
	if err != nil {
		return "", fmt.Errorf("social: X upload media INIT: %w", err)
	}
	defer initResp.Body.Close()
	initBody, _ := io.ReadAll(io.LimitReader(initResp.Body, 64*1024))
	if initResp.StatusCode != http.StatusOK && initResp.StatusCode != http.StatusCreated {
		slog.Warn("social: X upload media INIT non-2xx response",
			"status", initResp.StatusCode,
			"x_request_id", initResp.Header.Get("X-Request-Id"),
			"body", truncate(string(initBody), 256),
		)
		return "", &publishError{
			statusCode: initResp.StatusCode,
			msg:        fmt.Sprintf("X upload media INIT: HTTP %d: %s", initResp.StatusCode, truncate(string(initBody), 256)),
			terminal:   isTerminalStatus(initResp.StatusCode),
		}
	}
	var initMR xMediaUploadResponse
	if err := json.Unmarshal(initBody, &initMR); err != nil {
		return "", fmt.Errorf("social: X upload media INIT parse: %w", err)
	}
	mediaID := initMR.Data.ID

	// Step 2 — APPEND: upload the binary image bytes as segment 0.
	var appendBuf bytes.Buffer
	appendMW := multipart.NewWriter(&appendBuf)
	for _, kv := range [][2]string{
		{"command", "APPEND"},
		{"media_id", mediaID},
		{"segment_index", "0"},
	} {
		if err := appendMW.WriteField(kv[0], kv[1]); err != nil {
			return "", fmt.Errorf("social: X media APPEND multipart: %w", err)
		}
	}
	mediaPart := map[string][]string{
		"Content-Disposition": {`form-data; name="media"; filename="upload"`},
		"Content-Type":        {contentType},
	}
	fw, err := appendMW.CreatePart(mediaPart)
	if err != nil {
		return "", fmt.Errorf("social: X media APPEND part: %w", err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(imgBytes)); err != nil {
		return "", fmt.Errorf("social: X media APPEND write: %w", err)
	}
	appendMW.Close()

	appendReq, err := http.NewRequestWithContext(ctx, http.MethodPost, xMediaUploadURL, &appendBuf)
	if err != nil {
		return "", fmt.Errorf("social: X media APPEND request: %w", err)
	}
	appendReq.Header.Set("Authorization", "Bearer "+accessToken)
	appendReq.Header.Set("Content-Type", appendMW.FormDataContentType())

	slog.Debug("social: X upload media APPEND", "url", xMediaUploadURL)
	appendResp, err := client.Do(appendReq)
	if err != nil {
		return "", fmt.Errorf("social: X upload media APPEND: %w", err)
	}
	appendBody, _ := io.ReadAll(io.LimitReader(appendResp.Body, 64*1024))
	appendResp.Body.Close()
	if appendResp.StatusCode < 200 || appendResp.StatusCode >= 300 {
		slog.Warn("social: X upload media APPEND non-2xx response",
			"status", appendResp.StatusCode,
			"x_request_id", appendResp.Header.Get("X-Request-Id"),
			"body", truncate(string(appendBody), 256),
		)
		return "", &publishError{
			statusCode: appendResp.StatusCode,
			msg:        fmt.Sprintf("X upload media APPEND: HTTP %d: %s", appendResp.StatusCode, truncate(string(appendBody), 256)),
			terminal:   isTerminalStatus(appendResp.StatusCode),
		}
	}

	// Step 3 — FINALIZE: confirm the upload and retrieve the final media_id.
	var finBuf bytes.Buffer
	finMW := multipart.NewWriter(&finBuf)
	for _, kv := range [][2]string{
		{"command", "FINALIZE"},
		{"media_id", mediaID},
	} {
		if err := finMW.WriteField(kv[0], kv[1]); err != nil {
			return "", fmt.Errorf("social: X media FINALIZE multipart: %w", err)
		}
	}
	finMW.Close()

	finReq, err := http.NewRequestWithContext(ctx, http.MethodPost, xMediaUploadURL, &finBuf)
	if err != nil {
		return "", fmt.Errorf("social: X media FINALIZE request: %w", err)
	}
	finReq.Header.Set("Authorization", "Bearer "+accessToken)
	finReq.Header.Set("Content-Type", finMW.FormDataContentType())

	slog.Debug("social: X upload media FINALIZE", "url", xMediaUploadURL)
	finResp, err := client.Do(finReq)
	if err != nil {
		return "", fmt.Errorf("social: X upload media FINALIZE: %w", err)
	}
	defer finResp.Body.Close()
	finBody, _ := io.ReadAll(io.LimitReader(finResp.Body, 64*1024))
	if finResp.StatusCode != http.StatusOK && finResp.StatusCode != http.StatusCreated {
		slog.Warn("social: X upload media FINALIZE non-2xx response",
			"status", finResp.StatusCode,
			"x_request_id", finResp.Header.Get("X-Request-Id"),
			"body", truncate(string(finBody), 256),
		)
		return "", &publishError{
			statusCode: finResp.StatusCode,
			msg:        fmt.Sprintf("X upload media FINALIZE: HTTP %d: %s", finResp.StatusCode, truncate(string(finBody), 256)),
			terminal:   isTerminalStatus(finResp.StatusCode),
		}
	}
	var finMR xMediaUploadResponse
	if err := json.Unmarshal(finBody, &finMR); err != nil {
		return "", fmt.Errorf("social: X upload media FINALIZE parse: %w", err)
	}
	return finMR.Data.ID, nil
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
		return xTokenResponse{}, fmt.Errorf("social: X token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return xTokenResponse{}, fmt.Errorf("social: X token exchange read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return xTokenResponse{}, fmt.Errorf("social: X token exchange: HTTP %d: %s",
			resp.StatusCode, truncate(string(body), 256))
	}

	var tr xTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return xTokenResponse{}, fmt.Errorf("social: X token exchange parse: %w", err)
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
		return xTokenResponse{}, fmt.Errorf("social: X token refresh: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return xTokenResponse{}, fmt.Errorf("social: X token refresh read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return xTokenResponse{}, fmt.Errorf("social: X token refresh: HTTP %d: %s",
			resp.StatusCode, truncate(string(body), 256))
	}

	var tr xTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return xTokenResponse{}, fmt.Errorf("social: X token refresh parse: %w", err)
	}
	return tr, nil
}

// xURLRegexp matches http and https URLs in a post body for t.co weighting.
var xURLRegexp = regexp.MustCompile(`https?://\S+`)

// xWeightedBodyLen returns the X-weighted character count of body.
// X wraps all URLs with t.co and counts each as exactly xTcoURLLen characters,
// regardless of the URL's actual length.
func xWeightedBodyLen(body string) int {
	total := len([]rune(body))
	for _, m := range xURLRegexp.FindAllString(body, -1) {
		total += xTcoURLLen - len([]rune(m))
	}
	return total
}

// publish posts a tweet to X. The post body must not exceed xMaxBodyLength (280)
// weighted characters (URLs count as xTcoURLLen each). If p.MediaURL is set, the
// image is uploaded first via uploadXMedia and attached to the tweet.
// Returns the platform tweet ID on success.
func (c *twitterClient) publish(ctx context.Context, p ScheduledPost, cred PlatformCredential) (string, error) {
	if xWeightedBodyLen(p.Body) > xMaxBodyLength {
		return "", &publishError{
			msg:      fmt.Sprintf("post body exceeds X character limit (%d/%d)", xWeightedBodyLen(p.Body), xMaxBodyLength),
			terminal: true,
		}
	}

	tweetPayload := xTweetPayload{Text: p.Body}
	if p.MediaURL != "" {
		mediaID, err := uploadXMedia(ctx, c.httpClient, cred.accessToken, p.MediaURL)
		if err != nil {
			return "", fmt.Errorf("social: X media upload: %w", err)
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

	slog.Debug("social: X publish request", "method", req.Method, "url", xAPIBase+"/2/tweets")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("social: X publish: %w", err)
	}
	defer resp.Body.Close()

	// Handle 429 rate limit.
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return "", &rateLimitError{retryAfter: retryAfter}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("social: X publish read: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		slog.Warn("social: X publish non-2xx response",
			"status", resp.StatusCode,
			"x_request_id", resp.Header.Get("X-Request-Id"),
			"body", truncate(string(body), 256),
		)
		return "", &publishError{
			statusCode: resp.StatusCode,
			msg:        fmt.Sprintf("X publish: HTTP %d: %s", resp.StatusCode, truncate(string(body), 256)),
			terminal:   isTerminalStatus(resp.StatusCode),
		}
	}

	var tr xTweetResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("social: X publish parse: %w", err)
	}
	return tr.Data.ID, nil
}

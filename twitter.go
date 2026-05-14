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
	"net/http"
	"net/url"
	"time"
)

const (
	xAPIBase       = "https://api.twitter.com"
	xAuthBase      = "https://twitter.com"
	xMaxBodyLength = 280
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

// publish posts a tweet to X. The post body must not exceed xMaxBodyLength (280)
// characters. Media attachments are not supported in this version.
// Returns the platform tweet ID on success.
func (c *twitterClient) publish(ctx context.Context, p ScheduledPost, cred PlatformCredential) (string, error) {
	if len([]rune(p.Body)) > xMaxBodyLength {
		return "", &publishError{
			msg:      fmt.Sprintf("post body exceeds X character limit (%d/%d)", len([]rune(p.Body)), xMaxBodyLength),
			terminal: true,
		}
	}

	payload, err := json.Marshal(map[string]string{"text": p.Body})
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

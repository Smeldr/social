package social

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LinkedInConfig holds OAuth 2.0 client credentials for the LinkedIn platform.
type LinkedInConfig struct {
	// ClientID is the LinkedIn OAuth application client ID.
	ClientID string
	// ClientSecret is the LinkedIn OAuth application client secret.
	ClientSecret string
	// RedirectURL is the callback URL registered with the LinkedIn application,
	// e.g. cfg.BaseURL + "/oauth/linkedin/callback".
	RedirectURL string
	// SuccessURL is an optional URL to redirect the user to after a successful
	// OAuth connection. If empty, a plain confirmation response is returned.
	SuccessURL string
}

// linkedinClient calls the LinkedIn REST APIs.
type linkedinClient struct {
	cfg LinkedInConfig
	hc  *http.Client
}

func newLinkedinClient(cfg LinkedInConfig) *linkedinClient {
	return &linkedinClient{
		cfg: cfg,
		hc:  &http.Client{Timeout: 30 * time.Second},
	}
}

// authURL builds the LinkedIn OAuth 2.0 authorization URL.
// Scopes: openid profile w_member_social
func (lc *linkedinClient) authURL(state string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", lc.cfg.ClientID)
	v.Set("redirect_uri", lc.cfg.RedirectURL)
	v.Set("state", state)
	v.Set("scope", "openid profile w_member_social")
	return "https://www.linkedin.com/oauth/v2/authorization?" + v.Encode()
}

// linkedinTokenResponse is the JSON body returned by the token endpoint.
type linkedinTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"` // seconds
}

// exchangeCode exchanges the authorization code for an access token.
func (lc *linkedinClient) exchangeCode(ctx context.Context, code string) (*linkedinTokenResponse, error) {
	body := url.Values{}
	body.Set("grant_type", "authorization_code")
	body.Set("code", code)
	body.Set("redirect_uri", lc.cfg.RedirectURL)
	body.Set("client_id", lc.cfg.ClientID)
	body.Set("client_secret", lc.cfg.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://www.linkedin.com/oauth/v2/accessToken",
		strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("social: linkedin token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := lc.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("social: linkedin token exchange: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("social: linkedin token exchange: HTTP %d: %s", resp.StatusCode, b)
	}

	var tr linkedinTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("social: linkedin token decode: %w", err)
	}
	return &tr, nil
}

// fetchPersonURN calls the OpenID Connect userinfo endpoint and returns the
// LinkedIn person URN ("urn:li:person:{sub}").
func (lc *linkedinClient) fetchPersonURN(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.linkedin.com/v2/userinfo", nil)
	if err != nil {
		return "", fmt.Errorf("social: linkedin userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := lc.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("social: linkedin userinfo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("social: linkedin userinfo: HTTP %d: %s", resp.StatusCode, b)
	}

	var info struct {
		Sub string `json:"sub"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("social: linkedin userinfo decode: %w", err)
	}
	if info.Sub == "" {
		return "", fmt.Errorf("social: linkedin userinfo: sub field is empty")
	}
	return "urn:li:person:" + info.Sub, nil
}

// linkedinUGCPost is the JSON body for the UGC Posts API.
type linkedinUGCPost struct {
	Author          string         `json:"author"`
	LifecycleState  string         `json:"lifecycleState"`
	SpecificContent map[string]any `json:"specificContent"`
	Visibility      map[string]any `json:"visibility"`
}

// publish posts the ScheduledPost to LinkedIn via the UGC Posts API.
// Returns the LinkedIn post URN as platformID.
//
// Error classification:
//   - 401, 403, 422 → terminal publishError
//   - 429            → rateLimitError (honours Retry-After header)
//   - 5xx            → transient publishError (retried with backoff)
func (lc *linkedinClient) publish(ctx context.Context, p ScheduledPost, cred PlatformCredential) (string, error) {
	if cred.ActorID == "" {
		return "", &publishError{msg: "LinkedIn credential missing actor_id (person URN); reconnect the account", terminal: true}
	}

	payload := linkedinUGCPost{
		Author:         cred.ActorID,
		LifecycleState: "PUBLISHED",
		SpecificContent: map[string]any{
			"com.linkedin.ugc.ShareContent": map[string]any{
				"shareCommentary":    map[string]any{"text": truncate(p.Body, 3000)},
				"shareMediaCategory": "NONE",
			},
		},
		Visibility: map[string]any{
			"com.linkedin.ugc.MemberNetworkVisibility": "PUBLIC",
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("social: linkedin marshal post: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.linkedin.com/v2/ugcPosts", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("social: linkedin post request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cred.accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Restli-Protocol-Version", "2.0.0")

	resp, err := lc.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("social: linkedin post: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		// Success — extract the post URN from the response.
		var result struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &result); err != nil || result.ID == "" {
			// Return an empty platformID rather than failing — the post was created.
			return "", nil
		}
		return result.ID, nil

	case http.StatusTooManyRequests:
		return "", &rateLimitError{
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}

	default:
		terminal := isTerminalStatus(resp.StatusCode)
		msg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		return "", &publishError{
			statusCode: resp.StatusCode,
			msg:        msg,
			terminal:   terminal,
		}
	}
}

// linkedinDisplayName builds a human-readable name for the credential from
// the person URN. The URN is formatted as "urn:li:person:{sub}" — we use
// the numeric sub as the display name since the full name is not returned
// by the token endpoint (only by the userinfo endpoint which we already call).
// Callers that want a friendlier name can override it later.
func linkedinDisplayName(personURN string) string {
	// Strip "urn:li:person:" prefix.
	if after, ok := strings.CutPrefix(personURN, "urn:li:person:"); ok {
		return "linkedin:" + after
	}
	return "linkedin"
}

// linkedinExpiresAt computes the expiry time from an expiresIn value (seconds).
// Returns nil when expiresIn is 0.
func linkedinExpiresAt(expiresIn int) *time.Time {
	if expiresIn <= 0 {
		return nil
	}
	t := time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
	return &t
}

// linkedinStateKey is a helper used in tests to build a unique instance_url
// for LinkedIn credentials. LinkedIn does not have per-instance URLs; we use
// a synthetic constant so that upsertCredentialByInstance can match on it.
const linkedinInstanceURL = "https://www.linkedin.com"

package social

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"smeldr.dev/core"
)

// oauthStateTTL is the maximum age of an OAuth state token.
const oauthStateTTL = 10 * time.Minute

// insertOAuthState stores a new OAuth state token for the given platform.
// codeVerifier holds the PKCE verifier for platforms that require it (X);
// pass an empty string for Mastodon and LinkedIn.
func insertOAuthState(db smeldr.DB, state, platform, codeVerifier string) error {
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO forge_social_oauth_states (state, platform, code_verifier, created_at)
		VALUES (?, ?, ?, ?)`,
		state, platform, codeVerifier, time.Now().UTC(),
	)
	return err
}

// consumeOAuthState validates and deletes the OAuth state token.
// Returns the platform and code_verifier associated with the state,
// or an error if the state is unknown or older than oauthStateTTL.
// code_verifier is empty for platforms that do not use PKCE (Mastodon, LinkedIn).
func consumeOAuthState(db smeldr.DB, state string) (platform, codeVerifier string, err error) {
	var createdAt time.Time
	err = db.QueryRowContext(context.Background(), `
		SELECT platform, code_verifier, created_at FROM forge_social_oauth_states WHERE state=?`, state,
	).Scan(&platform, &codeVerifier, &createdAt)

	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("social: unknown OAuth state")
	}
	if err != nil {
		return "", "", err
	}

	// Delete regardless of TTL — state is single-use.
	_, _ = db.ExecContext(context.Background(),
		`DELETE FROM forge_social_oauth_states WHERE state=?`, state)

	if time.Since(createdAt) > oauthStateTTL {
		return "", "", fmt.Errorf("social: OAuth state expired")
	}

	return platform, codeVerifier, nil
}

// purgeExpiredOAuthStates removes state rows older than oauthStateTTL.
// Called periodically to prevent table bloat.
func purgeExpiredOAuthStates(db smeldr.DB) error {
	cutoff := time.Now().UTC().Add(-oauthStateTTL)
	_, err := db.ExecContext(context.Background(),
		`DELETE FROM forge_social_oauth_states WHERE created_at < ?`, cutoff)
	return err
}

// handleMastodonCallback processes the OAuth 2.0 callback from Mastodon.
// It validates the state, exchanges the authorization code for an access
// token, and persists the resulting PlatformCredential.
func (s *Social) handleMastodonCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	oauthError := r.URL.Query().Get("error")

	if oauthError != "" {
		http.Error(w, "OAuth error: "+oauthError, http.StatusBadRequest)
		return
	}
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	platform, codeVerifier, err := consumeOAuthState(s.creds.db, state)
	if err != nil {
		http.Error(w, "invalid or expired OAuth state", http.StatusBadRequest)
		return
	}
	if platform != "mastodon" {
		http.Error(w, "unexpected platform: "+platform, http.StatusBadRequest)
		return
	}

	tr, err := s.mastodon.exchangeCode(r.Context(), code, codeVerifier)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	var expiresAt *time.Time
	if tr.ExpiresIn > 0 {
		t := time.Now().UTC().Add(time.Duration(tr.ExpiresIn) * time.Second)
		expiresAt = &t
	}

	instanceURL := strings.TrimRight(s.mastodon.cfg.InstanceURL, "/")
	name := instanceURL // human-readable name: the instance URL
	// Strip the scheme for a tidier display name.
	if after, ok := strings.CutPrefix(name, "https://"); ok {
		name = after
	} else if after, ok := strings.CutPrefix(name, "http://"); ok {
		name = after
	}

	_, err = s.creds.upsertCredentialByInstance(
		"mastodon", instanceURL, name,
		tr.AccessToken, tr.RefreshToken, "", expiresAt,
	)
	if err != nil {
		http.Error(w, "failed to save credential", http.StatusInternalServerError)
		return
	}

	// Success response.
	if s.mastodon.cfg.SuccessURL != "" {
		http.Redirect(w, r, s.mastodon.cfg.SuccessURL, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Connected</title></head>
<body><p>Connected to Mastodon successfully. You can close this tab.</p></body>
</html>`)
}

// handleLinkedInCallback processes the OAuth 2.0 callback from LinkedIn.
// It validates the state, exchanges the code for an access token, fetches
// the person URN, and persists the resulting PlatformCredential.
func (s *Social) handleLinkedInCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	oauthError := r.URL.Query().Get("error")

	if oauthError != "" {
		http.Error(w, "OAuth error: "+oauthError, http.StatusBadRequest)
		return
	}
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	platform, _, err := consumeOAuthState(s.creds.db, state)
	if err != nil {
		http.Error(w, "invalid or expired OAuth state", http.StatusBadRequest)
		return
	}
	if platform != "linkedin" {
		http.Error(w, "unexpected platform: "+platform, http.StatusBadRequest)
		return
	}

	tr, err := s.linkedin.exchangeCode(r.Context(), code)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	actorID, err := s.linkedin.fetchPersonURN(r.Context(), tr.AccessToken)
	if err != nil {
		http.Error(w, "failed to fetch LinkedIn person URN: "+err.Error(), http.StatusBadGateway)
		return
	}

	name := linkedinDisplayName(actorID)
	expiresAt := linkedinExpiresAt(tr.ExpiresIn)

	_, err = s.creds.upsertCredentialByInstance(
		"linkedin", linkedinInstanceURL, name,
		tr.AccessToken, "", actorID, expiresAt,
	)
	if err != nil {
		http.Error(w, "failed to save credential", http.StatusInternalServerError)
		return
	}

	// Success response.
	if s.linkedin.cfg.SuccessURL != "" {
		http.Redirect(w, r, s.linkedin.cfg.SuccessURL, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Connected</title></head>
<body><p>Connected to LinkedIn successfully. You can close this tab.</p></body>
</html>`)
}

// handleXCallback processes the OAuth 2.0 + PKCE callback from X (Twitter).
// It validates the state, retrieves the stored code_verifier, exchanges the
// code for tokens, and persists the resulting PlatformCredential.
func (s *Social) handleXCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	oauthError := r.URL.Query().Get("error")

	if oauthError != "" {
		http.Error(w, "OAuth error: "+oauthError, http.StatusBadRequest)
		return
	}
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	platform, codeVerifier, err := consumeOAuthState(s.creds.db, state)
	if err != nil {
		http.Error(w, "invalid or expired OAuth state", http.StatusBadRequest)
		return
	}
	if platform != "x" {
		http.Error(w, "unexpected platform: "+platform, http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	tc := s.twitter
	s.mu.RUnlock()
	if tc == nil {
		http.Error(w, "X is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	tr, err := tc.exchangeCode(r.Context(), code, codeVerifier)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	var expiresAt *time.Time
	if tr.ExpiresIn > 0 {
		t := time.Now().UTC().Add(time.Duration(tr.ExpiresIn) * time.Second)
		expiresAt = &t
	}

	_, err = s.creds.upsertCredentialByInstance(
		"x", xAPIBase, "X account",
		tr.AccessToken, tr.RefreshToken, "", expiresAt,
	)
	if err != nil {
		http.Error(w, "failed to save credential", http.StatusInternalServerError)
		return
	}

	// Success response.
	if tc.cfg.SuccessURL != "" {
		http.Redirect(w, r, tc.cfg.SuccessURL, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Connected</title></head>
<body><p>Connected to X successfully. You can close this tab.</p></body>
</html>`)
}

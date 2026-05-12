package forgesocial

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	forge "forge-cms.dev/forge"
)

// oauthStateTTL is the maximum age of an OAuth state token.
const oauthStateTTL = 10 * time.Minute

// insertOAuthState stores a new OAuth state token for the given platform.
func insertOAuthState(db forge.DB, state, platform string) error {
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO forge_social_oauth_states (state, platform, created_at)
		VALUES (?, ?, ?)`,
		state, platform, time.Now().UTC(),
	)
	return err
}

// consumeOAuthState validates and deletes the OAuth state token.
// Returns the platform associated with the state, or an error if the state
// is unknown or older than oauthStateTTL.
func consumeOAuthState(db forge.DB, state string) (string, error) {
	var platform string
	var createdAt time.Time
	err := db.QueryRowContext(context.Background(), `
		SELECT platform, created_at FROM forge_social_oauth_states WHERE state=?`, state,
	).Scan(&platform, &createdAt)

	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("forgesocial: unknown OAuth state")
	}
	if err != nil {
		return "", err
	}

	// Delete regardless of TTL — state is single-use.
	_, _ = db.ExecContext(context.Background(),
		`DELETE FROM forge_social_oauth_states WHERE state=?`, state)

	if time.Since(createdAt) > oauthStateTTL {
		return "", fmt.Errorf("forgesocial: OAuth state expired")
	}

	return platform, nil
}

// purgeExpiredOAuthStates removes state rows older than oauthStateTTL.
// Called periodically to prevent table bloat.
func purgeExpiredOAuthStates(db forge.DB) error {
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

	platform, err := consumeOAuthState(s.creds.db, state)
	if err != nil {
		http.Error(w, "invalid or expired OAuth state", http.StatusBadRequest)
		return
	}
	if platform != "mastodon" {
		http.Error(w, "unexpected platform: "+platform, http.StatusBadRequest)
		return
	}

	tr, err := s.mastodon.exchangeCode(r.Context(), code)
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

	platform, err := consumeOAuthState(s.creds.db, state)
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

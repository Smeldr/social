package forgesocial

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	forge "forge-cms.dev/forge"
)

// PlatformCredential stores OAuth 2.0 credentials for a social platform.
// AccessToken and RefreshToken are stored encrypted in the database and
// are never exposed through MCP responses.
type PlatformCredential struct {
	ID          string     `json:"id"`
	Platform    string     `json:"platform"`
	Name        string     `json:"name"`
	InstanceURL string     `json:"instance_url"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// accessToken and refreshToken are decrypted values held in memory only.
	// They are populated by getCredential/listCredentials when the caller
	// needs to make API calls, and are never marshaled to JSON.
	accessToken  string
	refreshToken string
}

// credentialStore provides DB helpers for PlatformCredential.
// It holds the AES-256-GCM key derived from Config.Secret.
type credentialStore struct {
	db     forge.DB
	appKey [32]byte
}

func newCredentialStore(db forge.DB, secret []byte) *credentialStore {
	key := sha256.Sum256(secret)
	return &credentialStore{db: db, appKey: key}
}

// encryptToken encrypts plaintext using AES-256-GCM.
// The nonce is prepended to the ciphertext; the combined value is base64-encoded.
func (cs *credentialStore) encryptToken(plaintext string) (string, error) {
	block, err := aes.NewCipher(cs.appKey[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// decryptToken decrypts a base64-encoded AES-256-GCM ciphertext.
func (cs *credentialStore) decryptToken(enc string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("forgesocial: token base64 decode: %w", err)
	}
	block, err := aes.NewCipher(cs.appKey[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("forgesocial: token ciphertext too short")
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("forgesocial: token decrypt: %w", err)
	}
	return string(plain), nil
}

// upsertCredential inserts or replaces a credential row.
// On conflict (same platform + instance_url), the existing row is updated.
// accessToken and refreshToken are encrypted before storage.
func (cs *credentialStore) upsertCredential(cred PlatformCredential) error {
	encAccess, err := cs.encryptToken(cred.accessToken)
	if err != nil {
		return fmt.Errorf("forgesocial: encrypt access token: %w", err)
	}
	encRefresh, err := cs.encryptToken(cred.refreshToken)
	if err != nil {
		return fmt.Errorf("forgesocial: encrypt refresh token: %w", err)
	}
	now := time.Now().UTC()
	_, err = cs.db.ExecContext(context.Background(), `
		INSERT INTO forge_social_credentials
			(id, platform, name, instance_url, access_token, refresh_token, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			expires_at=excluded.expires_at,
			updated_at=excluded.updated_at`,
		cred.ID, cred.Platform, cred.Name, cred.InstanceURL,
		encAccess, encRefresh, nullTime(cred.ExpiresAt),
		now, now,
	)
	return err
}

// upsertCredentialByInstance updates an existing credential row matching
// (platform, instance_url), or inserts a new row. Returns the credential ID.
func (cs *credentialStore) upsertCredentialByInstance(platform, instanceURL, name, accessToken, refreshToken string, expiresAt *time.Time) (string, error) {
	encAccess, err := cs.encryptToken(accessToken)
	if err != nil {
		return "", fmt.Errorf("forgesocial: encrypt access token: %w", err)
	}
	encRefresh, err := cs.encryptToken(refreshToken)
	if err != nil {
		return "", fmt.Errorf("forgesocial: encrypt refresh token: %w", err)
	}
	now := time.Now().UTC()

	// Check for existing row.
	var existingID string
	err = cs.db.QueryRowContext(context.Background(),
		`SELECT id FROM forge_social_credentials WHERE platform=? AND instance_url=?`,
		platform, instanceURL,
	).Scan(&existingID)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	if existingID != "" {
		// Update existing credential.
		_, err = cs.db.ExecContext(context.Background(), `
			UPDATE forge_social_credentials
			SET name=?, access_token=?, refresh_token=?, expires_at=?, updated_at=?
			WHERE id=?`,
			name, encAccess, encRefresh, nullTime(expiresAt), now, existingID,
		)
		return existingID, err
	}

	// Insert new credential.
	id := forge.NewID()
	_, err = cs.db.ExecContext(context.Background(), `
		INSERT INTO forge_social_credentials
			(id, platform, name, instance_url, access_token, refresh_token, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, platform, name, instanceURL,
		encAccess, encRefresh, nullTime(expiresAt),
		now, now,
	)
	return id, err
}

// getCredential returns a credential by ID with decrypted tokens.
// Returns forge.ErrNotFound when no row exists.
func (cs *credentialStore) getCredential(id string) (PlatformCredential, error) {
	var c PlatformCredential
	var encAccess, encRefresh string
	var expiresAt sql.NullTime
	err := cs.db.QueryRowContext(context.Background(), `
		SELECT id, platform, name, instance_url, access_token, refresh_token,
		       expires_at, created_at, updated_at
		FROM forge_social_credentials WHERE id=?`, id,
	).Scan(
		&c.ID, &c.Platform, &c.Name, &c.InstanceURL,
		&encAccess, &encRefresh,
		&expiresAt, &c.CreatedAt, &c.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return c, forge.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		c.ExpiresAt = &t
	}
	c.accessToken, err = cs.decryptToken(encAccess)
	if err != nil {
		return c, err
	}
	c.refreshToken, err = cs.decryptToken(encRefresh)
	if err != nil {
		return c, err
	}
	return c, nil
}

// listCredentials returns all credentials without token fields decrypted.
// Token fields in the returned structs are empty — callers that need tokens
// must call getCredential by ID.
func (cs *credentialStore) listCredentials() ([]PlatformCredential, error) {
	rows, err := cs.db.QueryContext(context.Background(), `
		SELECT id, platform, name, instance_url, expires_at, created_at, updated_at
		FROM forge_social_credentials
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PlatformCredential
	for rows.Next() {
		var c PlatformCredential
		var expiresAt sql.NullTime
		if err := rows.Scan(
			&c.ID, &c.Platform, &c.Name, &c.InstanceURL,
			&expiresAt, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			c.ExpiresAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// deleteCredential permanently removes a credential row.
// Returns forge.ErrNotFound when no row exists.
func (cs *credentialStore) deleteCredential(id string) error {
	res, err := cs.db.ExecContext(context.Background(),
		`DELETE FROM forge_social_credentials WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return forge.ErrNotFound
	}
	return nil
}

// nullTime converts a *time.Time to sql.NullTime for storage.
func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

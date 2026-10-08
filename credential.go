package social

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

	"smeldr.dev/core"
)

// PlatformCredential stores OAuth 2.0 credentials for a social platform.
// AccessToken and RefreshToken are stored encrypted in the database and
// are never exposed through MCP responses.
type PlatformCredential struct {
	ID          string `json:"id"`
	Platform    string `json:"platform"`
	Name        string `json:"name"`
	InstanceURL string `json:"instance_url"`
	// ActorID is the platform-specific author identifier used when publishing.
	// For LinkedIn this is the person URN ("urn:li:person:{sub}").
	// For Mastodon it is unused and stored as an empty string.
	ActorID   string     `json:"actor_id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`

	// accessToken and refreshToken are decrypted values held in memory only.
	// They are populated by getCredential/listCredentials when the caller
	// needs to make API calls, and are never marshaled to JSON.
	accessToken  string
	refreshToken string
}

// credentialStore provides DB helpers for PlatformCredential.
// It holds the AES-256-GCM key derived from Config.Secret.
type credentialStore struct {
	db     smeldr.DB
	appKey [32]byte
}

func newCredentialStore(db smeldr.DB, secret []byte) *credentialStore {
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
		return "", fmt.Errorf("social: token base64 decode: %w", err)
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
		return "", fmt.Errorf("social: token ciphertext too short")
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("social: token decrypt: %w", err)
	}
	return string(plain), nil
}

// upsertCredentialByInstance updates an existing credential row matching
// (platform, instance_url), or inserts a new row. Returns the credential ID.
// actorID is the platform-specific author identifier (e.g. LinkedIn person URN);
// pass an empty string for platforms that do not use it (Mastodon).
func (cs *credentialStore) upsertCredentialByInstance(platform, instanceURL, name, accessToken, refreshToken, actorID string, expiresAt *time.Time) (string, error) {
	encAccess, err := cs.encryptToken(accessToken)
	if err != nil {
		return "", fmt.Errorf("social: encrypt access token: %w", err)
	}
	encRefresh, err := cs.encryptToken(refreshToken)
	if err != nil {
		return "", fmt.Errorf("social: encrypt refresh token: %w", err)
	}
	now := time.Now().UTC()

	// Check for existing row.
	var existingID string
	err = cs.db.QueryRowContext(context.Background(),
		`SELECT id FROM smeldr_social_credentials WHERE platform=$1 AND instance_url=$2`,
		platform, instanceURL,
	).Scan(&existingID)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	if existingID != "" {
		// Update existing credential.
		_, err = cs.db.ExecContext(context.Background(), `
			UPDATE smeldr_social_credentials
			SET name=$1, actor_id=$2, access_token=$3, refresh_token=$4, expires_at=$5, updated_at=$6
			WHERE id=$7`,
			name, actorID, encAccess, encRefresh, nullTime(expiresAt), now, existingID,
		)
		return existingID, err
	}

	// Insert new credential.
	id := smeldr.NewID()
	_, err = cs.db.ExecContext(context.Background(), `
		INSERT INTO smeldr_social_credentials
			(id, platform, name, instance_url, actor_id, access_token, refresh_token, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, platform, name, instanceURL, actorID,
		encAccess, encRefresh, nullTime(expiresAt),
		now, now,
	)
	return id, err
}

// getCredential returns a credential by ID with decrypted tokens.
// Returns smeldr.ErrNotFound when no row exists.
func (cs *credentialStore) getCredential(id string) (PlatformCredential, error) {
	var c PlatformCredential
	var encAccess, encRefresh string
	var expiresAt sql.NullTime
	err := cs.db.QueryRowContext(context.Background(), `
		SELECT id, platform, name, instance_url, actor_id, access_token, refresh_token,
		       expires_at, created_at, updated_at
		FROM smeldr_social_credentials WHERE id=$1`, id,
	).Scan(
		&c.ID, &c.Platform, &c.Name, &c.InstanceURL, &c.ActorID,
		&encAccess, &encRefresh,
		&expiresAt, &c.CreatedAt, &c.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return c, smeldr.ErrNotFound
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
		SELECT id, platform, name, instance_url, actor_id, expires_at, created_at, updated_at
		FROM smeldr_social_credentials
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
			&c.ID, &c.Platform, &c.Name, &c.InstanceURL, &c.ActorID,
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

// deleteCredential permanently removes a credential row. A credential that
// posts still use is refused with smeldr.ErrConflict (409) naming how many:
// the posts reference it, Postgres enforces that, and deleting it would leave
// them unable to publish. Delete or move those posts first. Returns
// smeldr.ErrNotFound when no row exists.
func (cs *credentialStore) deleteCredential(id string) error {
	var posts int
	if err := cs.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM smeldr_social_posts WHERE credential_id=$1`, id).Scan(&posts); err != nil {
		return err
	}
	if posts > 0 {
		return fmt.Errorf("%w: credential %s is used by %d post(s); delete them or move them to another credential first",
			smeldr.ErrConflict, id, posts)
	}
	res, err := cs.db.ExecContext(context.Background(),
		`DELETE FROM smeldr_social_credentials WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return smeldr.ErrNotFound
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

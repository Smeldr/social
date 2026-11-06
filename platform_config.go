package social

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"smeldr.dev/core"
)

// PlatformConfig holds the operator-supplied OAuth 2.0 app credentials for a
// social platform. One row per platform in smeldr_social_platform_config.
type PlatformConfig struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURL  string `json:"redirect_url"`
	// InstanceURL is the Mastodon instance base URL (e.g. https://mastodon.social).
	// Empty for all other platforms.
	InstanceURL string `json:"instance_url,omitempty"`
	// SuccessURL is an optional redirect URL shown after a successful OAuth callback.
	SuccessURL string `json:"success_url,omitempty"`
	// Scopes is the list of OAuth 2.0 scopes to request during authorisation.
	// If empty, the platform's default scope is used.
	// For Mastodon the default is "write:statuses write:media".
	Scopes []string `json:"scopes,omitempty"`
}

// platformConfigStore persists per-platform OAuth app credentials in the DB.
// Config blobs are encrypted with AES-256-GCM using a key derived from
// the application secret.
type platformConfigStore struct {
	db     smeldr.DB
	appKey [32]byte
}

func newPlatformConfigStore(db smeldr.DB, secret []byte) *platformConfigStore {
	key := sha256.Sum256(secret)
	return &platformConfigStore{db: db, appKey: key}
}

func (s *platformConfigStore) encrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(s.appKey[:])
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
	ct := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (s *platformConfigStore) decrypt(enc string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("social: platform config base64 decode: %w", err)
	}
	block, err := aes.NewCipher(s.appKey[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, fmt.Errorf("social: platform config ciphertext too short")
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("social: platform config decrypt: %w", err)
	}
	return plain, nil
}

// save encrypts and upserts the platform config into the DB.
func (s *platformConfigStore) save(platform string, cfg PlatformConfig) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("social: marshal platform config: %w", err)
	}
	enc, err := s.encrypt(b)
	if err != nil {
		return fmt.Errorf("social: encrypt platform config: %w", err)
	}
	_, err = s.db.ExecContext(context.Background(), `
		INSERT INTO smeldr_social_platform_config (platform, config, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT(platform) DO UPDATE SET config=excluded.config, updated_at=excluded.updated_at`,
		platform, enc, time.Now().UTC(),
	)
	return err
}

// load returns the decrypted PlatformConfig for a platform.
// Returns (zero, false, nil) when no config exists for that platform.
func (s *platformConfigStore) load(platform string) (PlatformConfig, bool, error) {
	var enc string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT config FROM smeldr_social_platform_config WHERE platform=$1`, platform,
	).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return PlatformConfig{}, false, nil
	}
	if err != nil {
		return PlatformConfig{}, false, err
	}
	plain, err := s.decrypt(enc)
	if err != nil {
		return PlatformConfig{}, false, err
	}
	var cfg PlatformConfig
	if err := json.Unmarshal(plain, &cfg); err != nil {
		return PlatformConfig{}, false, fmt.Errorf("social: unmarshal platform config: %w", err)
	}
	return cfg, true, nil
}

// PlatformConfigSummary is what list_platform_configs and
// get_platform_config return for one platform: the stored config without
// its client secret, which is never returned (T1150).
type PlatformConfigSummary struct {
	Platform        string    `json:"platform"`
	ClientID        string    `json:"client_id"`
	ClientSecretSet bool      `json:"client_secret_set"`
	RedirectURL     string    `json:"redirect_url"`
	InstanceURL     string    `json:"instance_url"`
	SuccessURL      string    `json:"success_url"`
	Scopes          []string  `json:"scopes"`
	UpdatedAt       time.Time `json:"updated_at"`
	// Error is set when the stored config cannot be read, for example after
	// the application secret changed; the other fields are then empty.
	Error string `json:"error,omitempty"`
}

// GetSlug returns the platform, the identifier get_platform_config takes.
func (p PlatformConfigSummary) GetSlug() string { return p.Platform }

// summaries returns one summary per stored platform config, ordered by
// platform, or only platform's when it is not empty. A row that does not
// decrypt is listed with Error set rather than failing the whole list.
func (s *platformConfigStore) summaries(platform string) ([]PlatformConfigSummary, error) {
	q := `SELECT platform, config, updated_at FROM smeldr_social_platform_config`
	var args []any
	if platform != "" {
		q += ` WHERE platform=$1`
		args = append(args, platform)
	}
	rows, err := s.db.QueryContext(context.Background(), q+` ORDER BY platform`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlatformConfigSummary
	for rows.Next() {
		var sum PlatformConfigSummary
		var enc string
		if err := rows.Scan(&sum.Platform, &enc, &sum.UpdatedAt); err != nil {
			return nil, err
		}
		sum.UpdatedAt = sum.UpdatedAt.UTC()
		var cfg PlatformConfig
		plain, err := s.decrypt(enc)
		if err == nil {
			err = json.Unmarshal(plain, &cfg)
		}
		if err != nil {
			sum.Error = "cannot decrypt; configure it again with create_platform_config"
		} else {
			sum.ClientID = cfg.ClientID
			sum.ClientSecretSet = cfg.ClientSecret != ""
			sum.RedirectURL = cfg.RedirectURL
			sum.InstanceURL = cfg.InstanceURL
			sum.SuccessURL = cfg.SuccessURL
			sum.Scopes = cfg.Scopes
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

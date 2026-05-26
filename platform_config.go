package forgesocial

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

	forge "smeldr.dev/core"
)

// PlatformConfig holds the operator-supplied OAuth 2.0 app credentials for a
// social platform. One row per platform in forge_social_platform_config.
type PlatformConfig struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURL  string `json:"redirect_url"`
	// InstanceURL is the Mastodon instance base URL (e.g. https://mastodon.social).
	// Empty for all other platforms.
	InstanceURL string `json:"instance_url,omitempty"`
	// SuccessURL is an optional redirect URL shown after a successful OAuth callback.
	SuccessURL string `json:"success_url,omitempty"`
}

// platformConfigStore persists per-platform OAuth app credentials in the DB.
// Config blobs are encrypted with AES-256-GCM using a key derived from
// the application secret.
type platformConfigStore struct {
	db     forge.DB
	appKey [32]byte
}

func newPlatformConfigStore(db forge.DB, secret []byte) *platformConfigStore {
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
		return nil, fmt.Errorf("forgesocial: platform config base64 decode: %w", err)
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
		return nil, fmt.Errorf("forgesocial: platform config ciphertext too short")
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("forgesocial: platform config decrypt: %w", err)
	}
	return plain, nil
}

// save encrypts and upserts the platform config into the DB.
func (s *platformConfigStore) save(platform string, cfg PlatformConfig) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("forgesocial: marshal platform config: %w", err)
	}
	enc, err := s.encrypt(b)
	if err != nil {
		return fmt.Errorf("forgesocial: encrypt platform config: %w", err)
	}
	_, err = s.db.ExecContext(context.Background(), `
		INSERT INTO forge_social_platform_config (platform, config, updated_at)
		VALUES (?, ?, ?)
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
		`SELECT config FROM forge_social_platform_config WHERE platform=?`, platform,
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
		return PlatformConfig{}, false, fmt.Errorf("forgesocial: unmarshal platform config: %w", err)
	}
	return cfg, true, nil
}

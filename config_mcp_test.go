package social

// config_mcp_test.go covers config_mcp.go's read tools and refusals (T1150).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"smeldr.dev/core"
)

// TestConfigModule_ListAndGet: list_platform_configs and get_platform_config
// return what was stored, ordered by platform, and never the client secret;
// a row that does not decrypt is listed with its error.
func TestConfigModule_ListAndGet(t *testing.T) {
	s, db := newScheduleTestSocial(t)
	cm := s.ConfigModule()
	ctx := scheduleTestAdminCtx()
	for _, f := range []map[string]any{
		{"platform": "x", "client_id": "x-id", "client_secret": "x-secret-value", "redirect_url": "https://a.test/oauth/x/callback"},
		{"platform": "mastodon", "client_id": "m-id", "client_secret": "m-secret-value", "redirect_url": "https://a.test/oauth/mastodon/callback",
			"instance_url": "https://mastodon.example", "scope": "write:statuses write:media"},
	} {
		if _, err := cm.MCPCreate(ctx, f); err != nil {
			t.Fatalf("MCPCreate %v: %v", f["platform"], err)
		}
	}

	items, err := cm.MCPList(ctx)
	if err != nil {
		t.Fatalf("MCPList: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("MCPList: %d items, want 2", len(items))
	}
	m := items[0].(PlatformConfigSummary)
	if m.Platform != "mastodon" || items[1].(PlatformConfigSummary).Platform != "x" {
		t.Errorf("order = %v, %v; want mastodon, x", m.Platform, items[1].(PlatformConfigSummary).Platform)
	}
	if m.ClientID != "m-id" || !m.ClientSecretSet || m.InstanceURL != "https://mastodon.example" ||
		len(m.Scopes) != 2 || m.UpdatedAt.IsZero() || m.Error != "" || m.GetSlug() != "mastodon" {
		t.Errorf("mastodon summary = %+v", m)
	}
	raw, _ := json.Marshal(items)
	if strings.Contains(string(raw), "secret-value") || strings.Contains(string(raw), `"client_secret"`) {
		t.Errorf("list JSON carries the secret: %s", raw)
	}

	got, err := cm.MCPGet(ctx, "x")
	if err != nil || got.(PlatformConfigSummary).ClientID != "x-id" {
		t.Errorf("MCPGet x = %+v, %v", got, err)
	}
	if _, err := cm.MCPGet(ctx, "linkedin"); !errors.Is(err, smeldr.ErrNotFound) {
		t.Errorf("MCPGet linkedin: err = %v; want ErrNotFound", err)
	}

	if _, err := db.Exec(`INSERT INTO smeldr_social_platform_config (platform, config, updated_at) VALUES ('linkedin', 'not-ciphertext', $1)`,
		time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err = cm.MCPGet(ctx, "linkedin")
	if err != nil || got.(PlatformConfigSummary).Error == "" || got.(PlatformConfigSummary).ClientID != "" {
		t.Errorf("undecryptable row = %+v, %v; want an Error and no fields", got, err)
	}

	if _, err := db.Exec("DROP TABLE smeldr_social_platform_config"); err != nil {
		t.Fatal(err)
	}
	if _, err := cm.MCPList(ctx); err == nil {
		t.Error("MCPList on a missing table: no error")
	}
	if _, err := cm.MCPGet(ctx, "x"); err == nil {
		t.Error("MCPGet on a missing table: no error")
	}
}

// TestConfigModule_UnsupportedSaysWhy: the tools platform config has no use
// for still refuse, but say why and what to call instead.
func TestConfigModule_UnsupportedSaysWhy(t *testing.T) {
	s, _ := newScheduleTestSocial(t)
	cm := s.ConfigModule()
	ctx := scheduleTestAdminCtx()
	_, upd := cm.MCPUpdate(ctx, "x", nil)
	for op, err := range map[string]error{
		"update":   upd,
		"publish":  cm.MCPPublish(ctx, "x", ""),
		"schedule": cm.MCPSchedule(ctx, "x", time.Now(), ""),
		"archive":  cm.MCPArchive(ctx, "x", ""),
		"delete":   cm.MCPDelete(ctx, "x"),
	} {
		if !errors.Is(err, smeldr.ErrBadRequest) || !strings.Contains(err.Error(), "no "+op) ||
			!strings.Contains(err.Error(), "create_platform_config") {
			t.Errorf("%s: err = %v", op, err)
		}
	}
}

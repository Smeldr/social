package social

import (
	"strings"
	"time"

	"smeldr.dev/core"
)

// configModule implements [smeldr.MCPModule] for platform configuration.
// It exposes a single write tool: create_platform_config (Admin role).
// Obtain it via [Social.ConfigModule].
type configModule struct {
	social *Social
}

// MCPMeta returns the MCP registration metadata for platform configuration.
func (m *configModule) MCPMeta() smeldr.MCPMeta {
	return smeldr.MCPMeta{
		Prefix:     "/social/config",
		TypeName:   "PlatformConfig",
		Operations: []smeldr.MCPOperation{smeldr.MCPWrite},
	}
}

// MCPSchema returns the field schema for the configure_platform tool.
func (m *configModule) MCPSchema() []smeldr.MCPField {
	return []smeldr.MCPField{
		{
			Name:        "Platform",
			JSONName:    "platform",
			Type:        "string",
			Required:    true,
			Enum:        []string{"mastodon", "linkedin", "x"},
			Description: "The social platform to configure. Each platform stores its own OAuth 2.0 app credentials.",
		},
		{
			Name:        "ClientID",
			JSONName:    "client_id",
			Type:        "string",
			Required:    true,
			Description: "OAuth 2.0 client ID issued by the platform developer portal.",
		},
		{
			Name:        "ClientSecret",
			JSONName:    "client_secret",
			Type:        "string",
			Required:    true,
			Description: "OAuth 2.0 client secret issued by the platform developer portal.",
		},
		{
			Name:        "RedirectURL",
			JSONName:    "redirect_url",
			Type:        "string",
			Required:    true,
			Description: "OAuth 2.0 callback URL registered with the platform. For Mastodon: <base_url>/oauth/mastodon/callback. For LinkedIn: <base_url>/oauth/linkedin/callback. For X: <base_url>/oauth/x/callback.",
		},
		{
			Name:        "InstanceURL",
			JSONName:    "instance_url",
			Type:        "string",
			Required:    false,
			Description: "Mastodon only: base URL of the Mastodon instance, e.g. https://mastodon.social. Must not be set for LinkedIn or X.",
		},
		{
			Name:        "SuccessURL",
			JSONName:    "success_url",
			Type:        "string",
			Required:    false,
			Description: "Optional URL to redirect the browser to after a successful OAuth callback. If empty, a plain HTML confirmation is shown.",
		},
		{
			Name:        "Scope",
			JSONName:    "scope",
			Type:        "string",
			Required:    false,
			Description: `Optional: space-separated OAuth 2.0 scopes to request. X default: "tweet.read users.read tweet.write offline.access media.write". Mastodon default: "write:statuses write:media".`,
		},
	}
}

// MCPList is not supported — platform config is write-only via MCP.
func (m *configModule) MCPList(_ smeldr.Context, _ ...smeldr.Status) ([]any, error) {
	return nil, smeldr.ErrBadRequest
}

// MCPGet is not supported — platform config is write-only via MCP.
func (m *configModule) MCPGet(_ smeldr.Context, _ string) (any, error) {
	return nil, smeldr.ErrBadRequest
}

// MCPCreate stores the OAuth 2.0 app credentials for a platform.
// Replaces any existing config for that platform. Returns a confirmation
// message — never returns the stored credentials.
func (m *configModule) MCPCreate(_ smeldr.Context, fields map[string]any) (any, error) {
	platform, _ := fields["platform"].(string)
	switch platform {
	case "mastodon", "linkedin", "x":
	default:
		return nil, smeldr.Err("platform", "must be 'mastodon', 'linkedin', or 'x'")
	}

	// X has a single fixed API endpoint — instance_url is not applicable.
	if platform == "x" {
		if v, _ := fields["instance_url"].(string); v != "" {
			return nil, smeldr.Err("instance_url", "not applicable for platform 'x' — X has a single fixed API endpoint")
		}
	}

	clientID, _ := fields["client_id"].(string)
	if clientID == "" {
		return nil, smeldr.Err("client_id", "required")
	}
	clientSecret, _ := fields["client_secret"].(string)
	if clientSecret == "" {
		return nil, smeldr.Err("client_secret", "required")
	}
	redirectURL, _ := fields["redirect_url"].(string)
	if redirectURL == "" {
		return nil, smeldr.Err("redirect_url", "required")
	}

	instanceURL := stringField(fields, "instance_url")
	if platform == "mastodon" && instanceURL == "" {
		return nil, smeldr.Err("instance_url", "required for platform='mastodon'")
	}

	var scopes []string
	if s := stringField(fields, "scope"); s != "" {
		scopes = strings.Fields(s)
	}

	cfg := PlatformConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		InstanceURL:  instanceURL,
		SuccessURL:   stringField(fields, "success_url"),
		Scopes:       scopes,
	}
	if err := m.social.platformCfgs.save(platform, cfg); err != nil {
		return nil, err
	}

	// Re-initialise the platform client so the new config takes effect
	// for subsequent publishes without requiring a restart.
	m.social.reloadPlatformClient(platform, cfg)

	return map[string]string{
		"message": "Platform '" + platform + "' configured successfully.",
	}, nil
}

// MCPUpdate is not supported.
func (m *configModule) MCPUpdate(_ smeldr.Context, _ string, _ map[string]any) (any, error) {
	return nil, smeldr.ErrBadRequest
}

// MCPPublish is not supported.
func (m *configModule) MCPPublish(_ smeldr.Context, _ string) error {
	return smeldr.ErrBadRequest
}

// MCPSchedule is not supported.
func (m *configModule) MCPSchedule(_ smeldr.Context, _ string, _ time.Time) error {
	return smeldr.ErrBadRequest
}

// MCPArchive is not supported.
func (m *configModule) MCPArchive(_ smeldr.Context, _ string) error {
	return smeldr.ErrBadRequest
}

// MCPDelete is not supported.
func (m *configModule) MCPDelete(_ smeldr.Context, _ string) error {
	return smeldr.ErrBadRequest
}

package social

import (
	"fmt"
	"strings"
	"time"

	"smeldr.dev/core"
)

// configModule implements [smeldr.MCPModule] for platform configuration.
// create_platform_config (Admin role) stores a platform's config;
// list_platform_configs and get_platform_config read it back without the
// client secret.
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

// MCPList returns one [PlatformConfigSummary] per configured platform,
// without the client secret (T1150).
func (m *configModule) MCPList(_ smeldr.Context, _ ...smeldr.Status) ([]any, error) {
	sums, err := m.social.platformCfgs.summaries("")
	if err != nil {
		return nil, err
	}
	out := make([]any, len(sums))
	for i, sum := range sums {
		out[i] = sum
	}
	return out, nil
}

// MCPGet returns the [PlatformConfigSummary] for the platform named by slug,
// or smeldr.ErrNotFound when it is not configured.
func (m *configModule) MCPGet(_ smeldr.Context, slug string) (any, error) {
	sums, err := m.social.platformCfgs.summaries(slug)
	if err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return nil, smeldr.ErrNotFound
	}
	return sums[0], nil
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
	return nil, unsupportedConfigOp("update")
}

// MCPPublish is not supported.
func (m *configModule) MCPPublish(_ smeldr.Context, _, _ string) error {
	return unsupportedConfigOp("publish")
}

// MCPSchedule is not supported.
func (m *configModule) MCPSchedule(_ smeldr.Context, _ string, _ time.Time, _ string) error {
	return unsupportedConfigOp("schedule")
}

// MCPArchive is not supported.
func (m *configModule) MCPArchive(_ smeldr.Context, _, _ string) error {
	return unsupportedConfigOp("archive")
}

// MCPDelete is not supported.
func (m *configModule) MCPDelete(_ smeldr.Context, _ string) error {
	return unsupportedConfigOp("delete")
}

// unsupportedConfigOp is the refusal for a platform config tool that has no
// meaning here, with the reason and what to do instead (T1150).
func unsupportedConfigOp(op string) error {
	return fmt.Errorf("%w: platform config has no %s; call create_platform_config again to replace it",
		smeldr.ErrBadRequest, op)
}

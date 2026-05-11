package forgesocial

import (
	"context"
	"fmt"
	"time"

	forge "forge-cms.dev/forge"
)

// ─── postModule — MCPModule for ScheduledPost ─────────────────────────────────

// postModule implements [forge.MCPModule] for [ScheduledPost].
// Obtain it via [Social.PostModule].
type postModule struct {
	social *Social
}

// MCPMeta returns the MCP registration metadata for ScheduledPost.
func (m *postModule) MCPMeta() forge.MCPMeta {
	return forge.MCPMeta{
		Prefix:     "/social/posts",
		TypeName:   "ScheduledPost",
		Operations: []forge.MCPOperation{forge.MCPRead, forge.MCPWrite},
	}
}

// MCPSchema returns the field schema for ScheduledPost.
func (m *postModule) MCPSchema() []forge.MCPField {
	return []forge.MCPField{
		{
			Name:        "CredentialID",
			JSONName:    "credential_id",
			Type:        "string",
			Required:    true,
			Description: "ID of the MastodonCredential to use for publishing.",
		},
		{
			Name:        "Body",
			JSONName:    "body",
			Type:        "string",
			Required:    true,
			Format:      "markdown",
			Description: "Post body text. Mastodon limit is 500 characters. Markdown is rendered as plain text.",
		},
		{
			Name:        "MediaURL",
			JSONName:    "media_url",
			Type:        "string",
			Required:    false,
			Description: "Optional URL of an image to attach. Must be an accessible HTTP(S) URL.",
		},
		{
			Name:        "AltText",
			JSONName:    "alt_text",
			Type:        "string",
			Required:    false,
			Description: "Alt text for the attached image. Required when media_url is set (WCAG 1.1.1).",
		},
		{
			Name:        "ScheduledAt",
			JSONName:    "scheduled_at",
			Type:        "datetime",
			Required:    false,
			Description: "RFC3339 datetime to publish the post. Omit to create a draft. Set status to 'scheduled' to activate.",
		},
		{
			Name:        "Status",
			JSONName:    "status",
			Type:        "string",
			Required:    false,
			Enum:        []string{"draft", "scheduled", "archived"},
			Description: "Post lifecycle status. Omit on create (defaults to 'draft'). Use publish_scheduled_post to publish immediately.",
		},
	}
}

// MCPList returns all ScheduledPost rows as []any.
// Lifecycle status filter maps forge.Status values: Published→published, Draft→draft, etc.
func (m *postModule) MCPList(_ forge.Context, statuses ...forge.Status) ([]any, error) {
	var ps []PostStatus
	for _, s := range statuses {
		ps = append(ps, PostStatus(string(s)))
	}
	posts, err := listPosts(m.social.db, ps...)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(posts))
	for i, p := range posts {
		out[i] = p
	}
	return out, nil
}

// MCPGet returns the ScheduledPost with the given slug (= ID).
func (m *postModule) MCPGet(_ forge.Context, slug string) (any, error) {
	p, err := getPost(m.social.db, slug)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// MCPCreate creates a new ScheduledPost with status=draft.
// If scheduled_at is provided, status is automatically set to "scheduled".
func (m *postModule) MCPCreate(_ forge.Context, fields map[string]any) (any, error) {
	credentialID, _ := fields["credential_id"].(string)
	if credentialID == "" {
		return nil, forge.Err("credential_id", "required")
	}
	body, _ := fields["body"].(string)
	if body == "" {
		return nil, forge.Err("body", "required")
	}

	now := time.Now().UTC()
	p := ScheduledPost{
		ID:           forge.NewID(),
		Platform:     "mastodon",
		CredentialID: credentialID,
		Body:         body,
		MediaURL:     stringField(fields, "media_url"),
		AltText:      stringField(fields, "alt_text"),
		Status:       PostStatusDraft,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if p.MediaURL != "" && p.AltText == "" {
		return nil, forge.Err("alt_text", "required when media_url is set")
	}

	if raw, ok := fields["scheduled_at"].(string); ok && raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, forge.Err("scheduled_at", "must be RFC3339 datetime")
		}
		utc := t.UTC()
		p.ScheduledAt = &utc
		p.Status = PostStatusScheduled
	}

	if err := insertPost(m.social.db, p); err != nil {
		return nil, err
	}
	return p, nil
}

// MCPUpdate applies a partial update to the ScheduledPost with the given slug.
// Only fields present in the fields map are changed. Published or failed posts
// cannot be updated.
func (m *postModule) MCPUpdate(_ forge.Context, slug string, fields map[string]any) (any, error) {
	p, err := getPost(m.social.db, slug)
	if err != nil {
		return nil, err
	}
	if p.Status == PostStatusPublished || p.Status == PostStatusFailed {
		return nil, forge.Err("status", fmt.Sprintf("cannot update a %s post", p.Status))
	}

	if v, ok := fields["body"].(string); ok {
		p.Body = v
	}
	if v, ok := fields["media_url"].(string); ok {
		p.MediaURL = v
	}
	if v, ok := fields["alt_text"].(string); ok {
		p.AltText = v
	}
	if v, ok := fields["credential_id"].(string); ok {
		p.CredentialID = v
	}

	if raw, ok := fields["scheduled_at"].(string); ok {
		if raw == "" {
			p.ScheduledAt = nil
		} else {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return nil, forge.Err("scheduled_at", "must be RFC3339 datetime")
			}
			utc := t.UTC()
			p.ScheduledAt = &utc
		}
	}

	if v, ok := fields["status"].(string); ok {
		switch PostStatus(v) {
		case PostStatusDraft, PostStatusScheduled, PostStatusArchived:
			p.Status = PostStatus(v)
		default:
			return nil, forge.Err("status", "must be draft, scheduled, or archived")
		}
	}

	// Auto-promote to scheduled when scheduled_at is set and status is draft.
	if p.ScheduledAt != nil && p.Status == PostStatusDraft {
		p.Status = PostStatusScheduled
	}

	if p.MediaURL != "" && p.AltText == "" {
		return nil, forge.Err("alt_text", "required when media_url is set")
	}

	if err := updatePost(m.social.db, p); err != nil {
		return nil, err
	}
	return p, nil
}

// MCPPublish publishes the ScheduledPost immediately, bypassing the scheduler.
// This is the "publish_now" operation.
func (m *postModule) MCPPublish(ctx forge.Context, slug string) error {
	p, err := getPost(m.social.db, slug)
	if err != nil {
		return err
	}
	if p.Status == PostStatusPublished {
		return forge.Err("status", "post is already published")
	}
	if p.Status == PostStatusArchived {
		return forge.Err("status", "archived posts cannot be published")
	}
	return m.social.publishNow(context.Background(), p)
}

// MCPSchedule sets the ScheduledPost to publish at the given time.
func (m *postModule) MCPSchedule(_ forge.Context, slug string, at time.Time) error {
	p, err := getPost(m.social.db, slug)
	if err != nil {
		return err
	}
	if p.Status == PostStatusPublished || p.Status == PostStatusArchived {
		return forge.Err("status", fmt.Sprintf("cannot schedule a %s post", p.Status))
	}
	utc := at.UTC()
	p.ScheduledAt = &utc
	p.Status = PostStatusScheduled
	return updatePost(m.social.db, p)
}

// MCPArchive transitions the ScheduledPost to archived.
func (m *postModule) MCPArchive(_ forge.Context, slug string) error {
	p, err := getPost(m.social.db, slug)
	if err != nil {
		return err
	}
	if p.Status == PostStatusPublished {
		return forge.Err("status", "published posts cannot be archived via this tool")
	}
	p.Status = PostStatusArchived
	return updatePost(m.social.db, p)
}

// MCPDelete permanently deletes the ScheduledPost.
func (m *postModule) MCPDelete(_ forge.Context, slug string) error {
	return deletePost(m.social.db, slug)
}

// ─── credentialModule — MCPModule for PlatformCredential ──────────────────────

// credentialModule implements [forge.MCPModule] for [PlatformCredential].
// Obtain it via [Social.CredentialModule].
type credentialModule struct {
	social *Social
}

// MCPMeta returns the MCP registration metadata for PlatformCredential.
// TypeName "MastodonCredential" produces tools named create_mastodon_credential,
// list_mastodon_credentials, etc.
func (m *credentialModule) MCPMeta() forge.MCPMeta {
	return forge.MCPMeta{
		Prefix:     "/social/credentials",
		TypeName:   "MastodonCredential",
		Operations: []forge.MCPOperation{forge.MCPRead, forge.MCPWrite},
	}
}

// MCPSchema returns the field schema for the credential create (OAuth connect) operation.
func (m *credentialModule) MCPSchema() []forge.MCPField {
	return []forge.MCPField{
		{
			Name:        "InstanceURL",
			JSONName:    "instance_url",
			Type:        "string",
			Required:    true,
			Description: "Base URL of the Mastodon instance, e.g. https://mastodon.social. Must match the instance registered for this application.",
		},
	}
}

// MCPList returns all PlatformCredentials as []any. Token fields are omitted.
func (m *credentialModule) MCPList(_ forge.Context, _ ...forge.Status) ([]any, error) {
	creds, err := m.social.creds.listCredentials()
	if err != nil {
		return nil, err
	}
	out := make([]any, len(creds))
	for i, c := range creds {
		out[i] = c
	}
	return out, nil
}

// MCPGet returns the PlatformCredential with the given slug (= ID).
// Token fields are omitted from the response.
func (m *credentialModule) MCPGet(_ forge.Context, slug string) (any, error) {
	// Use listCredentials and find by ID to avoid returning decrypted tokens.
	creds, err := m.social.creds.listCredentials()
	if err != nil {
		return nil, err
	}
	for _, c := range creds {
		if c.ID == slug {
			return c, nil
		}
	}
	return nil, forge.ErrNotFound
}

// MCPCreate initiates the Mastodon OAuth 2.0 flow.
// It generates an OAuth state token, stores it in the DB, and returns the
// authorization URL that the user must visit in a browser.
//
// The actual credential is not created until the OAuth callback completes at
// GET /oauth/mastodon/callback.
func (m *credentialModule) MCPCreate(_ forge.Context, fields map[string]any) (any, error) {
	instanceURL, _ := fields["instance_url"].(string)
	if instanceURL == "" {
		return nil, forge.Err("instance_url", "required")
	}

	state := forge.NewID()
	if err := insertOAuthState(m.social.creds.db, state, "mastodon"); err != nil {
		return nil, err
	}

	authURL := m.social.mastodon.authURL(state)
	return map[string]any{
		"redirect_url": authURL,
		"message":      "Visit redirect_url in a browser to authorise Mastodon. The credential will be saved automatically after authorisation.",
	}, nil
}

// MCPUpdate is not supported for credentials — use MCPCreate to reconnect.
func (m *credentialModule) MCPUpdate(_ forge.Context, _ string, _ map[string]any) (any, error) {
	return nil, forge.ErrBadRequest
}

// MCPPublish is not supported for credentials — they have no lifecycle.
func (m *credentialModule) MCPPublish(_ forge.Context, _ string) error {
	return forge.ErrBadRequest
}

// MCPSchedule is not supported for credentials.
func (m *credentialModule) MCPSchedule(_ forge.Context, _ string, _ time.Time) error {
	return forge.ErrBadRequest
}

// MCPArchive is not supported for credentials.
func (m *credentialModule) MCPArchive(_ forge.Context, _ string) error {
	return forge.ErrBadRequest
}

// MCPDelete permanently removes the credential.
// Posts that reference this credential will fail to publish after deletion.
func (m *credentialModule) MCPDelete(_ forge.Context, slug string) error {
	return m.social.creds.deleteCredential(slug)
}

// stringField safely extracts a string field from the fields map.
func stringField(fields map[string]any, key string) string {
	v, _ := fields[key].(string)
	return v
}

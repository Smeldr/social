# smeldr.dev/social

Social post scheduling and AI agent routing for [Forge](https://smeldr.dev) applications.

[![Go Reference](https://pkg.go.dev/badge/smeldr.dev/social.svg)](https://pkg.go.dev/smeldr.dev/social)
**v0.8.0 — stable.** See [CHANGELOG.md](CHANGELOG.md).

```bash
go get smeldr.dev/social@latest
```

## Migrating from v0.7.x

The package was renamed `forgesocial` → `social` in v0.8.0. Update imports from
`forgesocial.X` to `social.X` (or drop the alias: `import "smeldr.dev/social"`).
Error-string prefixes changed from `forgesocial:` to `social:`. No exported symbols
changed — only the package qualifier. The MCP wiring helper moved with mcp v1.17.0
(`forgemcp` → `mcp`). The `forge_social_*` DB tables are unchanged.

---

## What it does

`social` adds two layers to any Forge application:

**Layer 2 — Scheduler:** Create `ScheduledPost` records; the built-in scheduler publishes them to Mastodon, LinkedIn, or X (Twitter) at the right time, with exponential backoff and automatic retries.

**Layer 1 — Agent routing:** Wire Forge lifecycle signals to outbound HTTP calls, so AI agents can react when content is published, scheduled, archived, or deleted.

---

## Quick start

```go
import (
    "smeldr.dev/social"
    "smeldr.dev/mcp"
)

svc := social.New(db, social.Config{
    Secret: cfg.Secret,
})

svc.Register(app)   // wire OAuth callbacks + REST endpoints
defer svc.Stop()    // drain scheduler + delivery worker on shutdown

// Wire MCP tools (optional).
mcpSrv := mcp.New(app,
    mcp.WithModule(svc.PostModule()),
    mcp.WithModule(svc.CredentialModule()),
    mcp.WithModule(svc.ConfigModule()),     // create_platform_config (Admin)
    mcp.WithModule(svc.ScheduleModule()),   // slot-queue (v0.4.0+)
)

// Wire agent routing (optional — Layer 1).
svc.AddRoutes(app,
    social.OnPublish("Post", "https://agent.example.com/hooks/post-published"),
)
```

Platform credentials are stored in the database — no environment variables required after initial setup. Call `create_platform_config` via MCP (Admin role) to configure each platform.

> **Backwards compat:** `Config.Mastodon` and `Config.LinkedIn` are still accepted as fallbacks but are deprecated. A warning is logged at startup when env-var config is present and no DB config exists for that platform.

---

## ScheduledPost workflow

```
draft → scheduled → published
              ↓
           failed (up to 5 attempts, then terminal)
              ↓
           archived
```

Create a post via MCP tool `create_scheduled_post`, set `scheduled_at`, and the scheduler handles the rest. Call `publish_scheduled_post` to publish immediately without waiting.

**Platforms:** `mastodon` (default), `linkedin`, or `x`.  
**Body limits:** Mastodon 500 characters; LinkedIn 3000 characters; X 280 characters (returns terminal error if exceeded — not truncated).  
**Media:** Set `media_url` to attach an HTTPS image URL (Mastodon and LinkedIn only; X media is not yet supported).

---

## Slot queue (v0.4.0+)

Instead of a fixed `scheduled_at` time, posts can be queued for the next available slot in a `PublicationSchedule`.

### Create a schedule

```go
// Via MCP: create_publication_schedule
// credential_id: the ID of a connected SocialCredential
// slots: JSON array of slot objects
// status: "active" (default) or "paused"
```

Slot format:

```json
{ "weekday": 1, "time": "09:00", "timezone": "Europe/Copenhagen" }
```

- `weekday`: 0 = Sunday … 6 = Saturday (matches Go `time.Weekday`)
- `time`: HH:MM in 24-hour format
- `timezone`: IANA timezone name (e.g. `"Europe/Copenhagen"`, `"America/New_York"`)

Each credential may have at most one schedule.

### Queue a post

Set `status: "queued"` on `create_scheduled_post` (omit `scheduled_at`). The scheduler dequeues the oldest queued post for each credential whenever a slot fires.

```
draft → queued → published
```

### Catch-up policy

If the server was offline when a slot fired, the scheduler catches up on the next tick. One post is published per missed slot. The total catch-up per tick is capped at `len(slots)` to avoid flooding.

### Schedule status

- `active` — slots fire normally
- `paused` — slots are skipped; queued posts remain in the queue

---

## OAuth connect flows

### Mastodon

1. Call `create_platform_config` (Admin) — set `platform=mastodon`, `client_id`, `client_secret`, `redirect_url`, `instance_url`. Optional: `scope` (space-separated, default `"write:statuses write:media"`)
2. Call `create_social_credential` (MCP) → get `redirect_url`
3. Operator visits `redirect_url` in browser and authorises
4. Callback stores encrypted token automatically
5. Tokens do not expire — connect once

### LinkedIn

1. Call `create_platform_config` (Admin) — set `platform=linkedin`, `client_id`, `client_secret`, `redirect_url`
2. Call `create_social_credential` (MCP) with `platform=linkedin` → get `redirect_url`
3. Operator visits `redirect_url` in browser and authorises
4. Callback stores token + person URN automatically
5. **Tokens expire after 60 days** — repeat OAuth flow to reconnect

### X (Twitter) — OAuth 2.0 + PKCE (v0.5.0+)

1. Call `create_platform_config` (Admin) — set `platform=x`, `client_id`, `client_secret`, `redirect_url`
2. Call `create_social_credential` (MCP) with `platform=x` → get `redirect_url` (contains PKCE challenge — single use)
3. Operator visits `redirect_url` in browser and authorises
4. Callback validates PKCE code verifier and stores token automatically
5. The `code_verifier` is stored server-side; the agent never sees it

OAuth tokens are encrypted at rest with AES-256-GCM, keyed from `Config.Secret`. Never stored in plaintext.

---

## MCP tools

| Tool | Description |
|------|-------------|
| `create_platform_config` | Configure OAuth 2.0 app credentials for a platform (Admin) |
| `create_scheduled_post` | Create a draft post |
| `list_scheduled_posts` | List posts by status |
| `publish_scheduled_post` | Publish immediately or retry a failed post |
| `archive_scheduled_post` | Archive a post |
| `delete_scheduled_post` | Permanently delete a post |
| `create_social_credential` | Initiate OAuth connect flow; returns `redirect_url` |
| `list_social_credentials` | List all credentials |
| `get_social_credential` | Read a credential by ID |
| `delete_social_credential` | Delete a credential |
| `create_publication_schedule` | Create a recurring slot schedule for a credential |
| `get_publication_schedule` | Read a schedule by ID |
| `update_publication_schedule` | Update slots or pause/resume |
| `list_publication_schedules` | List all schedules |
| `delete_publication_schedule` | Delete a schedule |

---

## REST API

`svc.Register(app)` wires REST endpoints auto-generated by Forge:

```
POST   /social/posts
GET    /social/posts
GET    /social/posts/{slug}
PUT    /social/posts/{slug}
DELETE /social/posts/{slug}
```

Bearer token required on all endpoints.

---

## Agent routing (Layer 1)

`AddRoutes` is one way to act on Forge signals. For in-process handlers (audit logs, cache invalidation, SSE), use `app.OnSignal()` directly — see the [Signal bus docs](https://smeldr.dev/docs/signal-bus).

```go
svc.AddRoutes(app,
    social.OnPublish("Post",   "https://agent.example.com/hook"),
    social.OnArchive("Post",   "https://agent.example.com/hook"),
    social.OnSchedule("Event", "https://agent.example.com/hook"),
    social.OnDelete("Post",    "https://agent.example.com/hook"),
)
```

**SSRF protection:** Agent URLs must be HTTPS. Private IPs, `localhost`, and `.local` domains are rejected at startup (panic).

### Payload

```json
{
  "type":           "Post",
  "slug":           "my-post",
  "title":          "My Post",
  "url":            "https://mysite.com/posts/my-post",
  "timestamp":      "2026-05-12T14:30:00Z",
  "previous_state": "",
  "actor_role":     "Author",
  "actor_id":       "user-abc"
}
```

### Signature verification

Every POST includes `X-Forge-Signature: sha256=<HMAC-SHA256 of body, key=Config.Secret>`.

```go
func verifyForgeSignature(body []byte, secret []byte, header string) bool {
    mac := hmac.New(sha256.New, secret)
    mac.Write(body)
    expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(expected), []byte(header))
}
```

### Retry schedule

| Attempt | Delay |
|---------|-------|
| 1 | 30 seconds |
| 2 | 2 minutes |
| 3 | 10 minutes |
| 4 | 1 hour |
| 5+ | Terminal |

2xx = delivered. 4xx (non-429) = terminal (no retry). 429 = honour `Retry-After`. 5xx/network = transient retry.

Jobs survive restarts — persisted in SQLite.

---

## Graceful shutdown

Always call `svc.Stop()`. It waits for the scheduler and delivery worker to finish in-flight work:

```go
defer svc.Stop()
```

---

## Requirements

- Go 1.26+
- `smeldr.dev/core` v1.20.0+
- A `smeldr.DB` (SQLite or Postgres)

---

## License

AGPL-3.0. See [LICENSE](LICENSE).

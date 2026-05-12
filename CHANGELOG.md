# forge-social Changelog

## [0.3.0] — 2026-05-12

### Added

- Layer 1 agent routing: `AddRoutes(app, ...Route)` wires signal-bus events to outbound agent HTTP calls
- `Route` type with `OnPublish`, `OnSchedule`, `OnArchive`, `OnDelete` builder functions
- SSRF protection: agent URLs validated at `AddRoutes` time — must be HTTPS, non-private, non-local
- `forge_social_route_jobs` table: pending/delivered/failed outbound delivery jobs with retry state
- `forge_social_route_log` table: per-attempt delivery log
- Route delivery worker goroutine with the same exponential-backoff table as Layer 2 (30s/2m/10m/1h/terminal)
- `X-Forge-Signature: sha256=<HMAC-SHA256>` header on every outbound POST (key = `Config.Secret`)
- Error classification: 2xx → delivered; 4xx (non-429) → terminal; 429 → honour Retry-After; 5xx/network → transient retry
- `Social.Stop()` now also drains the route delivery worker when `AddRoutes` was called

## [0.2.0] — 2026-05-12

### Added

- LinkedIn publishing via UGC Posts API (text-only; image upload deferred to v0.3.0)
- `LinkedInConfig`: OAuth 2.0 client credentials for LinkedIn
- LinkedIn OAuth 2.0 authorization code flow via `GET /oauth/linkedin/callback`
- `fetchPersonURN`: fetches `urn:li:person:{sub}` from LinkedIn OpenID Connect userinfo
- `ActorID` field on `PlatformCredential`: stores the LinkedIn person URN
- `actor_id` column on `forge_social_credentials` with idempotent `ALTER TABLE` migration
- `callPlatformPublish`: internal dispatcher routing posts to Mastodon or LinkedIn
- `platform` field on `ScheduledPost` MCP schema (default `"mastodon"`)
- `SocialCredential` MCP type replaces `MastodonCredential`; `create_social_credential` now accepts `platform` field (`"mastodon"` or `"linkedin"`)

### Changed

- `Config` now includes `LinkedIn LinkedInConfig` field (zero value disables LinkedIn)
- `credentialModule.MCPCreate` now dispatches on `platform` field; `instance_url` is only required for Mastodon
- `ScheduledPost.MCPCreate` defaults `platform` to `"mastodon"` when omitted (backward compatible)

## [0.1.0] — 2026-05-11

### Added

- `ScheduledPost`: create, schedule, and publish content to Mastodon platforms
- `PlatformCredential`: OAuth 2.0 credential storage with AES-256-GCM encryption
- Mastodon OAuth 2.0 authorization code flow via `GET /oauth/mastodon/callback`
- Internal scheduler goroutine with adaptive ticking and exponential backoff retry
- MCP tools for `ScheduledPost` (create, update, publish, archive, delete, list, get)
- MCP tools for `MastodonCredential` (create/connect, list, get, delete)
- Single-image media upload via Mastodon v2 media API
- `Social.Stop()` for graceful scheduler shutdown

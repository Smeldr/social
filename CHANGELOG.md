# smeldr.dev/social Changelog

## [0.8.3] — 2026-06-09

### Changed

- `uploadXMedia` and `publish`: `slog.Debug` before each outgoing HTTP request
  (method, URL — no credentials logged); `slog.Warn` on any non-2xx response
  (status, truncated body ≤256 chars, `X-Request-Id` from X) (A140).

---

## [0.8.2] — 2026-06-08

### Fixed

- X post body length validation now counts each URL as 23 characters (t.co
  weighting) regardless of the URL's actual length. Previously, long URLs were
  counted at their full rune length, causing valid posts to be incorrectly
  rejected. New helper `xWeightedBodyLen` + constant `xTcoURLLen = 23` (A138).

---

## [0.8.1] — 2026-06-07

### Changed

- Brand-prose sweep (T101, A135): README "Forge" → "Smeldr" (4 lines) + `verifyForgeSignature`
  example function renamed to `verifySignature`. Godoc comments in route.go, router.go,
  social.go updated. No exported-symbol or behaviour change.

---

## [0.8.0] — 2026-06-06

### Changed (breaking)

- Package renamed `forgesocial` → `social` (T100 Step 4). Update imports from
  `forgesocial.X` to `social.X` (or drop the alias: `import "smeldr.dev/social"`).
  Error/panic/log-string prefixes `forgesocial:` → `social:`. No exported symbols
  changed — only the package qualifier.
- Updated stale `forge-mcp` doc cross-references to `mcp` (renamed in mcp v1.17.0),
  and README install/import paths from `forge-cms.dev/forge-social` to
  `smeldr.dev/social`.

### Preserved

- The 8 `forge_social_*` DB table names are unchanged — renaming them is a separate
  DB migration, not part of this rename.

## [0.6.0] — 2026-05-16

### Added

- X (Twitter): images attached via `media_url` are now uploaded to the X v2 media endpoint before tweeting. The `media_id` is attached to the tweet payload so the image appears inline. Previously `media_url` was silently ignored for X posts.
- X OAuth scope updated to include `media.write`. **Existing X credentials must be re-authorised** (re-run the OAuth flow) to pick up this scope — tokens issued before this release will fail with 403 on media upload.

## [0.5.2] — 2026-05-14

### Fixed

- X (Twitter) access tokens are now proactively refreshed before publish when within 5 minutes of expiry. The stored refresh token is exchanged for a new access+refresh token pair, which is persisted to the DB before the publish call proceeds. If the refresh fails, the existing token is used and the normal retry logic handles any resulting 401. X does not always rotate the refresh token — the existing one is retained when omitted from the response.

## [0.5.1] — 2026-05-14

### Fixed

- `create_social_credential` MCP schema: `platform` enum was missing `"x"` — schema now declares `mastodon | linkedin | x`. MCPCreate already handled `"x"` correctly.

## [0.5.0] — 2026-05-14

### Added

- **DB-driven platform config** (`forge_social_platform_config` table): operator-supplied OAuth 2.0 app credentials stored encrypted (AES-256-GCM) in the database, configurable without shell access
- `create_platform_config` MCP tool (Admin role): stores client_id, client_secret, redirect_url, instance_url (Mastodon), success_url per platform. Returns a confirmation — never returns stored credentials
- `Social.ConfigModule()`: wire the create_platform_config tool via `forgemcp.WithModule(social.ConfigModule())`
- **X (Twitter) integration**: OAuth 2.0 with PKCE (`S256`); `GET /oauth/x/callback` route; `POST /2/tweets` publishing; 280-character limit enforced at publish time
- PKCE support: `code_verifier` stored in `forge_social_oauth_states` during the X OAuth dance — agents see only the authorization URL
- `connect_social_credential` extended: `platform: "x"` generates PKCE pair and returns the X authorization URL
- Platform enum extended to `mastodon | linkedin | x` in all relevant MCP tools

### Changed

- `MastodonConfig` and `LinkedInConfig` in `forgesocial.Config` are now deprecated fallbacks. DB config (set via `create_platform_config`) takes priority. A deprecation warning is logged at startup when env-var config is used.
- `callPlatformPublish` now reads platform clients under a `sync.RWMutex` — allows `create_platform_config` to hot-swap clients without a restart

### Fixed

- Mastodon: `callPlatformPublish` no longer panics when Mastodon is not configured (returns terminal error instead)

## [0.4.1] — 2026-05-13

### Fixed

- `validateSlots`: rejects empty `timezone` field with an actionable error instead of silently defaulting to UTC
- Embed Go's timezone database (`time/tzdata`) in the binary — `time.LoadLocation` now works on Alpine and scratch containers with no OS tzdata installed

## [0.4.0] — 2026-05-12

### Added

- `MastodonConfig.Scopes []string` — configurable OAuth scope; defaults to `write:statuses write:media` when empty
- `PublicationSchedule` content type: recurring weekly time slots per credential (`forge_social_publication_schedules` table)
- `Slot` type: `weekday` (0–6), `time` (HH:MM), `timezone` (IANA)
- `PostStatusQueued` (`"queued"`) — posts without `scheduled_at` enter the slot queue instead of a fixed time
- Extended scheduler: `processSlotQueue` runs on every tick — fires slots since `last_tick_at`, dequeues oldest queued post per fired slot, caps at `len(slots)` per tick, catches up on missed slots
- `Social.ScheduleModule()` returning a `forge.MCPModule` with five MCP tools: `create_publication_schedule`, `get_publication_schedule`, `update_publication_schedule`, `list_publication_schedules`, `delete_publication_schedule`
- `"queued"` added to `create_scheduled_post` / `update_scheduled_post` status enum


### Fixed

- `authURL`: encode OAuth scope with `%20` instead of `+` to satisfy Mastodon's authorization endpoint (`url.Values.Encode` uses `+` for spaces; Mastodon rejects it with "scope is invalid")

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

# forge-social Changelog

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

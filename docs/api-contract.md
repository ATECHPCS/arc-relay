# Arc Relay HTTP API Contract

This is the authoritative reference for every externally-visible endpoint that Arc Relay exposes. It is kept in lockstep with `internal/server/http_contract_test.go` - if an endpoint is added, removed, or changes auth requirements, update this document and the contract test together.

The test in `internal/server/http_contract_test.go` fails when:

- a documented route is not registered (returns 404 for any verb)
- an auth-required route accepts unauthenticated callers (returns 200)

Four auth schemes are in play:

| Scheme | How callers authenticate | Used by |
|--------|-------------------------|---------|
| `none` | no credentials required | health, OAuth discovery, login, install script, device/invite bootstrap |
| `api_key` | `Authorization: Bearer <key>` (raw API key from `/api-keys`) | management API (`/api/servers`) |
| `mcp` | `Authorization: Bearer <key-or-oauth-token>` (API key OR OAuth access token) | `/mcp/{server}` proxy |
| `session` | `session` cookie set by `/login` | web UI pages and UI-backing JSON APIs |

Session-authenticated POST/PUT/DELETE also require a matching CSRF token (form field `csrf_token` or `X-CSRF-Token` header). The token is HMAC-derived from the session ID; see `Handlers.csrfToken`.

---

## MCP proxy

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| POST | `/mcp/{server-name}` | `mcp` | JSON-RPC dispatch to a configured server. Accepts API keys or OAuth access tokens. Notifications (no `id`) return `202 Accepted`. Responses are filtered by the caller's profile permissions. |

---

## Health

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/health` | `none` | Liveness probe. Returns `{"status":"ok"}`. |

---

## Management API

All endpoints under `/api/servers` require `api_key` auth. Non-admin callers see only servers their profile grants access to. Management actions (start/stop/enumerate/optimize) require admin access level.

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/api/servers` | `api_key` | List servers visible to the caller. |
| POST | `/api/servers` | `api_key` (admin) | Create a server. |
| GET | `/api/servers/{id}` | `api_key` | Get one server. |
| PUT | `/api/servers/{id}` | `api_key` (admin) | Update a server. |
| DELETE | `/api/servers/{id}` | `api_key` (admin) | Delete a server and stop its backend. |
| POST | `/api/servers/{id}/start` | `api_key` (admin) | Start a managed server. |
| POST | `/api/servers/{id}/stop` | `api_key` (admin) | Stop a managed server. |
| POST | `/api/servers/{id}/enumerate` | `api_key` (admin) | Re-enumerate tools/resources/prompts. |
| GET | `/api/servers/{id}/endpoints` | `api_key` | Return cached tools/resources/prompts. |
| POST | `/api/servers/{id}/health` | `api_key` | On-demand health probe. |
| GET | `/api/servers/{id}/tool-audit` | `api_key` | Tool size audit + optimization status. |
| POST | `/api/servers/{id}/optimize` | `api_key` (admin) | Run LLM tool-description optimization. Requires `ARC_RELAY_LLM_API_KEY`. |
| POST | `/api/servers/{id}/optimize-toggle` | `api_key` (admin) | Enable or disable serving optimized tools. |

401 responses include the `WWW-Authenticate: Bearer resource_metadata=".../.well-known/oauth-protected-resource{path}"` header (RFC 9728).

---

## OAuth 2.1 Authorization Server

Arc Relay is a full OAuth 2.1 Authorization Server so Claude Desktop and other MCP clients can authenticate end users.

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/.well-known/oauth-protected-resource[/...]` | `none` | RFC 9728 protected-resource metadata. The optional sub-path produces per-resource metadata. |
| GET | `/.well-known/oauth-authorization-server` | `none` | RFC 8414 authorization-server metadata (endpoints, PKCE support, grants). |
| POST | `/oauth/register` (alias `/register`) | `none` | RFC 7591 dynamic client registration. PKCE with S256 is required for public clients. |
| POST | `/oauth/token` (alias `/token`) | `none` | `authorization_code` (PKCE required) or `refresh_token` grant. Refresh tokens rotate on use. |
| GET, POST | `/oauth/authorize` (alias `/authorize`) | `session` | Consent screen. GET renders the prompt; POST records the approval and redirects back to the client. |

Auth codes are ephemeral (5 min TTL, in-memory). Access tokens expire in 1 hour; refresh tokens in 30 days.

---

## CLI onboarding

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| POST | `/api/auth/device` | `none` | Device-auth start (RFC 8628-style). Returns `device_code`, `user_code`, `verification_url`, `expires_in`, `interval`. |
| POST | `/api/auth/device/token` | `none` | Polling endpoint. Returns `{"error":"authorization_pending"}` while pending, `{"api_key":"..."}` when approved, or `{"error":"access_denied"}`. |
| GET, POST | `/auth/device` | `session` | Browser approval page. GET with `?code=<user_code>` shows approve/deny; POST with `action=approve\|deny` completes the flow and mints an API key inheriting the user's default profile. |
| POST | `/api/auth/invite` | `none` (invite token is proof) | Exchange a one-time invite token plus username/password for a new account and an API key. |
| GET, POST | `/invite/{token}` | `none` (invite token is proof) | Browser account setup: GET shows the form, POST creates the user, redeems the token, and signs them in. |

---

## Upstream OAuth (for remote MCP servers)

Used when a remote MCP server (e.g. Sentry, Shortcut) itself speaks OAuth - Arc Relay is the OAuth client.

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/oauth/start/{server_id}` | `session` (admin) | Begin the upstream OAuth flow, redirecting the admin to the provider's authorize URL. `?force=1` forces dynamic client re-registration. |
| GET | `/oauth/callback` | `none` | Upstream redirect target. Validates state, exchanges code, stores tokens, then redirects to `/servers/{id}`. |

---

## Web UI

All web UI pages require a `session` cookie. Non-admins are restricted by profile permissions.

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/` | `session` | Dashboard. Non-HTML clients (no `Accept: text/html`) get 401 with OAuth discovery header. |
| GET | `/servers` | `session` | Redirects to `/`. |
| GET, POST | `/servers/new` | `session` (admin) | New server form. |
| GET | `/servers/{id}` | `session` | Server detail. Non-admins see only permitted endpoints. |
| GET, POST | `/servers/{id}/edit` | `session` (admin) | Edit server. |
| POST | `/servers/{id}/{action}` | `session` (admin) | Actions: `start`, `stop`, `delete`, `enumerate`, `rebuild`, `rebuild-restart`, `recreate`, `recreate-stream`, `access-tier`, `middleware`, `health-check`, `optimize`, `optimize-toggle`, `tool-audit`. |
| GET | `/logs` | `session` (admin) | Request logs with filtering/pagination. |
| GET, POST | `/users` | `session` (admin) | Users admin. |
| POST | `/users/new` | `session` (admin) | Create user. |
| POST | `/users/invite-new` | `session` (admin) | Create an account-template invite token. |
| POST | `/users/invite-revoke/{id}` | `session` (admin) | Revoke a pending invite. |
| POST | `/users/{id}/delete`, `/users/{id}/update`, `/users/{id}/reset-password` | `session` (admin) | Per-user actions. |
| GET | `/api-keys` | `session` | List the caller's own API keys. |
| POST | `/api-keys/new` | `session` | Create a new API key (bound to the caller's profile). |
| POST | `/api-keys/{id}/revoke` | `session` | Revoke a key owned by the caller (admins can revoke any). |
| GET, POST | `/profiles` | `session` (admin) | List/create agent profiles. |
| GET, POST | `/profiles/{id}` | `session` (admin) | Profile detail. |
| POST | `/profiles/{id}/{action}` | `session` (admin) | Actions: `update`, `delete`, `permission`, `seed`. |
| GET, POST | `/account/password` | `session` | Self-service password change. Forced after admin resets. |
| GET | `/connect/desktop` | `session` | Claude Desktop onboarding view. |
| GET, POST | `/login` | `none` | Login form (rate-limited: 5 attempts / 15 min per IP). |
| GET | `/logout` | `none` | Clears session cookie and redirects to `/login`. |

---

## UI-backing JSON APIs

Session-authenticated endpoints consumed by the web UI's fetch() calls. CSRF token required for state-changing verbs.

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/api/catalog/search?q=...` | `session` | MCP registry search. Fails soft - returns `[]` on upstream error. |
| POST | `/api/catalog/discover-oauth` | `session` (admin) | Probe a remote URL for OAuth discovery metadata + DCR. Blocks private IP targets (SSRF guard). |
| POST | `/api/middleware/{name}/config?target=global` | `session` (admin) | Save global middleware config (currently `archive`). |
| POST | `/api/middleware/{name}/action/{action}` | `session` (admin) | Invoke a middleware action. Archive supports `test`, `retry`, `clear`, `status`, plus stateful `handoff_begin` / `handoff_complete`. |

Per-server middleware config uses `POST /servers/{id}/middleware` (see web UI table above).

---

## CLI distribution

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/install.sh` | `none` | Templated bash installer for `arc-sync`. Detects OS/arch, downloads from `/download/`, optionally runs `arc-sync init` with `--token`, `--username`, `--password` passthrough. |
| GET | `/download/{binary}` | `none` | Serves the `arc-sync` binary for supported OS/arch combos. Binary name is checked against an allowlist (`arc-sync-{linux,darwin,windows}-{amd64,arm64}[.exe]`). Falls back to the GitHub releases redirect if the file is not present locally. |

---

## Notes for maintainers

- Every new HTTP route must also appear in this table and in `contract` inside `internal/server/http_contract_test.go`.
- Prefer `authSession` over `authNone` for anything that exposes data or triggers work.
- When returning 401 from a Bearer-protected route, always set `WWW-Authenticate` via `setWWWAuthenticate` so MCP clients can discover OAuth.
- The aliases `/authorize`, `/token`, `/register` exist because some clients POST to them directly instead of following `/.well-known/oauth-authorization-server`. Keep both registered.

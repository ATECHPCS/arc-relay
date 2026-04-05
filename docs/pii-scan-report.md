# PII Exposure Scan Report

**Repository:** mcp-wrangler (Arc Relay)
**Date:** 2026-04-05
**Scanner:** nightshift/pii-scanner

---

## Summary

| Category            | Critical | High | Medium | Low | Total |
|---------------------|----------|------|--------|-----|-------|
| hardcoded-pii       | 0        | 0    | 0      | 2   | 2     |
| pii-in-logs         | 2        | 2    | 2      | 0   | 6     |
| env-secret          | 0        | 0    | 2      | 1   | 3     |
| unencrypted-storage | 1        | 1    | 0      | 0   | 2     |
| gitignore-gap       | 0        | 1    | 1      | 0   | 2     |
| **Total**           | **3**    | **4**| **5**  | **3**| **15**|

---

## Findings

### Category: pii-in-logs

#### PII-LOG-1: Admin password logged to stdout

- **file:** cmd/arc-relay/main.go
- **line:** 76
- **category:** pii-in-logs
- **severity:** critical
- **detail:** Generated random admin password is logged in plaintext via `log.Printf("Generated random admin password: %s", adminPw)`. Anyone with access to container logs, log aggregation systems, or CI output can read the credential.
- **recommendation:** Remove the password from log output. If the operator needs it, write it to a file with restricted permissions (0600) or require it to be set via env var before startup. Never log credentials.

#### PII-LOG-2: OAuth token response body in error messages

- **file:** internal/oauth/oauth.go
- **line:** 510, 524
- **category:** pii-in-logs
- **severity:** critical
- **detail:** Full HTTP response bodies from OAuth token endpoints are included in `fmt.Errorf()` return values: `"token endpoint returned %d: %s"` and `"no access_token in response: %s"`. Token endpoint responses typically contain `access_token` and `refresh_token` fields. These errors may propagate to log output or error tracking (Sentry).
- **recommendation:** Extract only the `error` and `error_description` fields from error responses. Never include the full body in error messages when it may contain tokens.

#### PII-LOG-3: Username logged across auth/authz events

- **file:** internal/web/device_auth.go
- **line:** 336, 346
- **file:** internal/web/handlers.go
- **line:** 1528, 1797
- **file:** internal/web/oauth_provider.go
- **line:** 454, 561
- **file:** internal/server/http.go
- **line:** 374, 384, 461
- **category:** pii-in-logs
- **severity:** high
- **detail:** `user.Username` is interpolated into log statements for device auth approvals, invite exchanges, password resets, OAuth authorizations, and access denials. Usernames are PII and create behavioral tracking risk when logged alongside actions and timestamps.
- **recommendation:** Log user IDs instead of usernames. If human-readable identification is needed for debugging, use a short correlation ID or truncated hash.

#### PII-LOG-4: HTTP response body in CLI relay errors

- **file:** internal/cli/relay/client.go
- **line:** 70
- **category:** pii-in-logs
- **severity:** high
- **detail:** Full relay API error responses are embedded in error messages: `"relay returned HTTP %d: %s"`. Response bodies could contain server metadata or error details that expose internal state.
- **recommendation:** Parse the response body for a structured error message field; do not include the raw body.

#### PII-LOG-5: Auth validation failures log remote address

- **file:** internal/server/middleware.go
- **line:** 60, 100, 109
- **category:** pii-in-logs
- **severity:** medium
- **detail:** API key and OAuth token validation failures are logged with `r.URL.Path` and `r.RemoteAddr`. While not directly logging the token, the remote IP combined with validation errors could help attackers identify targets.
- **recommendation:** Rely on access logs for IP tracking. Remove `r.RemoteAddr` from auth validation log lines.

#### PII-LOG-6: Access denial logs include username and resource

- **file:** internal/server/http.go
- **line:** 374, 384
- **category:** pii-in-logs
- **severity:** medium
- **detail:** Access denied log lines include `user.Username`, profile ID, access level, and the resource attempted. This creates a detailed record of failed authorization attempts tied to usernames.
- **recommendation:** Log user ID instead of username. Keep resource and tier info for audit purposes but decouple from PII.

---

### Category: env-secret

#### ENV-1: docker-compose.yml has insecure fallback defaults

- **file:** docker-compose.yml
- **line:** 16-18
- **category:** env-secret
- **severity:** medium
- **detail:** Environment variables use `${VAR:-changeme}` syntax, meaning if the env var is unset, the service starts with known weak credentials. The file includes a warning comment, but the fallback values still allow accidental deployment with weak secrets.
- **recommendation:** Use `${VAR:?error message}` syntax to require explicit values, or remove fallback defaults entirely. This forces operators to set credentials before starting.

#### ENV-2: config.example.toml and .env.example use placeholder credentials

- **file:** config.example.toml
- **line:** 16, 20, 22
- **file:** .env.example
- **line:** 5, 8, 11
- **category:** env-secret
- **severity:** medium
- **detail:** Example files contain `changeme` and `change-me-generate-a-real-key` placeholders. While appropriate for examples, operators may copy these verbatim. No startup validation rejects obviously weak values.
- **recommendation:** Add startup validation that rejects known placeholder values (changeme, change-me-*) in production mode. The example files themselves are fine.

#### ENV-3: Test fixtures contain hardcoded bearer tokens

- **file:** internal/cli/testdata/mcp-json/existing-relay-servers.json
- **line:** 7, 14
- **file:** internal/cli/testdata/mcp-json/mixed.json
- **line:** 7
- **category:** env-secret
- **severity:** low
- **detail:** Test fixture files contain `"Bearer test-key"` tokens. These are clearly test values but are committed to the repository. Pattern is also present across Go test files.
- **recommendation:** Acceptable for test-only scope. Ensure test credentials are obviously fake and never match production patterns. No action required.

---

### Category: unencrypted-storage

#### STORE-1: Archive queue stores auth credentials in plaintext

- **file:** migrations/012_archive_queue.sql
- **line:** 8-9
- **file:** internal/middleware/archive_dispatcher.go
- **line:** 103, 128
- **category:** unencrypted-storage
- **severity:** critical
- **detail:** The `archive_queue` table has an `auth_value` TEXT column that stores bearer tokens and API keys in plaintext. These credentials are enqueued as-is from `ArchiveConfig.AuthValue` and persist in the database until successfully delivered (or indefinitely on repeated failures).
- **recommendation:** Encrypt `auth_value` using the existing `ConfigEncryptor` before storing in the queue. Decrypt only at delivery time. Alternatively, store a reference to a secret and resolve it at send time.

#### STORE-2: Server config encryption is optional (disabled without key)

- **file:** internal/store/crypto.go
- **line:** 24-26, 43-45
- **file:** internal/config/config.go
- **line:** 34-36
- **category:** unencrypted-storage
- **severity:** high
- **detail:** `ConfigEncryptor` passes through plaintext when no encryption key is configured. Server configs containing OAuth client secrets, bearer tokens, and refresh tokens are stored unencrypted in the `config` column of the `servers` table. The system starts without warning when encryption is disabled.
- **recommendation:** Log a warning at startup when no encryption key is configured. Consider requiring an encryption key for production deployments. Auto-generate and persist a key on first run if none is provided.

---

### Category: hardcoded-pii

#### PII-1: Test fixture IP addresses

- **file:** internal/cli/testdata/mcp-json/existing-relay-servers.json
- **line:** 5, 12
- **file:** internal/cli/relay/client_test.go
- **line:** 111, 120, 129, 135
- **category:** hardcoded-pii
- **severity:** low
- **detail:** Test data uses IP address `10.10.69.50` consistently across fixtures and test files. While this is a private RFC 1918 address and clearly test data, it could reflect real internal infrastructure.
- **recommendation:** Consider using `127.0.0.1` or `example.com` in test fixtures. No urgent action needed.

#### PII-2: Test fixture server names resemble real services

- **file:** internal/cli/testdata/wrangler-responses/five-servers.json
- **line:** 1-5
- **category:** hardcoded-pii
- **severity:** low
- **detail:** Test fixture uses real service names (Sentry, pfSense, Home Assistant, Shortcut) that match actual deployed infrastructure documented in MEMORY.md. No PII is exposed, but fixtures mirror real topology.
- **recommendation:** Acceptable - no PII content. Service names are not sensitive.

---

### Category: gitignore-gap

#### GIT-1: Missing patterns for certificates, keys, and credential files

- **file:** .gitignore
- **category:** gitignore-gap
- **severity:** high
- **detail:** The .gitignore file did not cover `*.pem`, `*.key`, `*.p12`, `*.pfx`, `credentials.*`, or `secrets.*`. Any of these files accidentally created in the repo root would be staged for commit.
- **recommendation:** Added patterns for `*.pem`, `*.key`, `*.p12`, `*.pfx`, `credentials.*`, `secrets.*`. **FIXED in this PR.**

#### GIT-2: Missing .env wildcard and cross-compiled binary patterns

- **file:** .gitignore
- **category:** gitignore-gap
- **severity:** medium
- **detail:** Only `.env` was ignored, not `.env.*` variants (`.env.local`, `.env.production`). Cross-compiled binaries like `arc-sync-darwin-arm64` (seen in git status) were not covered by any pattern.
- **recommendation:** Added `.env.*` wildcard and `arc-sync-*`/`arc-relay-*` patterns. **FIXED in this PR.**

---

## Positive Findings (No Issues)

The following security practices are correctly implemented:

- **Passwords:** Hashed with bcrypt before storage (`internal/store/users.go`)
- **API keys:** SHA256 hashed before storage with constant-time comparison (`internal/store/users.go`)
- **OAuth tokens:** SHA256 hashed before storage (`internal/store/oauth_tokens.go`)
- **Invite tokens:** SHA256 hashed before storage (`internal/store/invites.go`)
- **OAuth client secrets:** SHA256 hashed before storage (`internal/store/oauth_clients.go`)
- **CSRF protection:** HMAC-based tokens validated on all state-changing requests
- **Session cookies:** HttpOnly, SameSite=Lax, conditional Secure flag
- **CI/CD secrets:** All GitHub Actions workflows use `${{ secrets.* }}` - no hardcoded tokens
- **Sensitive files ignored:** `.env`, `config.toml`, `.mcp.json`, `*.db` all in `.gitignore`
- **CLI config permissions:** Validated to 0600 (`internal/cli/config/config.go`)
- **SSRF protection:** `validateExternalURL()` blocks private/loopback addresses
- **Rate limiting:** Login endpoint rate-limited per IP

---

## Remediation Priority

1. **Immediate:** Remove admin password from log output (PII-LOG-1)
2. **Immediate:** Sanitize OAuth token error messages (PII-LOG-2)
3. **High:** Encrypt archive queue auth_value column (STORE-1)
4. **High:** Warn when config encryption is disabled (STORE-2)
5. **Medium:** Replace username with user ID in log statements (PII-LOG-3, PII-LOG-6)
6. **Medium:** Require explicit env vars in docker-compose (ENV-1)
7. **Low:** Remaining findings are informational or test-only scope

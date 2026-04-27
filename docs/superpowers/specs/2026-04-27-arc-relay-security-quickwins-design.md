# Arc Relay Security Quick-Wins — Design Spec

**Date:** 2026-04-27
**Scope:** Three findings from the [security posture audit](../../../) that close DoS surfaces and tighten file/transport defaults without architectural changes.
**Source:** Findings #2 (rate limiting), #3 (DB file mode), #5 (public base URL) from the post-Phase-2 security audit.

---

## 1. Problem

The post-Phase-2 audit found:

1. **Four public auth-init endpoints have no rate limiting** — `/oauth/register`, `/api/auth/device`, `/api/auth/device/token`, `/api/auth/invite`. An attacker can flood any of them. `/oauth/register` is the worst because it writes a fresh `oauth_clients` row on every call (DB exhaustion). The others fill in-memory state.
2. **DB files are world-readable on the host** — mode `0644` instead of `0600`. On the current single-tenant Proxmox container this is mostly cosmetic, but as defense-in-depth (LXC user added later, snapshot copies, etc.) it should be `0600`.
3. **`ARC_RELAY_BASE_URL` env var isn't propagating** — `WWW-Authenticate` headers and the device-auth `verification_url` both leak `localhost:8080`. The device-auth flow is broken externally because of this. Cloudflare Access bypass paths also expose this internal URL via discovery hints.

None expose memory data publicly. None require schema or API changes. All three are low-risk mechanical fixes.

## 2. Goal

Close the three findings with the **smallest possible diff**:

- Add per-IP rate limiting to the four public auth-init endpoints, reusing the existing `loginRateLimiter` pattern.
- Apply `os.Chmod(0o600)` to DB files at open time in `internal/store/db.go` AND fix the existing files in the production volume during deploy.
- Set `ARC_RELAY_BASE_URL=https://arc-relay.andersontechsolutions.com` in `/etc/komodo/secrets/mcp-gateway/mcp-gateway.env` so the relay's `cfg.PublicBaseURL()` returns the correct external URL.

### Success criteria

1. Rapid POSTing to any of the four endpoints from the same IP triggers HTTP 429 (or equivalent) after the configured limit
2. `ls -la /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/` shows mode `-rw-------` for all `*.db` and `*.db-wal`/`*.db-shm` files
3. `curl -I https://arc-relay.andersontechsolutions.com/api/memory/stats` (no auth) returns `WWW-Authenticate: Bearer resource_metadata="https://arc-relay.andersontechsolutions.com/.well-known/oauth-protected-resource/api/memory/stats"` (the public URL, NOT `localhost:8080`)
4. Device-auth POST (`POST /api/auth/device`) returns a `verification_url` pointing at the public hostname

### Non-goals

- **Don't change the container runtime user** (Finding #1 — defer; user explicitly chose "quick wins only")
- **Don't equalize login bcrypt timing** (Finding #4 — defer)
- **Don't make `/api/memory/stats` user-scoped** (Finding #6 — design choice, defer)
- **Don't add CSRF to device-auth/invite** (orthogonal; not in this scope)
- **Don't introduce new rate-limit framework** — extend the existing `loginRateLimiter` pattern. No new dependencies.

## 3. Design

### 3.1 Rate limiting

The existing `loginRateLimiter` (`internal/web/handlers.go` near line 2700-2800) tracks failed-login attempts per IP with a 15-minute window and a 5-attempt threshold. It exposes:

```go
func (l *loginRateLimiter) allow(ip string) bool
func (l *loginRateLimiter) recordFailure(ip string)
func (l *loginRateLimiter) recordSuccess(ip string)
```

**For this fix**, generalize to a parameterized limiter. Two options considered:

**A. Reuse `loginRateLimiter` directly with new instances per endpoint** — copy-paste the struct, parameterize `(window time.Duration, max int)`. Each endpoint gets its own `*loginRateLimiter` instance with its own threshold.

**B. Build a more general `IPRateLimiter` struct** — same shape but explicitly named, single shared instance with per-endpoint key prefix.

**Decision: Option A.** Simpler, smaller diff, no new abstraction. The four endpoints get four separate limiter instances on the `Handlers` struct.

**Per-endpoint limits (proposed defaults):**

| Endpoint | Window | Max requests/IP | Reasoning |
|---|---|---|---|
| `/oauth/register` | 1 hour | 10 | DCR is rare; legitimate clients register once |
| `/api/auth/device` | 15 min | 20 | Device-auth init is rare; 20/IP/15min is generous |
| `/api/auth/device/token` | 1 min | 60 | Polling endpoint; 1Hz is the typical client rate |
| `/api/auth/invite` | 15 min | 10 | Account-creation; rare per IP |

When the limit is hit, return HTTP 429 (Too Many Requests) with `Retry-After` header. JSON error body for the `/api/*` paths; plain text for `/oauth/register` (RFC 7591 doesn't dictate format).

**Important: count attempts, not just failures.** The login limiter only counts failures (legitimate users with correct passwords don't deplete the budget). For these endpoints, count every request — there's no "success retry" pattern.

**File layout:**
- New struct + helper added to `internal/web/handlers.go` near the existing `loginRateLimiter`
- Four new fields on `Handlers` struct: `oauthRegisterLimiter`, `deviceStartLimiter`, `deviceTokenLimiter`, `inviteLimiter`
- Initialized in `NewHandlers`
- Each handler method (`handleOAuthRegister`, `handleDeviceAuthStart`, `handleDeviceAuthToken`, `handleInviteExchange`) gets a one-line check at the top: `if !h.<limiter>.allow(clientIP(r)) { rateLimitResponse(w); return }`

### 3.2 DB file mode 0600

Two parts:

**A. Code-level fix** — in `internal/store/db.go`, after `Open()` succeeds, call `os.Chmod(path, 0o600)`. This sets the mode on every relay startup. Idempotent — already-correct files are unchanged. Skip for `:memory:` paths (no file).

```go
// After db.Ping() and integrity_check pass:
if path != ":memory:" && path != "" {
    if err := os.Chmod(path, 0o600); err != nil {
        slog.Warn("could not set db mode 0600", "path", path, "err", err)
        // Don't fail boot — degraded but functional
    }
}
```

The mode setting is best-effort. If it fails (e.g., a read-only mount), the relay still works — just doesn't have the hardened mode.

**B. Production fix** — one-shot `chmod 600` on the existing files during deploy:

```bash
ssh DockerKomodo 'chmod 600 /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/*.db /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/*.db-* 2>/dev/null'
```

The code fix handles future re-creates (e.g., after a clean redeploy). The one-shot handles the existing files.

**Why not also chmod the WAL/shm files?** SQLite creates `.db-wal` and `.db-shm` alongside the main file with whatever umask the process has. Setting `os.Chmod` on the main `.db` file doesn't propagate. Best practice: set the umask before opening, OR chmod after. Going with **chmod the main file at open time** (the spec says only `*.db` matters for the audit finding); WAL files inherit umask and are short-lived. If the audit ever scopes WAL specifically, we'd need a process umask change.

Looking at the existing files:
```
-rw-r--r-- 1 root root  475136 arc-relay.db
-rw-r--r-- 1 root root   32768 arc-relay.db-shm
-rw-r--r-- 1 root root 1837552 arc-relay.db-wal
-rw-r--r-- 1 root root 60948480 memory.db
-rw-r--r-- 1 root root   32768 memory.db-shm
-rw-r--r-- 1 root root  6925752 memory.db-wal
```

All 6 files are 0644. The one-shot deploy step chmods all of them. The code-level chmod hits the main `.db` files at every relay start; WAL/shm get hit too via the production one-shot until the next clean recreate (where they'll come back at default umask).

**Decision: also set process umask in main.go to 0o077.** Tiny change, ensures all subsequent file operations create files with mode `0700` for dirs and `0600` for files.

### 3.3 Public base URL

Currently `ARC_RELAY_BASE_URL` is empty inside the container (verified by external probe — `WWW-Authenticate` references `localhost:8080`). The compose file says `ARC_RELAY_BASE_URL: ${ARC_RELAY_BASE_URL}`, which substitutes from the shell env when `docker compose up` runs. That shell env doesn't have it, so the substitution becomes empty.

**Fix:** Add to `/etc/komodo/secrets/mcp-gateway/mcp-gateway.env`:

```
ARC_RELAY_BASE_URL=https://arc-relay.andersontechsolutions.com
```

This file is already an `env_file` in the compose, so the value is picked up directly without shell-var substitution.

**Verification:** After redeploy, `curl -sI https://arc-relay.andersontechsolutions.com/api/memory/stats` should return `WWW-Authenticate` with the public URL, and `curl -X POST .../api/auth/device` should return a `verification_url` pointing at `https://arc-relay.andersontechsolutions.com/auth/device?code=...`.

This change is **production-only** — no code change. Add to deploy steps, not a code commit.

## 4. Files

| File | Change | Phase |
|---|---|---|
| `internal/web/handlers.go` | Generalize `loginRateLimiter` (or copy-pattern); add 4 new limiter fields to `Handlers`; add 4 one-line guards in handlers; add `rateLimitResponse` helper | Code |
| `internal/store/db.go` | `os.Chmod(path, 0o600)` after `Open()` | Code |
| `cmd/arc-relay/main.go` | `syscall.Umask(0o077)` very early in `main()` | Code |
| `internal/web/handlers_test.go` (or new test file) | Test the rate limiter triggers 429 after threshold | Code |
| `internal/store/db_test.go` | Test that `Open` sets mode 0600 on file-backed DBs | Code |
| `/etc/komodo/secrets/mcp-gateway/mcp-gateway.env` | Add `ARC_RELAY_BASE_URL=https://arc-relay.andersontechsolutions.com` | Production-only |
| Existing volume DB files | `chmod 600 /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/*.db*` | Production-only |

## 5. Tests

**Rate limiter tests** (`internal/web/handlers_test.go` — extend existing or add):

- `TestRateLimiter_AllowsBelowThreshold` — 9 requests in a window pass
- `TestRateLimiter_BlocksAtThreshold` — 11th request returns 429 with `Retry-After`
- `TestRateLimiter_PerIPIsolation` — IP A's exhaustion doesn't affect IP B
- `TestRateLimiter_WindowExpiry` — after window passes, fresh budget

**DB chmod test** (`internal/store/db_test.go`):

- `TestOpen_SetsFileMode0600` — open a tempfile-backed DB, stat the file, assert mode `0600`
- `TestOpen_SkipsChmodForMemoryDB` — `:memory:` path doesn't error or call Chmod

**Manual production verification** (post-deploy):

- `curl -sI https://arc-relay.andersontechsolutions.com/api/memory/stats | grep WWW-Authenticate` shows public URL
- `curl -X POST https://arc-relay.andersontechsolutions.com/api/auth/device -d '{}' | jq .verification_url` shows public URL
- `ssh DockerKomodo 'ls -la /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/*.db'` shows `-rw-------`
- Rapid POSTing to `/oauth/register` from one IP returns 429 after 10 attempts within an hour

## 6. Phasing

Single phase. Three changes ship together in one deploy because they're all small and the deploy machinery (rebuild image + redeploy container + chmod existing files + add env var) is cleaner as one operation.

Estimated implementation work: ~1 hour of agent time. Deploy: ~5 minutes (build + recreate + chmod).

## 7. Risks

| Risk | Mitigation |
|---|---|
| Rate-limit thresholds too low → break legitimate clients | Default values are generous; tune via `loginRateLimiter` pattern (in-memory, no config file). If a real client hits 429, lower the limit takes a code change + redeploy. Acceptable for v1. |
| `os.Chmod` fails on some platform → boot regression | Wrapped in `slog.Warn`, doesn't fail boot. Worst case: file mode stays at default umask. |
| Setting umask 0o077 breaks something else (e.g., backup file mode) | Backups via `VACUUM INTO` create files in same dir; 0600 is appropriate for them too. The relay process never creates files outside the data dir during normal operation. Low risk. |
| `ARC_RELAY_BASE_URL` typo → broken WWW-Authenticate URLs | Manual verification step in production deploy explicitly checks the URL is correct. |
| Adding rate limit to `/api/auth/device/token` breaks polling | 60 polls/min is a 1Hz cap; legitimate clients poll at ~5s intervals (12/min). Comfortable headroom. |
| The four limiters add memory pressure | Each limiter holds a per-IP map. Cleanup goroutine (mirroring `loginRateLimiter`) prunes expired entries. Bounded by IP cardinality which is bounded by the public exposure (CF Access in front of dashboard, but `/api/auth/device` etc. are public). Worst case: ~10K unique IPs × 4 limiters × ~100 bytes = 4MB. Acceptable. |

## 8. Acceptance

This spec is approved when:
- Rate-limit thresholds in §3.1 are confirmed (or adjusted) by user
- The deploy steps in §4 are agreed
- Implementation plan written from this spec is reviewed before subagent dispatch

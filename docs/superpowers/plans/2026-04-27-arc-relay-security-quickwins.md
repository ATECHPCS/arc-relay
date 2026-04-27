# Arc Relay Security Quick-Wins Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers-extended-cc:subagent-driven-development (recommended) or superpowers-extended-cc:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close three findings from the post-Phase-2 security audit: rate limit four public auth-init endpoints, set DB file mode to 0600, and fix the public-base-URL leak in `WWW-Authenticate` and device-auth verification URLs.

**Architecture:** Two code tasks plus a deploy step. Task 0 generalizes the existing `loginRateLimiter` pattern (per-IP, sliding window, in-memory) and applies guards to four handler functions. Task 1 sets `os.Chmod(0o600)` after `store.Open` and `syscall.Umask(0o077)` at the start of `main()`. The third fix (`ARC_RELAY_BASE_URL` env var) is production-only — no code change, just an edit to `/etc/komodo/secrets/mcp-gateway/mcp-gateway.env` plus a `chmod 600` on existing volume files during deploy.

**Tech Stack:** Go 1.24, no new dependencies. Reuses the existing `loginRateLimiter` shape from `internal/web/handlers.go:86-130`.

**Spec:** [`docs/superpowers/specs/2026-04-27-arc-relay-security-quickwins-design.md`](../specs/2026-04-27-arc-relay-security-quickwins-design.md)

---

## File structure

| File | Change | Owner |
|---|---|---|
| `internal/web/handlers.go` | Generalize `loginRateLimiter` → `ipRateLimiter`; add 4 new limiter fields to `Handlers`; add guards in 4 handlers; add `rateLimitResponse` helper | Task 0 |
| `internal/web/device_auth.go` | Add limiter guard at top of `handleDeviceAuthStart` and `handleDeviceAuthToken` | Task 0 |
| `internal/web/oauth_provider.go` | Add limiter guard at top of `handleOAuthRegister` | Task 0 |
| `internal/web/handlers_test.go` | New tests for the generalized limiter (under-threshold, at-threshold, per-IP isolation, window expiry) | Task 0 |
| `internal/store/db.go` | `os.Chmod(path, 0o600)` after integrity check | Task 1 |
| `internal/store/db_test.go` | Test that `Open` sets file mode 0600; skips `:memory:` | Task 1 |
| `cmd/arc-relay/main.go` | `syscall.Umask(0o077)` very early in `main()` | Task 1 |
| `/etc/komodo/secrets/mcp-gateway/mcp-gateway.env` (production) | Add `ARC_RELAY_BASE_URL=https://arc-relay.andersontechsolutions.com` | Deploy |
| `/var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/*.db*` (production) | `chmod 600` on existing files | Deploy |

---

## Pre-Flight constraints

- **Reuse the existing limiter pattern.** `loginRateLimiter` in `internal/web/handlers.go:86-130` is the canonical shape: per-IP map of timestamps, sliding window, mutex-protected, cleanup goroutine. Generalize it to `ipRateLimiter` with parameterized `(window, max)`. The existing `loginLimiter` field/usage stays — just point it at the new struct, or keep the type name as `loginRateLimiter` and add a new `ipRateLimiter` alongside. Pick the cleaner option as you read the code.
- **Don't introduce new dependencies.** Standard library only.
- **Rate-limit response** = HTTP 429 with `Retry-After` header (in seconds, conservative — use the window length). For `/api/*` paths return JSON body `{"error":"rate limit exceeded"}`. For `/oauth/register` return plain text per RFC 7591 (its existing error responses are JSON, so JSON is fine).
- **Tests must use deterministic time.** Don't `time.Sleep` — inject a clock if needed, or assert via the `len(filtered)` count after `recordN` calls. The existing `loginRateLimiter` tests (look in `internal/web/csrf_test.go` neighborhood for any) may show a pattern.
- **Don't break existing tests.** `loginRateLimiter` is exercised indirectly via login tests. After generalizing, `loginLimiter` should behave identically (5 attempts / 15 min).
- **`syscall.Umask` is Unix-only.** macOS/Linux relay; works there. The existing main.go imports `syscall` already so no new import.

---

### Task 0: Generic IP rate limiter + apply to 4 endpoints

**Goal:** Generalize the existing per-IP failed-login limiter into a parameterized `ipRateLimiter` and apply it as the entry guard on `/oauth/register`, `/api/auth/device`, `/api/auth/device/token`, and `/api/auth/invite`.

**Files:**
- Modify: `internal/web/handlers.go` (generalize limiter, add 4 fields, init in NewHandlers, add `rateLimitResponse` helper, apply guard in `handleInviteExchange`)
- Modify: `internal/web/device_auth.go` (apply guard at top of `handleDeviceAuthStart` and `handleDeviceAuthToken`)
- Modify: `internal/web/oauth_provider.go` (apply guard at top of `handleOAuthRegister`)
- Create: `internal/web/ratelimiter_test.go` (new test file for the generic limiter)

**Acceptance Criteria:**
- [ ] `ipRateLimiter` struct exists with `allow(ip)`, `record(ip)`, `cleanup()` methods, parameterized by `(window time.Duration, max int)`
- [ ] Existing `loginLimiter` continues to work identically (5 attempts / 15 min)
- [ ] Four new limiter fields on `Handlers`: `oauthRegisterLimiter` (1 hour / 10), `deviceStartLimiter` (15 min / 20), `deviceTokenLimiter` (1 min / 60), `inviteLimiter` (15 min / 10)
- [ ] All four handlers reject with HTTP 429 + `Retry-After` header when limit exceeded
- [ ] Existing successful login + device + invite flows still work below the threshold
- [ ] Per-IP isolation: IP A's exhaustion doesn't affect IP B (test)
- [ ] Window expiry: budget refreshes after window passes (test, deterministic — no `time.Sleep`)
- [ ] `make test` passes

**Verify:** `cd /Users/ian/code/arc-relay-memory-pivot && CGO_ENABLED=1 go test -tags sqlite_fts5 ./internal/web/ -run "TestIPRateLimiter|TestLoginRateLimiter" -v`

**Steps:**

- [ ] **Step 1: Read the existing pattern**

```bash
cd /Users/ian/code/arc-relay-memory-pivot
sed -n '85,135p' internal/web/handlers.go
```

You'll see `loginRateLimiter` with hardcoded 15-minute window and `< 5` threshold in `allow()`. Two ways to refactor:

A. Rename `loginRateLimiter` → `ipRateLimiter` and add `window`/`max` fields. Update one call site (`newLoginRateLimiter` → `newIPRateLimiter(15*time.Minute, 5)`).

B. Keep `loginRateLimiter` as a type alias / thin wrapper. New struct alongside.

**Pick (A).** Cleaner, smaller diff.

- [ ] **Step 2: Generalize the struct**

Replace lines 86-130 of `internal/web/handlers.go` with the parameterized version:

```go
// ipRateLimiter tracks request timestamps per client IP within a sliding
// window. Used for failed-login throttling AND for the unauth public auth-
// init endpoints. Per-IP map; cleanup goroutine prunes expired entries
// every window/3 to bound memory.
type ipRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	window   time.Duration
	max      int
}

func newIPRateLimiter(window time.Duration, max int) *ipRateLimiter {
	rl := &ipRateLimiter{
		attempts: make(map[string][]time.Time),
		window:   window,
		max:      max,
	}
	go rl.cleanup()
	return rl
}

// allow returns true iff the IP has fewer than `max` recent attempts in the
// past `window`. Trims stale entries as a side effect.
func (rl *ipRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-rl.window)
	recent := rl.attempts[ip]
	filtered := recent[:0]
	for _, t := range recent {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	rl.attempts[ip] = filtered
	return len(filtered) < rl.max
}

// record adds a new attempt at time.Now() for the given IP.
func (rl *ipRateLimiter) record(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.attempts[ip] = append(rl.attempts[ip], time.Now())
}

func (rl *ipRateLimiter) cleanup() {
	// Prune every window/3 (or 5 minutes minimum) so old IPs don't accumulate.
	tick := rl.window / 3
	if tick < 5*time.Minute {
		tick = 5 * time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-rl.window)
		for ip, attempts := range rl.attempts {
			filtered := attempts[:0]
			for _, t := range attempts {
				if t.After(cutoff) {
					filtered = append(filtered, t)
				}
			}
			if len(filtered) == 0 {
				delete(rl.attempts, ip)
			} else {
				rl.attempts[ip] = filtered
			}
		}
		rl.mu.Unlock()
	}
}

// loginRateLimiter is the per-IP failed-login throttle. Kept as a type alias
// so existing call sites compile unchanged.
type loginRateLimiter = ipRateLimiter

func newLoginRateLimiter() *loginRateLimiter {
	return newIPRateLimiter(15*time.Minute, 5)
}
```

The type alias `type loginRateLimiter = ipRateLimiter` and the wrapper `newLoginRateLimiter` keep all existing call sites working unchanged. Verify with a targeted grep:

```bash
grep -n "loginRateLimiter\|loginLimiter" internal/web/*.go
```

Should still compile after this change with no other edits.

- [ ] **Step 3: Add limiter fields + initialization**

In `internal/web/handlers.go`, add to the `Handlers` struct (around line 168 where `loginLimiter` already lives):

```go
oauthRegisterLimiter *ipRateLimiter
deviceStartLimiter   *ipRateLimiter
deviceTokenLimiter   *ipRateLimiter
inviteLimiter        *ipRateLimiter
```

In `NewHandlers`, around line 204 where `loginLimiter` is initialized, add:

```go
oauthRegisterLimiter: newIPRateLimiter(1*time.Hour, 10),
deviceStartLimiter:   newIPRateLimiter(15*time.Minute, 20),
deviceTokenLimiter:   newIPRateLimiter(1*time.Minute, 60),
inviteLimiter:        newIPRateLimiter(15*time.Minute, 10),
```

- [ ] **Step 4: Add `rateLimitResponse` helper**

Append to `internal/web/handlers.go` near the existing `clientIP` helper:

```go
// rateLimitResponse writes HTTP 429 with a Retry-After header (in seconds,
// floor of the limiter's window) and a JSON error body. Used by all four
// public auth-init endpoint guards.
func (h *Handlers) rateLimitResponse(w http.ResponseWriter, retryAfter time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = fmt.Fprint(w, `{"error":"rate limit exceeded"}`)
}
```

If `strconv` isn't imported in handlers.go, add it. (Verify by `grep -n "\"strconv\"" internal/web/handlers.go`.)

- [ ] **Step 5: Apply guard in `handleOAuthRegister` (`internal/web/oauth_provider.go`)**

Find `func (h *Handlers) handleOAuthRegister(w http.ResponseWriter, r *http.Request)` (around line 209). Add at the very top of the function body, before the existing method check:

```go
	if !h.oauthRegisterLimiter.allow(clientIP(r)) {
		h.rateLimitResponse(w, 1*time.Hour)
		return
	}
	h.oauthRegisterLimiter.record(clientIP(r))
```

- [ ] **Step 6: Apply guards in `handleDeviceAuthStart` and `handleDeviceAuthToken` (`internal/web/device_auth.go`)**

Find `func (h *Handlers) handleDeviceAuthStart(...)` around line 177. At the top:

```go
	if !h.deviceStartLimiter.allow(clientIP(r)) {
		h.rateLimitResponse(w, 15*time.Minute)
		return
	}
	h.deviceStartLimiter.record(clientIP(r))
```

Same pattern in `handleDeviceAuthToken` (around line 206) but with `deviceTokenLimiter` and `1*time.Minute`:

```go
	if !h.deviceTokenLimiter.allow(clientIP(r)) {
		h.rateLimitResponse(w, 1*time.Minute)
		return
	}
	h.deviceTokenLimiter.record(clientIP(r))
```

If `time` isn't imported in `device_auth.go` (likely is — it's a Go file dealing with TTLs), add it.

- [ ] **Step 7: Apply guard in `handleInviteExchange` (`internal/web/handlers.go`)**

Find `func (h *Handlers) handleInviteExchange(...)` around line 1742. At the top:

```go
	if !h.inviteLimiter.allow(clientIP(r)) {
		h.rateLimitResponse(w, 15*time.Minute)
		return
	}
	h.inviteLimiter.record(clientIP(r))
```

- [ ] **Step 8: Write tests at `internal/web/ratelimiter_test.go`**

```go
package web

import (
	"testing"
	"time"
)

func TestIPRateLimiter_AllowsBelowThreshold(t *testing.T) {
	rl := newIPRateLimiter(time.Hour, 5)
	for i := 0; i < 4; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("attempt %d should be allowed", i)
		}
		rl.record("1.2.3.4")
	}
	// 5th attempt should still be allowed (allow checks <max, max=5 → 4 records ok)
	if !rl.allow("1.2.3.4") {
		t.Fatal("attempt 5 should be allowed")
	}
}

func TestIPRateLimiter_BlocksAtThreshold(t *testing.T) {
	rl := newIPRateLimiter(time.Hour, 3)
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("attempt %d should be allowed", i)
		}
		rl.record("1.2.3.4")
	}
	// 4th attempt blocked
	if rl.allow("1.2.3.4") {
		t.Fatal("attempt 4 should be blocked (>= max)")
	}
}

func TestIPRateLimiter_PerIPIsolation(t *testing.T) {
	rl := newIPRateLimiter(time.Hour, 2)
	for i := 0; i < 2; i++ {
		_ = rl.allow("1.2.3.4")
		rl.record("1.2.3.4")
	}
	// IP A is now at the threshold
	if rl.allow("1.2.3.4") {
		t.Fatal("IP A should be blocked")
	}
	// IP B is unaffected
	if !rl.allow("5.6.7.8") {
		t.Fatal("IP B should be allowed (not exhausted)")
	}
}

func TestIPRateLimiter_WindowExpiry(t *testing.T) {
	rl := newIPRateLimiter(time.Hour, 2)
	// Manually inject old timestamps to simulate window expiry without sleeping
	old := time.Now().Add(-2 * time.Hour)
	rl.mu.Lock()
	rl.attempts["1.2.3.4"] = []time.Time{old, old}
	rl.mu.Unlock()
	// allow() should prune the stale entries and return true
	if !rl.allow("1.2.3.4") {
		t.Fatal("stale entries should be pruned; new attempt should be allowed")
	}
	// Confirm the prune happened
	rl.mu.Lock()
	got := len(rl.attempts["1.2.3.4"])
	rl.mu.Unlock()
	if got != 0 {
		t.Fatalf("stale entries not pruned: %d remaining", got)
	}
}

func TestLoginRateLimiter_StillWorks(t *testing.T) {
	// Regression: existing wrapper produces a 5/15min limiter
	rl := newLoginRateLimiter()
	for i := 0; i < 5; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("login attempt %d should be allowed", i)
		}
		rl.record("1.2.3.4")
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("6th login attempt should be blocked")
	}
}
```

- [ ] **Step 9: Run tests + verify no regressions**

```bash
cd /Users/ian/code/arc-relay-memory-pivot
CGO_ENABLED=1 go test -tags sqlite_fts5 ./internal/web/ -run "TestIPRateLimiter|TestLoginRateLimiter" -v
make test
make build
```

All must pass. The existing login tests should still work via the type alias.

- [ ] **Step 10: Commit**

```bash
cd /Users/ian/code/arc-relay-memory-pivot
git add internal/web/handlers.go internal/web/device_auth.go internal/web/oauth_provider.go internal/web/ratelimiter_test.go
git commit -m "feat(security): rate limit public auth-init endpoints

Generalizes the existing loginRateLimiter into a parameterized
ipRateLimiter (kept as a type alias so login call sites compile
unchanged). Adds per-IP guards on the four CF-Access-bypassed
public auth-init endpoints flagged in the audit:

  /oauth/register            — 10/hour
  /api/auth/device           — 20/15min
  /api/auth/device/token     — 60/min
  /api/auth/invite           — 10/15min

Each returns HTTP 429 with Retry-After when exhausted. Limits are
generous; legitimate clients won't hit them. Closes audit finding
#2 (DoS surface on public auth-init endpoints — particularly the
DCR endpoint which writes a fresh oauth_clients row per call).

Generated with [Claude Code](https://claude.ai/code)
via [Happy](https://happy.engineering)

Co-Authored-By: Claude <noreply@anthropic.com>
Co-Authored-By: Happy <yesreply@happy.engineering>"
```

---

### Task 1: DB file mode 0600 + process umask

**Goal:** Set newly-created DB files to mode 0600. Use `syscall.Umask(0o077)` early in `main()` so all files (including SQLite's WAL/shm) inherit secure perms; also set `os.Chmod(path, 0o600)` after `store.Open` succeeds for belt-and-braces on the main `.db` file.

**Files:**
- Modify: `internal/store/db.go` (add `os.Chmod` after integrity check passes)
- Modify: `cmd/arc-relay/main.go` (add `syscall.Umask(0o077)` at the start of `main()`)
- Modify: `internal/store/db_test.go` (new test for the chmod)

**Acceptance Criteria:**
- [ ] `store.Open` calls `os.Chmod(path, 0o600)` after the integrity check passes, for non-`:memory:` paths only
- [ ] Failure to chmod is logged via `slog.Warn` but does NOT fail boot (degraded but functional)
- [ ] `:memory:` paths skip the chmod entirely (no spurious warning)
- [ ] `syscall.Umask(0o077)` is called as one of the first lines in `main()`, before any file is opened
- [ ] Test: opening a tempfile-backed DB results in mode `0o600`
- [ ] Test: `:memory:` open does not error
- [ ] `make test` passes; `make build` succeeds

**Verify:** `cd /Users/ian/code/arc-relay-memory-pivot && CGO_ENABLED=1 go test -tags sqlite_fts5 ./internal/store/ -run "TestOpen_Sets|TestOpen_Memory" -v`

**Steps:**

- [ ] **Step 1: Read current `Open()` to find the right insertion point**

```bash
sed -n '20,55p' internal/store/db.go
```

You'll see `Open` does: `sql.Open` → `Ping` → `integrity_check` → `migrate`. The chmod goes after integrity check (DB file exists by then) and before/after migrations is fine.

- [ ] **Step 2: Add chmod in `internal/store/db.go`**

Find the block that handles the integrity check (around line 35-45). After the `else if result != "ok"` block returns, before `db := &DB{...}` (around line 48), add:

```go
	// Set restrictive mode on the DB file. Best-effort — we log on failure
	// but don't fail boot, since :memory: paths can't be chmod'd and a
	// read-only filesystem would also fail here legitimately.
	if path != ":memory:" && path != "" {
		if err := os.Chmod(path, 0o600); err != nil {
			slog.Warn("could not set db file mode 0600", "path", path, "err", err)
		}
	}
```

`os` and `slog` are likely already imported. Check via `grep -n "\"os\"\|\"log/slog\"" internal/store/db.go`. If `slog` isn't imported, add it.

- [ ] **Step 3: Add umask in `cmd/arc-relay/main.go`**

Find `func main()` (line 32). Add as the FIRST line of the function body:

```go
	syscall.Umask(0o077)
```

`syscall` is already imported (verified). The function body order should now be:

```go
func main() {
	syscall.Umask(0o077)
	configPath := flag.String("config", "", "path to config file (TOML)")
	flag.Parse()
	// ...
}
```

- [ ] **Step 4: Write tests at `internal/store/db_test.go`**

Append (or create the file if it doesn't have an existing test) the following:

```go
package store

import (
	"os"
	"path/filepath"
	"testing"

	migrationsmemory "github.com/comma-compliance/arc-relay/migrations-memory"
)

func TestOpen_SetsFileMode0600(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath, migrationsmemory.FS)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Fatalf("want mode 0600, got %o", mode)
	}
}

func TestOpen_MemoryPathSkipsChmod(t *testing.T) {
	// Should not error and should not attempt a chmod on the path ":memory:"
	db, err := Open(":memory:", migrationsmemory.FS)
	if err != nil {
		t.Fatalf("open :memory:: %v", err)
	}
	defer db.Close()
}
```

The `migrationsmemory.FS` import works because the memory schema is small and self-contained — no need for a separate fixture. If `internal/store/db_test.go` already exists with a different package import path, adapt accordingly.

- [ ] **Step 5: Run tests + build**

```bash
cd /Users/ian/code/arc-relay-memory-pivot
CGO_ENABLED=1 go test -tags sqlite_fts5 ./internal/store/ -run "TestOpen_SetsFileMode0600|TestOpen_MemoryPathSkipsChmod" -v
make test
make build
```

All must pass. The new tests verify the chmod behavior; the broader suite confirms no regression.

- [ ] **Step 6: Commit**

```bash
cd /Users/ian/code/arc-relay-memory-pivot
git add internal/store/db.go internal/store/db_test.go cmd/arc-relay/main.go
git commit -m "fix(security): DB files mode 0600 + process umask 0o077

Two changes that close audit finding #3 (world-readable DB files
on the host volume):

  store.Open() now calls os.Chmod(path, 0o600) after integrity
  check passes. Best-effort — logs slog.Warn on failure but does
  not fail boot. :memory: paths skip the chmod.

  main() sets syscall.Umask(0o077) as its first line, so all files
  the relay creates (including SQLite's WAL/shm and the periodic
  VACUUM-INTO backup files) inherit mode 0o600 / dirs 0o700. The
  Open chmod is belt-and-braces for the main .db file.

Existing files in the production volume still need a one-shot
chmod 600 — that's part of the deploy step, not this commit.

Generated with [Claude Code](https://claude.ai/code)
via [Happy](https://happy.engineering)

Co-Authored-By: Claude <noreply@anthropic.com>
Co-Authored-By: Happy <yesreply@happy.engineering>"
```

---

## Deploy steps (production-only — no code commits)

After Tasks 0 + 1 are merged into the deploy branch and rebuilt, the deploy needs three additional manual operations on the host:

### Step D1: Add `ARC_RELAY_BASE_URL` to the secrets file

```bash
ssh DockerKomodo 'cat /etc/komodo/secrets/mcp-gateway/mcp-gateway.env'
# (verify the file exists and check its current contents)

ssh DockerKomodo 'echo "ARC_RELAY_BASE_URL=https://arc-relay.andersontechsolutions.com" | tee -a /etc/komodo/secrets/mcp-gateway/mcp-gateway.env >/dev/null && grep ARC_RELAY_BASE_URL /etc/komodo/secrets/mcp-gateway/mcp-gateway.env'
```

The file is mode 0600 root-owned per the compose comment. Appending one line is safe.

### Step D2: One-shot chmod on existing volume files

```bash
ssh DockerKomodo 'chmod 600 /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/*.db /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/*.db-* 2>/dev/null && ls -la /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/'
```

Expected: all `*.db`, `*.db-wal`, `*.db-shm` files now show `-rw-------`.

### Step D3: Tag rollback image + rebuild + recreate

```bash
# Tag rollback safety net
ssh DockerKomodo 'docker tag arc-relay:local arc-relay:pre-security-quickwins'

# Push deploy branch (run from local machine — same as Phase 1/2 deploys)
cd /Users/ian/code/arc-relay
git checkout deploy/memory-pivot
git merge --no-ff feat/security-quickwins -m "merge feat/security-quickwins into deploy"
git push DockerKomodo:/opt/arc-relay-build deploy/memory-pivot
git push fork deploy/memory-pivot

# Rebuild on host
ssh DockerKomodo 'cd /opt/arc-relay-build && git checkout deploy/memory-pivot && docker build -t arc-relay:local .'

# Recreate container
ssh DockerKomodo 'cd /etc/komodo/stacks/mcp-gateway/deploy && docker compose -p mcp-gateway -f compose.mcp-gateway.yml up -d --force-recreate arc-relay'
```

### Step D4: Post-deploy verification

```bash
# 1. WWW-Authenticate uses public URL (was localhost:8080)
curl -sI https://arc-relay.andersontechsolutions.com/api/memory/stats | grep -i "www-authenticate"
# Expected: Bearer resource_metadata="https://arc-relay.andersontechsolutions.com/.well-known/oauth-protected-resource/api/memory/stats"

# 2. Device-auth verification_url uses public URL
curl -sX POST -H "Content-Type: application/json" -d '{}' https://arc-relay.andersontechsolutions.com/api/auth/device | python3 -m json.tool
# Expected: verification_url field starts with https://arc-relay.andersontechsolutions.com/

# 3. Rate limit triggers — POST 11 times rapidly to /oauth/register
for i in $(seq 1 11); do
  curl -sw "Attempt $i: HTTP %{http_code}\n" -o /dev/null -X POST -H "Content-Type: application/json" -d '{"redirect_uris":["http://localhost"]}' https://arc-relay.andersontechsolutions.com/oauth/register
done
# Expected: first 10 → HTTP 201; 11th → HTTP 429

# 4. DB files mode 0600
ssh DockerKomodo 'ls -la /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/*.db' | head -3
# Expected: all show -rw-------

# 5. Boot logs clean
ssh DockerKomodo 'docker logs arc-relay --tail 10 2>&1' | grep -E "memory|listening"
# Expected: memory database opened + arc relay listening
```

If anything fails, rollback:

```bash
ssh DockerKomodo 'docker tag arc-relay:pre-security-quickwins arc-relay:local && cd /etc/komodo/stacks/mcp-gateway/deploy && docker compose -p mcp-gateway -f compose.mcp-gateway.yml up -d --force-recreate arc-relay'
```

---

## Self-Review Notes

- **Spec coverage:** §3.1 rate limiting → Task 0 ✅; §3.2 DB mode + umask → Task 1 ✅; §3.3 base URL → Deploy Step D1 ✅. All three findings have an implementation step.
- **Type consistency:** `ipRateLimiter` is consistent across all references (struct, field types, `newIPRateLimiter`, type alias `loginRateLimiter`). Method names `allow` / `record` / `cleanup` match the existing pattern.
- **No placeholders:** every step has actual code, exact file paths, exact commands.
- **TDD-strict for the limiter:** tests exist in Task 0 with deterministic time-injection (no `time.Sleep`).
- **Manual production verification** in Step D4 covers all three fixes via observable behavior (WWW-Authenticate header, verification_url, rate limit response, file mode).
- **Rollback path** documented — single command, single image tag.

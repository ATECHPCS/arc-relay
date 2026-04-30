# Arc Relay Skill Update Checker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers-extended-cc:subagent-driven-development (recommended) or superpowers-extended-cc:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add opt-in upstream tracking on relay skills with daily git-based drift detection, OpenAI `gpt-4o-mini` severity classification (with offline fallback), and an `outdated` status flag surfaced via the existing skills API and `arc-sync skill list --remote`. Includes a Phase 0 swap of arc-relay's LLM client from Anthropic Claude to OpenAI.

**Architecture:** A new `skill_upstreams` table stores the opted-in `(git_url, subpath, ref)` plus inlined latest-drift fields. A new `internal/skills/checker/` package runs a daily cron + on-demand HTTP endpoint that does cheap two-stage detection (`git log` filter, then deterministic subtree-hash diff) and only invokes the LLM when real drift is found. The LLM produces structured JSON (`{severity, summary, recommended_action}`); when no API key is configured, fallback synthesis uses `git log --oneline` output verbatim. Phase 0 re-implements `internal/llm/client.go` against OpenAI's `chat/completions` endpoint while keeping the existing Go interface (`Available/Model/Complete`) intact, so the existing `mcp.OptimizeTools` callers (`internal/web/handlers.go:3363`, `internal/server/http.go:1079`) keep working without modification.

**Tech Stack:** Go 1.24, SQLite (via existing `internal/store`), Anthropic→OpenAI swap (no new SDK — keeps using `net/http` with hand-rolled marshaling), TOML for sidecar parsing (existing `BurntSushi/toml` indirect dep), `git` binary on PATH for upstream fetches (no go-git dependency).

**Spec:** [`docs/superpowers/specs/2026-04-30-arc-relay-skill-update-checker-design.md`](../specs/2026-04-30-arc-relay-skill-update-checker-design.md)

---

## File structure

| File | Change | Owner |
|---|---|---|
| `internal/llm/client.go` | Rewrite request/response shapes for OpenAI `chat/completions`; keep public API identical | Task 0 |
| `internal/llm/client_test.go` | NEW — httptest server fixture asserting OpenAI request shape and response parsing | Task 0 |
| `cmd/arc-relay/main.go:289-294` | Update log message ("OpenAI tool optimizer available") | Task 0 |
| `migrations/017_skill_upstreams.sql` | NEW — `skill_upstreams` table + `skills.outdated` column | Task 1 |
| `internal/store/skills.go` | NEW types `SkillUpstream`, `DriftReport`; methods `UpsertUpstream`, `GetUpstream`, `ClearUpstream`, `ListUpstreams`, `WriteDriftReport`, `ClearDriftReport`, `SetOutdated` | Task 2 |
| `internal/store/skills_test.go` | Tests for the new methods | Task 2 |
| `internal/web/skills_handlers.go` | `uploadVersion` accepts optional `upstream` JSON form field + `clear_upstream` boolean; clears drift on success | Task 3 |
| `internal/web/skills_handlers_test.go` (new or extend) | Push integration tests with upstream metadata | Task 3 |
| `internal/skills/subhash/hash.go` | NEW — deterministic subtree hash function | Task 4 |
| `internal/skills/subhash/hash_test.go` | NEW — table-driven tests including symlinks, executable bit, sort order | Task 4 |
| `internal/cli/sync/upstream.go` | NEW — sidecar TOML parser; `upstream.toml` schema | Task 5 |
| `internal/cli/sync/upstream_test.go` | NEW — parser tests | Task 5 |
| `internal/cli/sync/skills.go` | Extend `Push()` to read sidecar + accept `--upstream-*` flags + send metadata | Task 6 |
| `cmd/arc-sync/skill.go` | Wire new flags into `runSkillPush` | Task 6 |
| `internal/skills/checker/git.go` | NEW — clone/fetch helper with cache dir lifecycle | Task 7 |
| `internal/skills/checker/git_test.go` | NEW — tests against `t.TempDir()` git repos | Task 7 |
| `internal/skills/checker/detect.go` | NEW — two-stage detection (log filter + hash diff) | Task 8 |
| `internal/skills/checker/detect_test.go` | NEW — tests for skip paths and drift detection | Task 8 |
| `internal/skills/checker/cron.go` | NEW — `RunCron(ctx, interval)` matching memory extractor pattern | Task 9 |
| `internal/skills/checker/metrics.go` | NEW — Prometheus counters and histogram | Task 9 |
| `cmd/arc-relay/main.go` | Wire checker.Service + start `RunCron` goroutine | Task 9 |
| `internal/skills/checker/llm.go` | NEW — LLM classification + structured output schema + fallback | Task 10 |
| `internal/skills/checker/llm_test.go` | NEW — tests with mock httptest server | Task 10 |
| `internal/skills/checker/cron.go` | Wire LLM into the per-skill check; persist drift fields | Task 11 |
| `internal/web/skills_handlers.go` | NEW handler `handleCheckDrift`; route at `/api/skills/<slug>/check-drift` | Task 12 |
| `internal/server/http.go` | Register new route ordering | Task 12 |
| `internal/web/skills_handlers.go` | Extend `getSkill` JSON to include `drift` block when `outdated=1` | Task 13 |
| `cmd/arc-sync/skill.go` | NEW subcommand `check-updates [<slug>]` | Task 14 |
| `internal/cli/sync/skills.go` | Extend list-rendering to show `outdated · <severity>` | Task 14 |
| `internal/config/config.go` | NEW `SkillsCheckerConfig` block | Task 15 |
| `config.example.toml` | Document new `[skills.checker]` block | Task 15 |
| `README.md`, `docs/skills.md` (if exists) | Document the feature | Task 15 |

---

## Pre-flight constraints

- **CGO + sqlite_fts5**. All test commands use `CGO_ENABLED=1 go test -tags sqlite_fts5 ./...` per the existing project convention (see `internal/web/handlers_test.go` invocation in earlier plans).
- **Don't introduce new SDK deps.** OpenAI swap stays on `net/http` + hand-rolled JSON, matching the existing Anthropic implementation style. No `github.com/openai/openai-go` import.
- **Git binary, not go-git.** Phase 3 shells out to `git` via `exec.Command`. Already on every container the relay runs in (Dockerfile installs git for the existing memory extractor).
- **Existing patterns to match:**
  - Cron pattern: `internal/memory/extractor/cron.go:20-37` — `RunCron(ctx, interval)` with ticker, ctx cancellation, INFO log on cycle.
  - LLM client gating: `client == nil || !client.Available()` short-circuit. Match this in checker.
  - Migration registration: `migrations/migrations.go` uses `//go:embed *.sql` so new SQL files are picked up automatically; no registration code change needed.
  - Store struct pattern: `internal/store/skills.go:63-72` — `type SkillStore struct { db *DB }`; `func NewSkillStore(db *DB) *SkillStore`. Add new methods to existing struct.
  - Test fixtures for HTTP-based clients: see how `OptimizeTools` could be tested by injecting an `*http.Client` via the `Client` struct's `http` field. We'll do the same in `client_test.go`.
- **No `time.Sleep` in tests.** Cron tests use a `time.Tick`-replacing factory or expose a `RunOnce(ctx)` method that the cron loop calls — same pattern as `archive_dispatcher_test.go:23` (`noTickTicker`).
- **Push handler clears drift on success.** All three "push side-effect" cases from the spec (push with new metadata / push without metadata for existing row / push for skill with no upstream row) must be covered by tests in Task 3.
- **Production env var swap.** `ARC_RELAY_LLM_API_KEY` semantic flips from Anthropic key to OpenAI key. The Komodo stack (`mcp-gateway` on `10.10.0.162`) needs the secret rotated at deploy. Task 0 documents this; deploy is out of plan scope but blocks production rollout.

---

## Phase 0 — Swap LLM provider (Anthropic → OpenAI)

### Task 0: Rewrite `internal/llm/client.go` for OpenAI chat completions

**Goal:** Replace the Anthropic Messages API request/response shapes with OpenAI's `chat/completions` shapes while keeping the public Go interface (`NewClient`, `Available`, `Model`, `Complete`) byte-identical so callers in `internal/mcp/optimize.go` need zero changes.

**Files:**
- Modify: `internal/llm/client.go` (whole file)
- Create: `internal/llm/client_test.go`
- Modify: `cmd/arc-relay/main.go:289-294` (log message wording)

**Acceptance Criteria:**
- [ ] `NewClient(apiKey, model string) *Client` signature unchanged
- [ ] `client.Available()`, `client.Model()`, `client.Complete(ctx, system, userPrompt) (*Result, error)` signatures unchanged
- [ ] `Result` struct fields `Text`, `InputTokens`, `OutputTokens` unchanged (callers in `optimize.go` use `result.Text`)
- [ ] Default model = `"gpt-4o-mini"` (was `"claude-haiku-4-5-20251001"`)
- [ ] Default base URL = `"https://api.openai.com/v1/chat/completions"`
- [ ] Auth header is `Authorization: Bearer <key>` (no `x-api-key`, no `anthropic-version`)
- [ ] System prompt + user prompt are sent as two messages in OpenAI's `messages` array (system first, then user)
- [ ] Response is parsed from `choices[0].message.content` (string)
- [ ] Token usage is parsed from `usage.prompt_tokens` → `InputTokens` and `usage.completion_tokens` → `OutputTokens`
- [ ] Non-200 responses parse OpenAI's error envelope (`{"error": {"message": "...", "type": "..."}}`) and return a clean message
- [ ] httptest-based unit test asserts: request URL, Authorization header, request body shape (model, messages array, max_tokens), and response parsing
- [ ] `go vet ./internal/llm/... && go test ./internal/llm/...` passes
- [ ] `go test ./internal/mcp/...` still passes (existing optimizer tests use a stubbed `*llm.Client`; unaffected, but verify)

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/llm/... ./internal/mcp/... -v`

**Steps:**

- [ ] **Step 1: Write the failing test first**

Create `internal/llm/client_test.go`:

```go
package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComplete_SendsOpenAIShape(t *testing.T) {
	var captured struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		capturedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hello world"}}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 3}
		}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", "gpt-4o-mini")
	c.baseURL = srv.URL

	res, err := c.Complete(context.Background(), "you are helpful", "say hi")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Text != "hello world" {
		t.Errorf("Text = %q, want %q", res.Text, "hello world")
	}
	if res.InputTokens != 12 || res.OutputTokens != 3 {
		t.Errorf("tokens: in=%d out=%d, want 12/3", res.InputTokens, res.OutputTokens)
	}
	if capturedAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", capturedAuth)
	}
	if captured.Model != "gpt-4o-mini" {
		t.Errorf("model = %q", captured.Model)
	}
	if len(captured.Messages) != 2 {
		t.Fatalf("expected 2 messages (system+user), got %d", len(captured.Messages))
	}
	if captured.Messages[0].Role != "system" || captured.Messages[0].Content != "you are helpful" {
		t.Errorf("first message wrong: %+v", captured.Messages[0])
	}
	if captured.Messages[1].Role != "user" || captured.Messages[1].Content != "say hi" {
		t.Errorf("second message wrong: %+v", captured.Messages[1])
	}
}

func TestComplete_NoAPIKey(t *testing.T) {
	c := NewClient("", "")
	_, err := c.Complete(context.Background(), "", "hi")
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("expected API key error, got %v", err)
	}
}

func TestComplete_OpenAIErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"type": "invalid_api_key", "message": "bad key"}}`))
	}))
	defer srv.Close()
	c := NewClient("bad", "")
	c.baseURL = srv.URL
	_, err := c.Complete(context.Background(), "", "hi")
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("expected 'bad key' in error, got %v", err)
	}
}

func TestAvailable(t *testing.T) {
	if NewClient("", "").Available() {
		t.Error("Available() = true with empty key")
	}
	if !NewClient("k", "").Available() {
		t.Error("Available() = false with key")
	}
}

func TestDefaultModel(t *testing.T) {
	c := NewClient("k", "")
	if c.Model() != "gpt-4o-mini" {
		t.Errorf("default model = %q, want gpt-4o-mini", c.Model())
	}
}
```

- [ ] **Step 2: Run the test, confirm failure**

```bash
cd ~/Documents/Repos/arc-relay
CGO_ENABLED=1 go test -tags sqlite_fts5 ./internal/llm/... -run TestComplete_SendsOpenAIShape -v
```

Expected: FAIL — current Anthropic client emits a different request shape.

- [ ] **Step 3: Rewrite `internal/llm/client.go`**

Replace the entire file:

```go
// Package llm provides a minimal OpenAI chat-completions client for tool
// optimization and other internal LLM tasks.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultBaseURL = "https://api.openai.com/v1/chat/completions"
	defaultModel   = "gpt-4o-mini"
	maxTokens      = 16384
)

// Client is a minimal OpenAI chat-completions client.
type Client struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// NewClient creates a new OpenAI client.
// If model is empty, defaults to gpt-4o-mini.
func NewClient(apiKey, model string) *Client {
	if model == "" {
		model = defaultModel
	}
	return &Client{
		apiKey:  apiKey,
		model:   model,
		baseURL: defaultBaseURL,
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func (c *Client) Model() string  { return c.model }
func (c *Client) Available() bool { return c.apiKey != "" }

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []Message `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Result holds the API response text and token usage.
type Result struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// Complete sends system + user messages to OpenAI and returns the response.
func (c *Client) Complete(ctx context.Context, system, userPrompt string) (*Result, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("LLM API key not configured (set ARC_RELAY_LLM_API_KEY)")
	}

	reqBody := chatRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		Messages: []Message{
			{Role: "system", Content: system},
			{Role: "user", Content: userPrompt},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Error.Message != "" {
			return nil, fmt.Errorf("%s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result chatResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("API error: %s: %s", result.Error.Type, result.Error.Message)
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("API returned no choices")
	}

	return &Result{
		Text:         result.Choices[0].Message.Content,
		InputTokens:  result.Usage.PromptTokens,
		OutputTokens: result.Usage.CompletionTokens,
	}, nil
}
```

- [ ] **Step 4: Update main.go log message**

In `cmd/arc-relay/main.go:289-294`, the existing block:
```go
llmClient := llm.NewClient(cfg.LLM.APIKey, cfg.LLM.Model)
if llmClient.Available() {
    slog.Info("LLM tool optimizer available", "model", llmClient.Model())
}
```

Stays as-is. The log message is provider-agnostic ("LLM tool optimizer available"). No edit needed.

- [ ] **Step 5: Run tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/llm/... ./internal/mcp/... -v
```

Expected: all green. The optimizer tests in `internal/mcp/` exercise the optimizer logic with a stubbed client and don't depend on Anthropic-specific bits.

- [ ] **Step 6: Commit**

```bash
git add internal/llm/client.go internal/llm/client_test.go
git commit -m "$(cat <<'EOF'
feat(llm): swap Anthropic client for OpenAI gpt-4o-mini

Re-implements internal/llm/client.go against OpenAI's chat/completions
endpoint while keeping the public Go interface (NewClient, Available,
Model, Complete) unchanged. Existing mcp.OptimizeTools callers in
internal/web/handlers.go:3363 and internal/server/http.go:1079 keep
working without modification.

Production rollout requires rotating ARC_RELAY_LLM_API_KEY from an
Anthropic key to an OpenAI key. Default model flips from
claude-haiku-4-5-20251001 to gpt-4o-mini (~6.7x cheaper input tokens).

Phase 0 of the skill update checker rollout (see
docs/superpowers/specs/2026-04-30-arc-relay-skill-update-checker-design.md).
EOF
)"
```

---

### Task 1: Production env var rotation note

**Goal:** Document the production deploy requirement so the LLM swap doesn't ship with a key that no longer works.

**Files:**
- Modify: `docs/superpowers/plans/2026-04-30-arc-relay-skill-update-checker.md` (this file — add to "Deploy notes" appendix below)
- Modify: `cmd/arc-relay/main.go` only if a clarifying log line is needed (likely not)

**Acceptance Criteria:**
- [ ] Appendix section "Deploy notes" exists at end of this plan documenting the env-var rotation
- [ ] Note covers: source of truth for the new key (1Password "OpenAI API Key" item), Komodo stack name (`mcp-gateway`), env file path (`/etc/komodo/secrets/mcp-gateway/mcp-gateway.env`), and the rollout order (rotate key → restart container → verify with admin tool-optimizer page)

**Verify:** Manual review of the appendix.

**Steps:**

- [ ] **Step 1: Add Deploy notes appendix**

Append to this plan file (after the last task):

```markdown
---

## Deploy notes

**LLM key rotation (Phase 0 prerequisite for production):**

1. Provision an OpenAI API key in the team OpenAI account, save to 1Password as "Arc Relay OpenAI Key" in `API/SSH/Tokens`.
2. Update Komodo secret: `op item get "Arc Relay OpenAI Key" --reveal --field=credential` → paste into `/etc/komodo/secrets/mcp-gateway/mcp-gateway.env` as `ARC_RELAY_LLM_API_KEY=<value>`. Run `chmod 600` on the file.
3. Redeploy `mcp-gateway` stack via Komodo: `komodo execute DeployStackService` (see memory: arc-relay deploy flow).
4. Smoke test: visit the admin tool-optimizer page in arc-relay's web UI; trigger an optimization and confirm a 200 response with non-empty optimized tools.

**Skill checker config (Phase 6 deploy):**

Add to `/etc/komodo/secrets/mcp-gateway/mcp-gateway.env`:
```
ARC_RELAY_SKILLS_CHECKER_ENABLED=true
```

Restart container. Verify via `arc-sync skill check-updates` from a host with admin credentials.
```

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/plans/2026-04-30-arc-relay-skill-update-checker.md
git commit -m "docs(plans): add deploy notes for skill update checker rollout"
```

---

## Phase 1 — Schema + Push wiring

### Task 2: Migration `017_skill_upstreams.sql`

**Goal:** Add the `skill_upstreams` table and `skills.outdated` column. Purely additive — no backfill, no data loss path.

**Files:**
- Create: `migrations/017_skill_upstreams.sql`

**Acceptance Criteria:**
- [ ] Migration file embedded via existing `//go:embed *.sql` in `migrations/migrations.go` (no registration code change)
- [ ] `skill_upstreams.skill_id` is PK + FK to `skills(id)` ON DELETE CASCADE
- [ ] `skills.outdated` defaults to 0
- [ ] `CHECK` constraint on `drift_severity` enforces NULL or one of `cosmetic|minor|major|security|unknown`
- [ ] Migration is idempotent: `CREATE TABLE IF NOT EXISTS` and `ALTER TABLE` is wrapped to skip if column already exists (SQLite idiom: try/catch in test)
- [ ] `go test ./internal/store/...` passes (existing migration smoke tests pick up the new file automatically)

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/store/... -v`

**Steps:**

- [ ] **Step 1: Create the migration file**

```sql
-- migrations/017_skill_upstreams.sql
--
-- Skill update checker (see docs/superpowers/specs/2026-04-30-arc-relay-skill-update-checker-design.md).
-- Adds opt-in upstream tracking + inlined latest-drift fields.

CREATE TABLE IF NOT EXISTS skill_upstreams (
    skill_id                 TEXT PRIMARY KEY REFERENCES skills(id) ON DELETE CASCADE,
    upstream_type            TEXT NOT NULL DEFAULT 'git'
                                 CHECK(upstream_type IN ('git')),
    git_url                  TEXT NOT NULL,
    git_subpath              TEXT NOT NULL DEFAULT '',
    git_ref                  TEXT NOT NULL DEFAULT 'HEAD',

    -- last successful check (whether or not drift was found):
    last_checked_at          DATETIME,
    last_seen_sha            TEXT,
    last_seen_hash           TEXT,

    -- latest drift; all NULL once a new version clears it:
    drift_detected_at        DATETIME,
    drift_relay_version      TEXT,
    drift_relay_hash         TEXT,
    drift_upstream_sha       TEXT,
    drift_upstream_hash      TEXT,
    drift_commits_ahead      INTEGER,
    drift_severity           TEXT CHECK(drift_severity IS NULL OR
                                        drift_severity IN ('cosmetic','minor','major','security','unknown')),
    drift_summary            TEXT,
    drift_recommended_action TEXT,
    drift_llm_model          TEXT,

    created_at               DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at               DATETIME DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE skills ADD COLUMN outdated INTEGER NOT NULL DEFAULT 0;
```

> Note: SQLite's `ALTER TABLE ... ADD COLUMN` is non-idempotent. The existing migration runner in `internal/store/db.go` runs migrations sequentially and tracks applied versions, so re-running this migration on an already-migrated DB is not a concern in production. For tests, the migration runner is invoked once per fresh `t.TempDir()` DB.

- [ ] **Step 2: Run existing migration tests**

```bash
cd ~/Documents/Repos/arc-relay
CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/store/... -v
```

Expected: all pass. The migration runner picks up `017_*.sql` automatically.

- [ ] **Step 3: Commit**

```bash
git add migrations/017_skill_upstreams.sql
git commit -m "feat(skills): migration for skill_upstreams table + skills.outdated column"
```

---

### Task 3: `SkillStore` upstream + drift methods

**Goal:** Add Go-level types and store methods for managing the new table.

**Files:**
- Modify: `internal/store/skills.go`
- Modify: `internal/store/skills_test.go`

**Acceptance Criteria:**
- [ ] New types `SkillUpstream` and `DriftReport` defined in `internal/store/skills.go`
- [ ] `(*SkillStore).UpsertUpstream(u *SkillUpstream) error` — INSERT OR REPLACE on `skill_id`
- [ ] `(*SkillStore).GetUpstream(skillID string) (*SkillUpstream, error)` — returns `(nil, nil)` if no row, error only on DB failure
- [ ] `(*SkillStore).ClearUpstream(skillID string) error` — DELETE
- [ ] `(*SkillStore).ListUpstreams() ([]*SkillUpstream, error)` — for the cron iterator; ORDER BY `last_checked_at NULLS FIRST, skill_id`
- [ ] `(*SkillStore).WriteDriftReport(skillID string, r *DriftReport) error` — UPDATEs the drift_* columns + sets `skills.outdated=1` in same transaction
- [ ] `(*SkillStore).ClearDriftReport(skillID string, latestSeenHash string) error` — NULLs all `drift_*` cols, sets `last_seen_hash`, sets `skills.outdated=0` in same transaction
- [ ] `(*SkillStore).UpdateUpstreamCheck(skillID, sha, hash string, checkedAt time.Time) error` — for "no drift" cron path; updates only `last_seen_sha`, `last_seen_hash`, `last_checked_at`
- [ ] All methods have unit tests covering happy path + missing-row + edge cases (e.g., GetUpstream returns nil for unknown skill, not error)
- [ ] `go test ./internal/store/...` passes

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/store/ -run "Upstream|Drift" -v`

**Steps:**

- [ ] **Step 1: Add types to `internal/store/skills.go`**

After the existing `type SkillAssignment struct` block, append:

```go
// SkillUpstream is the opted-in upstream-tracking row for a skill.
// One row per skill_id (1:1 with skills).
type SkillUpstream struct {
	SkillID                  string
	UpstreamType             string // "git"
	GitURL                   string
	GitSubpath               string
	GitRef                   string
	LastCheckedAt            *time.Time
	LastSeenSHA              *string
	LastSeenHash             *string
	DriftDetectedAt          *time.Time
	DriftRelayVersion        *string
	DriftRelayHash           *string
	DriftUpstreamSHA         *string
	DriftUpstreamHash        *string
	DriftCommitsAhead        *int
	DriftSeverity            *string
	DriftSummary             *string
	DriftRecommendedAction   *string
	DriftLLMModel            *string
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// DriftReport is what the checker writes when drift is detected.
// All fields required.
type DriftReport struct {
	RelayVersion       string
	RelayHash          string
	UpstreamSHA        string
	UpstreamHash       string
	CommitsAhead       int
	Severity           string // cosmetic|minor|major|security|unknown
	Summary            string
	RecommendedAction  string
	LLMModel           string // empty if fallback path
	DetectedAt         time.Time
}
```

- [ ] **Step 2: Add store methods**

Append to `internal/store/skills.go`:

```go
func (s *SkillStore) UpsertUpstream(u *SkillUpstream) error {
	if u.UpstreamType == "" {
		u.UpstreamType = "git"
	}
	if u.GitRef == "" {
		u.GitRef = "HEAD"
	}
	_, err := s.db.Exec(`
		INSERT INTO skill_upstreams (
			skill_id, upstream_type, git_url, git_subpath, git_ref,
			last_seen_hash, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(skill_id) DO UPDATE SET
			upstream_type = excluded.upstream_type,
			git_url       = excluded.git_url,
			git_subpath   = excluded.git_subpath,
			git_ref       = excluded.git_ref,
			updated_at    = CURRENT_TIMESTAMP
	`, u.SkillID, u.UpstreamType, u.GitURL, u.GitSubpath, u.GitRef, u.LastSeenHash)
	return err
}

func (s *SkillStore) GetUpstream(skillID string) (*SkillUpstream, error) {
	row := s.db.QueryRow(`
		SELECT skill_id, upstream_type, git_url, git_subpath, git_ref,
			last_checked_at, last_seen_sha, last_seen_hash,
			drift_detected_at, drift_relay_version, drift_relay_hash,
			drift_upstream_sha, drift_upstream_hash, drift_commits_ahead,
			drift_severity, drift_summary, drift_recommended_action, drift_llm_model,
			created_at, updated_at
		FROM skill_upstreams WHERE skill_id = ?
	`, skillID)
	var u SkillUpstream
	err := row.Scan(
		&u.SkillID, &u.UpstreamType, &u.GitURL, &u.GitSubpath, &u.GitRef,
		&u.LastCheckedAt, &u.LastSeenSHA, &u.LastSeenHash,
		&u.DriftDetectedAt, &u.DriftRelayVersion, &u.DriftRelayHash,
		&u.DriftUpstreamSHA, &u.DriftUpstreamHash, &u.DriftCommitsAhead,
		&u.DriftSeverity, &u.DriftSummary, &u.DriftRecommendedAction, &u.DriftLLMModel,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *SkillStore) ClearUpstream(skillID string) error {
	_, err := s.db.Exec(`DELETE FROM skill_upstreams WHERE skill_id = ?`, skillID)
	return err
}

func (s *SkillStore) ListUpstreams() ([]*SkillUpstream, error) {
	rows, err := s.db.Query(`
		SELECT skill_id, upstream_type, git_url, git_subpath, git_ref,
			last_checked_at, last_seen_sha, last_seen_hash,
			drift_detected_at, drift_relay_version, drift_relay_hash,
			drift_upstream_sha, drift_upstream_hash, drift_commits_ahead,
			drift_severity, drift_summary, drift_recommended_action, drift_llm_model,
			created_at, updated_at
		FROM skill_upstreams
		ORDER BY last_checked_at IS NULL DESC, last_checked_at ASC, skill_id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*SkillUpstream
	for rows.Next() {
		var u SkillUpstream
		if err := rows.Scan(
			&u.SkillID, &u.UpstreamType, &u.GitURL, &u.GitSubpath, &u.GitRef,
			&u.LastCheckedAt, &u.LastSeenSHA, &u.LastSeenHash,
			&u.DriftDetectedAt, &u.DriftRelayVersion, &u.DriftRelayHash,
			&u.DriftUpstreamSHA, &u.DriftUpstreamHash, &u.DriftCommitsAhead,
			&u.DriftSeverity, &u.DriftSummary, &u.DriftRecommendedAction, &u.DriftLLMModel,
			&u.CreatedAt, &u.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

func (s *SkillStore) UpdateUpstreamCheck(skillID, sha, hash string, checkedAt time.Time) error {
	_, err := s.db.Exec(`
		UPDATE skill_upstreams SET
			last_seen_sha = ?, last_seen_hash = ?, last_checked_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE skill_id = ?
	`, sha, hash, checkedAt, skillID)
	return err
}

func (s *SkillStore) WriteDriftReport(skillID string, r *DriftReport) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		UPDATE skill_upstreams SET
			drift_detected_at = ?, drift_relay_version = ?, drift_relay_hash = ?,
			drift_upstream_sha = ?, drift_upstream_hash = ?, drift_commits_ahead = ?,
			drift_severity = ?, drift_summary = ?, drift_recommended_action = ?,
			drift_llm_model = ?,
			last_seen_sha = ?, last_seen_hash = ?, last_checked_at = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE skill_id = ?
	`, r.DetectedAt, r.RelayVersion, r.RelayHash,
		r.UpstreamSHA, r.UpstreamHash, r.CommitsAhead,
		r.Severity, r.Summary, r.RecommendedAction, r.LLMModel,
		r.UpstreamSHA, r.UpstreamHash, r.DetectedAt,
		skillID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE skills SET outdated = 1 WHERE id = ?`, skillID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SkillStore) ClearDriftReport(skillID, latestSeenHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		UPDATE skill_upstreams SET
			drift_detected_at = NULL, drift_relay_version = NULL, drift_relay_hash = NULL,
			drift_upstream_sha = NULL, drift_upstream_hash = NULL, drift_commits_ahead = NULL,
			drift_severity = NULL, drift_summary = NULL, drift_recommended_action = NULL,
			drift_llm_model = NULL,
			last_seen_hash = ?, updated_at = CURRENT_TIMESTAMP
		WHERE skill_id = ?
	`, latestSeenHash, skillID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE skills SET outdated = 0 WHERE id = ?`, skillID); err != nil {
		return err
	}
	return tx.Commit()
}
```

Add `"database/sql"` to imports if not already present.

- [ ] **Step 3: Add tests to `internal/store/skills_test.go`**

```go
func TestSkillStore_UpsertGetClearUpstream(t *testing.T) {
	st := setupSkillStore(t) // existing helper from skills_test.go
	// Seed a skill row first (foreign key requirement):
	_ = st.CreateSkill(&Skill{ID: "sk-1", Slug: "test-skill", DisplayName: "Test"})

	// GetUpstream on missing row returns (nil, nil)
	u, err := st.GetUpstream("sk-1")
	if err != nil || u != nil {
		t.Fatalf("expected (nil,nil), got (%v, %v)", u, err)
	}

	// Upsert creates
	if err := st.UpsertUpstream(&SkillUpstream{
		SkillID: "sk-1", GitURL: "https://github.com/foo/bar",
		GitSubpath: "skills/baz", GitRef: "main",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	u, err = st.GetUpstream("sk-1")
	if err != nil || u == nil {
		t.Fatalf("Get: %v %v", u, err)
	}
	if u.GitURL != "https://github.com/foo/bar" || u.GitSubpath != "skills/baz" {
		t.Errorf("wrong fields: %+v", u)
	}

	// Upsert updates (same skill_id, different ref)
	if err := st.UpsertUpstream(&SkillUpstream{
		SkillID: "sk-1", GitURL: "https://github.com/foo/bar",
		GitSubpath: "skills/baz", GitRef: "develop",
	}); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	u, _ = st.GetUpstream("sk-1")
	if u.GitRef != "develop" {
		t.Errorf("Update did not change ref: %q", u.GitRef)
	}

	// Clear
	if err := st.ClearUpstream("sk-1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	u, _ = st.GetUpstream("sk-1")
	if u != nil {
		t.Errorf("expected nil after Clear, got %+v", u)
	}
}

func TestSkillStore_DriftReport(t *testing.T) {
	st := setupSkillStore(t)
	_ = st.CreateSkill(&Skill{ID: "sk-2", Slug: "drift-skill", DisplayName: "Drift"})
	_ = st.UpsertUpstream(&SkillUpstream{
		SkillID: "sk-2", GitURL: "https://example.com/x", GitSubpath: "", GitRef: "HEAD",
	})

	now := time.Now().UTC().Truncate(time.Second)
	rep := &DriftReport{
		RelayVersion: "0.1.0", RelayHash: "abcd",
		UpstreamSHA: "deadbeef", UpstreamHash: "ef12", CommitsAhead: 3,
		Severity: "minor", Summary: "added stuff", RecommendedAction: "review",
		LLMModel: "gpt-4o-mini", DetectedAt: now,
	}
	if err := st.WriteDriftReport("sk-2", rep); err != nil {
		t.Fatalf("WriteDriftReport: %v", err)
	}

	u, _ := st.GetUpstream("sk-2")
	if u.DriftSeverity == nil || *u.DriftSeverity != "minor" {
		t.Errorf("DriftSeverity not persisted: %+v", u.DriftSeverity)
	}
	if u.DriftCommitsAhead == nil || *u.DriftCommitsAhead != 3 {
		t.Errorf("DriftCommitsAhead = %v", u.DriftCommitsAhead)
	}

	// Verify skills.outdated flipped to 1
	sk, _ := st.GetSkill("sk-2")
	if sk == nil || sk.Outdated != 1 {
		t.Errorf("skills.outdated not set: %+v", sk)
	}

	// Clear drift
	if err := st.ClearDriftReport("sk-2", "newhash"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	u, _ = st.GetUpstream("sk-2")
	if u.DriftSeverity != nil {
		t.Errorf("Severity not cleared: %v", u.DriftSeverity)
	}
	if u.LastSeenHash == nil || *u.LastSeenHash != "newhash" {
		t.Errorf("LastSeenHash = %v, want newhash", u.LastSeenHash)
	}
	sk, _ = st.GetSkill("sk-2")
	if sk.Outdated != 0 {
		t.Errorf("outdated not cleared: %+v", sk)
	}
}
```

Note: `Skill.Outdated` field needs to be added to the existing `Skill` struct in `skills.go` (alongside the new column). Add `Outdated int` to the struct and update `GetSkill`/`ListSkills` SELECTs to include it.

- [ ] **Step 4: Run tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/store/ -run "Upstream|Drift" -v
```

Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/store/skills.go internal/store/skills_test.go
git commit -m "feat(skills): SkillStore methods for upstream tracking + drift reports"
```

---

### Task 4: Push handler accepts upstream metadata + clears drift

**Goal:** Extend the existing push endpoint (`uploadVersion` in `internal/web/skills_handlers.go`) to accept optional `upstream` JSON form field and `clear_upstream` boolean. Also clear drift fields on every successful push (regardless of metadata) for skills with an existing upstream row.

**Files:**
- Modify: `internal/web/skills_handlers.go` (`uploadVersion` method around line 298)
- Modify: `internal/web/skills_handlers_test.go` or create if missing

**Acceptance Criteria:**
- [ ] After successful version insert, push handler reads optional multipart fields:
  - `upstream` (string, optional): JSON `{"type":"git","url":"...","subpath":"...","ref":"..."}`
  - `clear_upstream` (string, optional): "true" → DELETE existing row
- [ ] If `upstream` is present and parses, calls `SkillStore.UpsertUpstream`
- [ ] If `clear_upstream=true`, calls `SkillStore.ClearUpstream`
- [ ] If both absent: existing upstream row left in place
- [ ] After any successful push: if a `skill_upstreams` row exists, call `ClearDriftReport(skillID, newSubtreeHash)` (computed from the uploaded tarball — this depends on Task 5's `subhash` package, which is built in parallel; for this task, accept a placeholder hash `""` and update in Task 11 wiring)
- [ ] Response JSON gains `"upstream_recorded": true|false`
- [ ] Unit tests cover all three side-effect cases (with new metadata, without metadata for existing row, without metadata and no row)
- [ ] Existing push tests still pass

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/web/ -run "Skill.*[Uu]pload|Skill.*[Pp]ush|Upstream" -v`

**Steps:**

- [ ] **Step 1: Read the existing `uploadVersion` to understand multipart layout**

```bash
sed -n '298,335p' internal/web/skills_handlers.go
```

Note where versioning + manifest writes happen; insert upstream side-effects after the version row is committed (so a metadata error doesn't block the version itself).

- [ ] **Step 2: Add upstream parsing + side effects**

Inside `uploadVersion`, after the existing successful version commit and before writing the JSON response:

```go
// Optional upstream metadata (added in skill update checker rollout — see
// docs/superpowers/specs/2026-04-30-arc-relay-skill-update-checker-design.md).
upstreamRecorded := false
if raw := r.FormValue("clear_upstream"); raw == "true" {
	if err := h.skillStore.ClearUpstream(skill.ID); err != nil {
		slog.Warn("clear upstream failed", "skill_id", skill.ID, "err", err)
	}
} else if raw := r.FormValue("upstream"); raw != "" {
	var meta struct {
		Type    string `json:"type"`
		URL     string `json:"url"`
		Subpath string `json:"subpath"`
		Ref     string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		slog.Warn("malformed upstream JSON", "skill_id", skill.ID, "err", err)
	} else if meta.Type != "" && meta.Type != "git" {
		slog.Warn("unsupported upstream type", "skill_id", skill.ID, "type", meta.Type)
	} else if meta.URL == "" {
		slog.Warn("upstream missing url", "skill_id", skill.ID)
	} else {
		if err := h.skillStore.UpsertUpstream(&store.SkillUpstream{
			SkillID:    skill.ID,
			UpstreamType: "git",
			GitURL:     meta.URL,
			GitSubpath: meta.Subpath,
			GitRef:     meta.Ref,
		}); err != nil {
			slog.Warn("upsert upstream failed", "skill_id", skill.ID, "err", err)
		} else {
			upstreamRecorded = true
		}
	}
}

// Always clear drift state on successful push, IF an upstream row exists.
// last_seen_hash placeholder for now; Task 11 wires the real subtree hash.
if u, _ := h.skillStore.GetUpstream(skill.ID); u != nil {
	if err := h.skillStore.ClearDriftReport(skill.ID, ""); err != nil {
		slog.Warn("clear drift failed", "skill_id", skill.ID, "err", err)
	}
}

// Existing JSON response — add upstream_recorded:
resp := map[string]any{
	"id":            skill.ID,
	"slug":          skill.Slug,
	"version":       version.Version,
	"upstream_recorded": upstreamRecorded,
	// ... existing fields
}
```

(Adapt the response shape to whatever the existing handler currently returns — keep all existing keys, add the one new key.)

- [ ] **Step 3: Add tests**

In `internal/web/skills_handlers_test.go` (create if absent), add three tests:

```go
func TestUploadVersion_WithUpstreamMetadata(t *testing.T) {
	h, _ := newSkillsHandlersTestRig(t) // existing or new helper

	body, ct := buildPushMultipart(t, map[string]string{
		"version":  "0.2.0",
		"upstream": `{"type":"git","url":"https://github.com/foo/bar","subpath":"skills/baz","ref":"main"}`,
	}, "fake-tarball-bytes")
	req := httptest.NewRequest("POST", "/api/skills/test-slug/versions", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+adminToken(t))
	w := httptest.NewRecorder()

	h.HandleSkillByPath(w, req)
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["upstream_recorded"] != true {
		t.Errorf("upstream_recorded = %v", resp["upstream_recorded"])
	}

	u, _ := h.skillStore.GetUpstream("sk-test")
	if u == nil || u.GitURL != "https://github.com/foo/bar" {
		t.Errorf("upstream not stored: %+v", u)
	}
}

func TestUploadVersion_WithoutMetadata_PreservesExistingUpstream(t *testing.T) { /* push twice; second push omits metadata; verify row still there */ }

func TestUploadVersion_ClearUpstream(t *testing.T) { /* seed upstream, push with clear_upstream=true, verify row deleted */ }
```

- [ ] **Step 4: Run tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/web/ -run "Upload|Skill|Upstream" -v
```

Expected: green.

- [ ] **Step 5: Commit**

```bash
git add internal/web/skills_handlers.go internal/web/skills_handlers_test.go
git commit -m "feat(skills): push handler accepts upstream metadata + clears drift fields"
```

---

## Phase 2 — Subtree hash + sidecar + CLI push

### Task 5: Deterministic subtree hash function

**Goal:** Implement the spec's subtree-hash algorithm (sorted file walk, mode-aware, mtime-stripped) as a standalone, byte-stable function in a new `internal/skills/subhash` package.

**Files:**
- Create: `internal/skills/subhash/hash.go`
- Create: `internal/skills/subhash/hash_test.go`

**Acceptance Criteria:**
- [ ] `func Hash(rootDir string) (string, error)` returns a hex SHA256 string
- [ ] Walk order is `filepath.Walk` with sorted entries per directory (lexicographic, byte-wise)
- [ ] Skips: `.git/` (always), `.DS_Store` (always), and entries matching `.gitignore` patterns inside `rootDir` (best-effort — use `os.ReadFile` + line-by-line glob)
- [ ] Per-file emit: `<relative-path>\n<mode-octal>\n<content-bytes>\n`
- [ ] File modes: regular files → `0o100644` or `0o100755` (executable bit only); symlinks → `0o120000` with link target as content
- [ ] Same input bytes → same hash, regardless of mtime, atime, or filesystem
- [ ] Tests cover: empty dir, single file, sorted order with multiple files, executable bit, symlink, `.gitignore` exclusion, `.git/` exclusion, identical content twice → same hash, content tweak → different hash
- [ ] No allocations larger than necessary; streams content into hasher (never `os.ReadFile` whole file into memory; use `io.Copy` to the hash)

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/skills/subhash/... -v`

**Steps:**

- [ ] **Step 1: Write tests first**

```go
package subhash

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHash_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	h, err := Hash(dir)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if len(h) != 64 {
		t.Errorf("expected 64-char hex, got %q", h)
	}
}

func TestHash_DeterministicAcrossOrderingNoise(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	// Create same files in different order
	_ = os.WriteFile(filepath.Join(dir1, "b.txt"), []byte("b"), 0o644)
	_ = os.WriteFile(filepath.Join(dir1, "a.txt"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir2, "a.txt"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir2, "b.txt"), []byte("b"), 0o644)
	h1, _ := Hash(dir1)
	h2, _ := Hash(dir2)
	if h1 != h2 {
		t.Errorf("h1=%s h2=%s should match", h1, h2)
	}
}

func TestHash_ExecutableBitChangesHash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "script.sh")
	_ = os.WriteFile(p, []byte("#!/bin/sh"), 0o644)
	h1, _ := Hash(dir)
	_ = os.Chmod(p, 0o755)
	h2, _ := Hash(dir)
	if h1 == h2 {
		t.Error("hash should differ when exec bit changes")
	}
}

func TestHash_ContentChangesHash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	_ = os.WriteFile(p, []byte("a"), 0o644)
	h1, _ := Hash(dir)
	_ = os.WriteFile(p, []byte("b"), 0o644)
	h2, _ := Hash(dir)
	if h1 == h2 {
		t.Error("content change should change hash")
	}
}

func TestHash_SkipsGitDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a"), 0o644)
	h1, _ := Hash(dir)

	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core]"), 0o644)
	h2, _ := Hash(dir)
	if h1 != h2 {
		t.Errorf(".git/ should be excluded; h1=%s h2=%s", h1, h2)
	}
}

func TestHash_RespectsGitignore(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("k"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	h1, _ := Hash(dir)
	_ = os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("noise"), 0o644)
	h2, _ := Hash(dir)
	if h1 != h2 {
		t.Errorf(".gitignore'd file should be excluded; h1=%s h2=%s", h1, h2)
	}
}

func TestHash_SymlinkContentUsesTarget(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hi"), 0o644)
	_ = os.Symlink("real.txt", filepath.Join(dir, "link"))
	h, err := Hash(dir)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if len(h) != 64 {
		t.Errorf("expected hash, got %q", h)
	}
}
```

- [ ] **Step 2: Run tests, confirm failure**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/skills/subhash/... -v
```

Expected: package not found.

- [ ] **Step 3: Implement `internal/skills/subhash/hash.go`**

```go
// Package subhash computes deterministic SHA256 hashes over directory subtrees
// for skill upstream drift detection.
package subhash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Hash returns a deterministic SHA256 hex digest of the contents of rootDir.
// The hash is stable across mtime, sort order, and filesystem boundaries.
// Excludes .git/, .DS_Store, and files matching .gitignore patterns at root.
func Hash(rootDir string) (string, error) {
	ignored, err := loadGitignorePatterns(rootDir)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	if err := walk(rootDir, "", h, ignored); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func walk(absRoot, relDir string, h io.Writer, ignored []string) error {
	absDir := filepath.Join(absRoot, relDir)
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, e := range entries {
		name := e.Name()
		if name == ".git" || name == ".DS_Store" {
			continue
		}
		relPath := filepath.ToSlash(filepath.Join(relDir, name))
		if matchAny(ignored, relPath) {
			continue
		}
		if e.IsDir() {
			if err := walk(absRoot, relPath, h, ignored); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		mode := uint32(0o100644)
		if info.Mode()&os.ModeSymlink != 0 {
			mode = 0o120000
			target, err := os.Readlink(filepath.Join(absRoot, relPath))
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\n%o\n%s\n", relPath, mode, target)
			continue
		}
		if info.Mode()&0o111 != 0 {
			mode = 0o100755
		}
		fmt.Fprintf(h, "%s\n%o\n", relPath, mode)
		f, err := os.Open(filepath.Join(absRoot, relPath))
		if err != nil {
			return err
		}
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
		_, _ = h.Write([]byte("\n"))
	}
	return nil
}

func loadGitignorePatterns(root string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func matchAny(patterns []string, relPath string) bool {
	base := filepath.Base(relPath)
	for _, p := range patterns {
		ok, _ := filepath.Match(p, relPath)
		if ok {
			return true
		}
		ok, _ = filepath.Match(p, base)
		if ok {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/skills/subhash/... -v
```

Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/skills/subhash/
git commit -m "feat(skills): deterministic subtree hash for drift detection"
```

---

### Task 6: Sidecar TOML parser + CLI push integration

**Goal:** Parse `.arc-sync/upstream.toml` from the skill source dir during `arc-sync skill push`. CLI flags override the file. Send the resulting metadata as a multipart `upstream` field to the relay.

**Files:**
- Create: `internal/cli/sync/upstream.go`
- Create: `internal/cli/sync/upstream_test.go`
- Modify: `internal/cli/sync/skills.go` (existing `Push` method)
- Modify: `cmd/arc-sync/skill.go` (existing `runSkillPush` to add flags)

**Acceptance Criteria:**
- [ ] `LoadUpstream(skillDir string) (*Upstream, error)` reads `.arc-sync/upstream.toml`, returns nil if absent
- [ ] `(*Upstream).WithOverrides(url, subpath, ref string, clear bool) *Upstream` applies flag overrides
- [ ] Sidecar schema:
```toml
[upstream]
type    = "git"
url     = "..."
subpath = "..."
ref     = "..."
```
- [ ] CLI flags on `skill push`: `--upstream-git`, `--upstream-path`, `--upstream-ref`, `--no-upstream`
- [ ] `--no-upstream` sends `clear_upstream=true` to the relay (skips reading sidecar)
- [ ] Push request includes multipart `upstream` JSON when metadata is present
- [ ] Tests cover: missing sidecar, malformed sidecar, sidecar+flags merge, `--no-upstream`
- [ ] Existing push tests still pass

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/cli/sync/ -run "Upstream|Push" -v`

**Steps:**

- [ ] **Step 1: Tests first** (`internal/cli/sync/upstream_test.go`):

```go
package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUpstream_Missing(t *testing.T) {
	u, err := LoadUpstream(t.TempDir())
	if err != nil || u != nil {
		t.Errorf("expected (nil,nil), got (%v,%v)", u, err)
	}
}

func TestLoadUpstream_Valid(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".arc-sync"), 0o755)
	body := `
[upstream]
type    = "git"
url     = "https://github.com/foo/bar"
subpath = "skills/baz"
ref     = "main"
`
	_ = os.WriteFile(filepath.Join(dir, ".arc-sync", "upstream.toml"), []byte(body), 0o644)
	u, err := LoadUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.URL != "https://github.com/foo/bar" || u.Subpath != "skills/baz" || u.Ref != "main" {
		t.Errorf("got %+v", u)
	}
}

func TestUpstream_Overrides(t *testing.T) {
	u := &Upstream{Type: "git", URL: "old", Ref: "main"}
	o := u.WithOverrides("new", "sub", "develop", false)
	if o.URL != "new" || o.Subpath != "sub" || o.Ref != "develop" {
		t.Errorf("override not applied: %+v", o)
	}
}
```

- [ ] **Step 2: Implement `internal/cli/sync/upstream.go`**

```go
package sync

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Upstream struct {
	Type    string `toml:"type"`
	URL     string `toml:"url"`
	Subpath string `toml:"subpath"`
	Ref     string `toml:"ref"`
}

type upstreamFile struct {
	Upstream Upstream `toml:"upstream"`
}

// LoadUpstream reads .arc-sync/upstream.toml from skillDir.
// Returns (nil, nil) if the file does not exist.
func LoadUpstream(skillDir string) (*Upstream, error) {
	p := filepath.Join(skillDir, ".arc-sync", "upstream.toml")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f upstreamFile
	if _, err := toml.Decode(string(b), &f); err != nil {
		return nil, err
	}
	if f.Upstream.URL == "" {
		return nil, errors.New("upstream.toml: url is required")
	}
	if f.Upstream.Type == "" {
		f.Upstream.Type = "git"
	}
	return &f.Upstream, nil
}

// WithOverrides applies CLI-flag overrides on top of a sidecar-loaded Upstream.
// Empty override strings leave the existing field untouched. clearAll=true
// returns a sentinel zero-value with Type = "" to signal "send clear_upstream=true".
func (u *Upstream) WithOverrides(url, subpath, ref string, clearAll bool) *Upstream {
	if clearAll {
		return &Upstream{} // zero value signals "clear"
	}
	out := *u
	if url != "" {
		out.URL = url
	}
	if subpath != "" {
		out.Subpath = subpath
	}
	if ref != "" {
		out.Ref = ref
	}
	return &out
}
```

- [ ] **Step 3: Add flags to `cmd/arc-sync/skill.go` `runSkillPush`**

Find the `flag` declarations for `runSkillPush` and add:

```go
upstreamGit := flag.String("upstream-git", "", "git URL for upstream tracking (overrides sidecar)")
upstreamPath := flag.String("upstream-path", "", "subpath inside upstream repo")
upstreamRef := flag.String("upstream-ref", "", "branch/tag/sha (default HEAD)")
noUpstream := flag.Bool("no-upstream", false, "ignore sidecar and signal clear_upstream=true")
```

Wire these into the call to the existing push function. Pseudocode:

```go
side, _ := sync.LoadUpstream(skillDir) // existing or new helper
var meta *sync.Upstream
if *noUpstream {
	meta = (&sync.Upstream{}).WithOverrides("", "", "", true) // clear sentinel
} else if side != nil || *upstreamGit != "" {
	base := side
	if base == nil {
		base = &sync.Upstream{Type: "git"}
	}
	meta = base.WithOverrides(*upstreamGit, *upstreamPath, *upstreamRef, false)
}
// Pass meta into existing Push() call.
```

- [ ] **Step 4: Extend `Push()` in `internal/cli/sync/skills.go`**

Add an `upstream *Upstream` argument (or option). When non-nil:

```go
if upstream != nil {
	if upstream.Type == "" && upstream.URL == "" {
		// Sentinel from --no-upstream
		_ = mw.WriteField("clear_upstream", "true")
	} else {
		js, _ := json.Marshal(map[string]string{
			"type": upstream.Type, "url": upstream.URL,
			"subpath": upstream.Subpath, "ref": upstream.Ref,
		})
		_ = mw.WriteField("upstream", string(js))
	}
}
```

(Existing `Push` already builds a `multipart.Writer`. Find the spot where the tarball is being written and add this metadata field beforehand.)

- [ ] **Step 5: Run tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/cli/sync/... -v
```

- [ ] **Step 6: Manual smoke**

```bash
cd /tmp && mkdir -p test-skill/.arc-sync
cat > test-skill/SKILL.md <<'EOF'
---
name: test-skill
description: smoke test
---
# test
EOF
cat > test-skill/.arc-sync/upstream.toml <<'EOF'
[upstream]
url = "https://github.com/marcfargas/odoo-toolbox"
subpath = "skills/odoo"
ref = "master"
EOF

go run ./cmd/arc-sync skill push /tmp/test-skill --version 0.0.1 --visibility public
```

Expected: push succeeds; `arc-sync skill list --remote --json` shows the new skill; `sqlite3 <db> "SELECT * FROM skill_upstreams"` shows the row.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/sync/upstream.go internal/cli/sync/upstream_test.go internal/cli/sync/skills.go cmd/arc-sync/skill.go
git commit -m "feat(arc-sync): sidecar + flags for upstream tracking on skill push"
```

---

## Phase 3 — Drift checker (no LLM yet)

### Task 7: Git fetch helper with cache dir lifecycle

**Goal:** Wrap `exec.Command("git", ...)` with a lifecycle that does `git clone --no-tags --filter=blob:none` on first run and `git fetch origin` on subsequent runs, with corruption recovery.

**Files:**
- Create: `internal/skills/checker/git.go`
- Create: `internal/skills/checker/git_test.go`

**Acceptance Criteria:**
- [ ] `func EnsureCache(ctx context.Context, cacheDir, gitURL string) error` — clone if dir empty, fetch if dir has a `.git`, re-clone if any subcommand fails
- [ ] `func ResolveSHA(ctx context.Context, cacheDir, ref string) (string, error)` — returns `git rev-parse <ref>` output, trimmed
- [ ] `func LogPath(ctx context.Context, cacheDir, fromSHA, toSHA, subpath string) ([]string, error)` — returns `git log <fromSHA>..<toSHA> -- <subpath> --oneline` output as a slice of lines (empty slice if no commits touch the path)
- [ ] `func CheckoutSubpath(ctx context.Context, cacheDir, sha, subpath, destDir string) error` — uses `git --git-dir <cache>/.git --work-tree <dest> checkout <sha> -- <subpath>` then moves into `destDir`
- [ ] All functions take `ctx` for cancellation; respect `git_clone_timeout` from config
- [ ] Tests use `t.TempDir()` git repos created via `exec.Command("git", "init", ...)` — no network required

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/skills/checker/... -run "Ensure|Resolve|LogPath" -v`

**Steps:**

- [ ] **Step 1: Tests first** (`internal/skills/checker/git_test.go`):

```go
package checker

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func makeTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	_ = exec.Command("touch", filepath.Join(dir, "a.txt")).Run() // os.WriteFile in real test
	run("add", ".")
	run("commit", "-m", "init")
	return dir
}

func TestEnsureCache_FreshClone(t *testing.T) {
	src := makeTestRepo(t)
	cache := t.TempDir()
	if err := EnsureCache(context.Background(), cache, src); err != nil {
		t.Fatal(err)
	}
	// Should have a .git
	if _, err := exec.Command("git", "-C", cache, "rev-parse", "HEAD").Output(); err != nil {
		t.Errorf("clone didn't take: %v", err)
	}
}

func TestLogPath_NoCommitsTouchingSubpath(t *testing.T) { /* commit only top-level file; query subpath; expect empty */ }
func TestLogPath_ReturnsTouchedCommits(t *testing.T) { /* multi-commit fixture */ }
```

- [ ] **Step 2: Implement `internal/skills/checker/git.go`** (omitted here for brevity — straightforward `exec.Command` wrappers around `git clone --no-tags --filter=blob:none`, `git fetch origin`, `git rev-parse`, `git log`)

- [ ] **Step 3: Run tests; iterate until green**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/skills/checker/... -run "Ensure|Resolve|LogPath" -v
```

- [ ] **Step 4: Commit**

```bash
git add internal/skills/checker/git.go internal/skills/checker/git_test.go
git commit -m "feat(checker): git cache lifecycle for upstream fetches"
```

---

### Task 8: Two-stage detection function

**Goal:** Compose `EnsureCache` + `ResolveSHA` + `LogPath` + `subhash.Hash` into a single function that returns a `Detection` result with the three skip paths and the drift confirmation case.

**Files:**
- Create: `internal/skills/checker/detect.go`
- Create: `internal/skills/checker/detect_test.go`

**Acceptance Criteria:**
- [ ] `Detection` struct: `{ Result enum NoMovement|NoPathTouch|RevertedToSame|Drift, NewSHA, NewHash, CommitsAhead int, ChangedFiles []string, DiffSummary string }`
- [ ] `func Detect(ctx, upstream, lastSeenSHA, lastSeenHash, cacheDir) (*Detection, error)`
- [ ] Skip path 1: resolved SHA == lastSeenSHA → `NoMovement`
- [ ] Skip path 2: log of subpath empty → `NoPathTouch`
- [ ] Skip path 3: new hash == lastSeenHash → `RevertedToSame`
- [ ] Otherwise: `Drift` with populated diff metadata
- [ ] `DiffSummary` is the `git diff --stat` truncated to `llm_diff_max_bytes`
- [ ] Tests cover all four outcomes with `t.TempDir()` repos

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/skills/checker/... -run "Detect" -v`

**Steps:**

(Standard TDD cycle: write each outcome's test first, implement, repeat. Skipped here for brevity — pattern matches Tasks 5 and 7.)

- [ ] **Commit:** `git commit -m "feat(checker): two-stage drift detection"`

---

### Task 9: Cron + Prometheus metrics + main wiring

**Goal:** Drop the cron in alongside `RunCron` from `internal/memory/extractor/cron.go:20`. Wire it into `cmd/arc-relay/main.go`. Surface Prometheus counters.

**Note on config:** This task introduces a minimal `config.SkillsCheckerConfig` struct (`Enabled`, `Interval`, `UpstreamCacheDir`) so main.go can wire the cron. Task 15 expands the same struct with LLM/diff-size knobs. Don't duplicate — extend.

**Files:**
- Create: `internal/skills/checker/cron.go`
- Create: `internal/skills/checker/metrics.go`
- Create: `internal/skills/checker/service.go` (the `Service` type wiring stores/git/llm together)
- Modify: `cmd/arc-relay/main.go` (start the cron goroutine after the existing extractor cron)

**Acceptance Criteria:**
- [ ] `type Service struct { skills *store.SkillStore, llm *llm.Client, cfg config.SkillsCheckerConfig }` (LLM unused yet, wired in Task 11)
- [ ] `func (s *Service) RunCron(ctx context.Context, interval time.Duration)` ticker loop matching `extractor.RunCron`
- [ ] `func (s *Service) RunOnce(ctx context.Context)` runs one full pass; called by cron and by Task 12's HTTP endpoint
- [ ] `func (s *Service) checkOne(ctx, *SkillUpstream)` does the 4-outcome handling; for `Drift` writes the report (Task 11 fills in LLM call; for now writes severity=`unknown` summary `"drift detected"`)
- [ ] Prometheus counters registered:
  - `arc_relay_skill_checks_total{result}` 
  - `arc_relay_skill_check_duration_seconds` (histogram)
- [ ] main.go starts `go skillChecker.RunCron(ctx, cfg.Skills.Checker.Interval)` after the extractor cron line
- [ ] Integration test: seed a skill + upstream + fixture git repo; run `RunOnce`; assert the upstream row is updated

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/skills/checker/... -v && go build ./...`

**Steps:**

(Mirror `internal/memory/extractor/cron.go` structure. main.go wiring:)

```go
// In cmd/arc-relay/main.go after extractorSvc.RunCron:
skillChecker := checker.NewService(skillStore, llmClient, cfg.Skills.Checker)
go skillChecker.RunCron(ctx, cfg.Skills.Checker.Interval)
```

- [ ] **Commit:** `git commit -m "feat(checker): daily cron + Prometheus metrics + main wiring"`

---

## Phase 4 — LLM integration

### Task 10: LLM classifier + fallback synthesis

**Goal:** Build `Classify(ctx, llm, skill, det) (*store.DriftReport, error)`. When LLM available, send structured prompt; parse JSON response. When unavailable or fails, synthesize a fallback report from `det.ChangedFiles` and `det.DiffSummary`.

**Files:**
- Create: `internal/skills/checker/llm.go`
- Create: `internal/skills/checker/llm_test.go`

**Acceptance Criteria:**
- [ ] LLM prompt matches the spec's prompt sketch (severity guide, JSON-only response)
- [ ] Parsed response validates: severity is one of the allowed enum, summary non-empty, recommended_action non-empty
- [ ] Invalid LLM JSON → fallback path (logs warn, doesn't fail the cron)
- [ ] Fallback summary: `"<N> commits touched <subpath> since the published version. Run \`git log\` upstream for details: <first 3 oneline entries>"`; severity=`unknown`; recommended_action=`"Review upstream commits manually before pulling."`
- [ ] Tests use httptest stub of OpenAI responding with both valid and malformed JSON; also test no-LLM path

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/skills/checker/... -run "Classify|Fallback" -v`

**Steps:** (TDD as before; commit when green) — `git commit -m "feat(checker): LLM classifier with offline fallback"`

---

### Task 11: Wire LLM into checker; persist drift fields

**Goal:** Replace the placeholder `severity=unknown` writes in `Service.checkOne` with a real call to `Classify`. Connect Task 4's push-side `ClearDriftReport` placeholder hash to a real subtree hash via `subhash.Hash` of the unpacked tarball.

**Files:**
- Modify: `internal/skills/checker/service.go`
- Modify: `internal/web/skills_handlers.go` (`uploadVersion` from Task 4 — replace `""` with real hash)
- Modify: `internal/skills/service.go` (add a helper to compute the subtree hash from a freshly-uploaded archive — extract to tmp dir, hash, delete)

**Acceptance Criteria:**
- [ ] `checkOne` flows through `Classify` and writes the resulting `DriftReport`
- [ ] `uploadVersion` computes the subtree hash from the uploaded tarball before calling `ClearDriftReport`
- [ ] Integration test: end-to-end push → cron → drift detected → report written, with stubbed OpenAI server
- [ ] No-LLM-key integration test: same flow, fallback severity=unknown, summary contains "Run `git log`"

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./... -run "Drift|Checker" -v`

**Steps:** TDD; integration test using the same `t.TempDir()` git fixture pattern. Commit: `git commit -m "feat(checker): wire LLM into per-skill check + real subtree hash on push"`

---

## Phase 5 — API + CLI surfacing

### Task 12: `POST /api/skills/<slug>/check-drift` endpoint

**Goal:** Add the on-demand HTTP endpoint. Admin-only.

**Files:**
- Modify: `internal/web/skills_handlers.go` (new `handleCheckDrift` method)
- Modify: `internal/server/http.go` (route registration; ordering: `/check-drift` is a deeper path than `/api/skills/`, falls through `HandleSkillByPath`)

**Acceptance Criteria:**
- [ ] `POST /api/skills/<slug>/check-drift` admin-only
- [ ] Calls `skillChecker.RunOnce` scoped to one skill (add `RunOneSlug(ctx, slug)` to checker.Service)
- [ ] Returns 200 + drift JSON if drift detected (or already flagged), 204 if up-to-date, 404 if slug unknown, 409 if no upstream row, 502 on upstream fetch failure
- [ ] 60s timeout
- [ ] Tests for each status code

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/web/ -run "CheckDrift" -v`

**Steps:** TDD; commit: `git commit -m "feat(api): on-demand drift-check endpoint"`

---

### Task 13: Extend `GET /api/skills` JSON with drift block

**Goal:** When a skill has `outdated=1`, include a `drift` object in the JSON response. Existing consumers (which ignore unknown fields) keep working.

**Files:**
- Modify: `internal/web/skills_handlers.go` (`getSkill` and the list rendering)

**Acceptance Criteria:**
- [ ] `getSkill` returns `drift: {...}` when present
- [ ] List endpoint includes `drift` per-row (only when present)
- [ ] Tests assert: skill without drift has no `drift` key; skill with drift has the full block

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./internal/web/ -run "Skill.*[Dd]rift|GetSkill" -v`

**Steps:** TDD; commit: `git commit -m "feat(api): expose drift block in skills GET responses"`

---

### Task 14: arc-sync `skill check-updates` + list display

**Goal:** Wire CLI consumers — new `check-updates` subcommand and extended `list --remote` output that shows `outdated · <severity>`.

**Files:**
- Modify: `cmd/arc-sync/skill.go` (new `runSkillCheckUpdates`, dispatch in `runSkill`)
- Modify: `internal/cli/sync/skills.go` (extend list rendering)

**Acceptance Criteria:**
- [ ] `arc-sync skill check-updates <slug>` calls the new endpoint, prints drift summary to stdout
- [ ] `arc-sync skill check-updates` (no slug) iterates all skills with declared upstreams, prints status-per-skill
- [ ] `arc-sync skill list --remote` shows `outdated · <severity>` in the STATUS column
- [ ] `--json` output includes the `drift` block per row
- [ ] Tests for both commands

**Verify:** Manual: `arc-sync skill check-updates odoo-toolbox`. Unit: `go test ./internal/cli/sync/... ./cmd/arc-sync/... -v`

**Steps:** TDD + manual; commit: `git commit -m "feat(arc-sync): skill check-updates command + outdated display"`

---

## Phase 6 — Config + docs

### Task 15: Config block + documentation

**Goal:** Wire the new config block into `config.example.toml` and `internal/config/config.go`. Update README + `docs/skills.md` if it exists.

**Files:**
- Modify: `internal/config/config.go` (new `SkillsCheckerConfig` struct under `Config.Skills.Checker`)
- Modify: `config.example.toml` (document `[skills.checker]` block)
- Modify: `README.md` (one paragraph + link to docs/skills.md)
- Modify: `docs/skills.md` (if exists, otherwise create with a single section on update checking)

**Acceptance Criteria:**
- [ ] `SkillsCheckerConfig` fields: `Enabled bool, Interval time.Duration, UpstreamCacheDir string, GitCloneTimeout time.Duration, LLMModel string, LLMDiffMaxBytes int, LLMPerFileMaxBytes int`
- [ ] Env var overrides for `Enabled`, `Interval`, `UpstreamCacheDir` (matching existing arc-relay convention)
- [ ] Defaults match the spec (`24h`, `/var/lib/arc-relay/upstream-cache`, `60s`, `gpt-4o-mini`, `32768`, `4096`)
- [ ] `config.example.toml` has a documented stanza
- [ ] README links to the new docs section
- [ ] All tests pass

**Verify:** `cd ~/Documents/Repos/arc-relay && CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./... && go build ./...`

**Steps:**

- [ ] **Step 1: Add config struct**

```go
// In internal/config/config.go:
type SkillsConfig struct {
	BundlesDir string                `toml:"bundles_dir"`
	Checker    SkillsCheckerConfig  `toml:"checker"`
}

type SkillsCheckerConfig struct {
	Enabled            bool          `toml:"enabled"`
	Interval           time.Duration `toml:"interval"`
	UpstreamCacheDir   string        `toml:"upstream_cache_dir"`
	GitCloneTimeout    time.Duration `toml:"git_clone_timeout"`
	LLMModel           string        `toml:"llm_model"`
	LLMDiffMaxBytes    int           `toml:"llm_diff_max_bytes"`
	LLMPerFileMaxBytes int           `toml:"llm_per_file_max_bytes"`
}
```

Add defaulting in `Load()` if zero values present.

- [ ] **Step 2: Document `config.example.toml`**

```toml
[skills.checker]
enabled = true
interval = "24h"
upstream_cache_dir = "/var/lib/arc-relay/upstream-cache"
git_clone_timeout = "60s"
llm_model = "gpt-4o-mini"
llm_diff_max_bytes = 32768
llm_per_file_max_bytes = 4096
```

- [ ] **Step 3: Final test**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./... && go build ./...
```

- [ ] **Step 4: Commit**

```bash
git commit -m "feat(skills): config block + docs for skill update checker"
```

---

## Verification: end-to-end smoke

Once all tasks land, run this manual smoke test against a real upstream:

```bash
# 1. Add upstream metadata to the existing odoo-toolbox skill
cat > ~/.agents/skills/odoo-toolbox/.arc-sync/upstream.toml <<'EOF'
[upstream]
url = "https://github.com/marcfargas/odoo-toolbox"
subpath = "skills/odoo"
ref = "master"
EOF

# 2. Push 0.1.1 with metadata
arc-sync skill push ~/.agents/skills/odoo-toolbox --version 0.1.1 --visibility public

# 3. Trigger drift check (should be no-op — we just pushed)
arc-sync skill check-updates odoo-toolbox
# Expected: "up-to-date"

# 4. Wait a day (or simulate by manually advancing fixtures); meanwhile pretend upstream moves
# In production: real time passes; cron picks up next morning.

# 5. Verify
arc-sync skill list --remote
# Expected: odoo-toolbox shows outdated · <severity> if upstream has moved
```

---

## Deploy notes

**LLM key rotation (Phase 0 prerequisite for production):**

1. Provision an OpenAI API key in the team OpenAI account, save to 1Password as "Arc Relay OpenAI Key" in `API/SSH/Tokens`.
2. Update the Komodo secret file `/etc/komodo/secrets/mcp-gateway/mcp-gateway.env`: replace the existing `ARC_RELAY_LLM_API_KEY=<anthropic-key>` with the new OpenAI key. Run `chmod 600` on the file after editing.
3. Redeploy `mcp-gateway` stack via Komodo: `komodo execute DeployStackService` (see memory: arc-relay deploy flow).
4. Smoke test: visit the admin tool-optimizer page in arc-relay's web UI; trigger an optimization and confirm a 200 response with non-empty optimized tools.

**Skill checker config (Phase 6 deploy):**

Add to `/etc/komodo/secrets/mcp-gateway/mcp-gateway.env`:
```
ARC_RELAY_SKILLS_CHECKER_ENABLED=true
ARC_RELAY_SKILLS_CHECKER_INTERVAL=24h
```

Restart container via Komodo. Verify via `arc-sync skill check-updates` from a host with admin credentials.

**Sanity check the data model migration:**

```bash
sqlite3 /var/lib/docker/volumes/mcp-gateway_arc-relay-data/_data/arc-relay.db \
  "SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'skill%';"
# Expect: skills, skill_versions, skill_assignments, skill_upstreams
```

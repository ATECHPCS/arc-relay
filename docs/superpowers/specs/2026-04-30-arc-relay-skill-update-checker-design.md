# Arc Relay Skill Update Checker — Design

**Status:** Drafted 2026-04-30 from a brainstorm session. Awaiting user review before implementation plan.

## Goal

Detect when a published skill on the relay has drifted from its declared upstream git source and surface that drift to admins via the relay catalog and `arc-sync skill list --remote`. The checker uses cheap git-based detection by default and invokes an LLM only when real drift is found, to classify severity and produce a human-readable summary.

## Motivation

Skills like `odoo-toolbox@0.1.0` are pushed as frozen snapshots of upstream repos (e.g., `marcfargas/odoo-toolbox#master/skills/odoo`). Today the relay has no concept of "upstream" — once a skill is pushed it sits at that version forever, and the admin who pushed it has no way to know upstream has progressed. This design adds opt-in upstream tracking and a daily drift check.

## Non-goals (v1)

- **Auto-republish.** The checker only flags drift; pulling and re-pushing remains manual.
- **Push notifications.** No Discord, no email. Surface is the in-product status flag and CLI/API consumers polling for it.
- **Drift history.** Only the latest drift report is retained per skill (inlined on the upstream row).
- **Private repos.** Only public git URLs are supported in v1. No deploy keys, no PAT auth.
- **Non-git upstream sources.** No tarball URLs, no npm-style registries, no federated arc-relay catalogs.
- **Heuristic upstream detection.** Tracking is strictly opt-in via declared metadata.
- **Opinionated update workflow.** No `arc-sync skill update <slug>` — the admin pulls and re-pushes themselves once they see drift.

## Locked decisions (from brainstorm)

| # | Decision | Rationale |
|---|---|---|
| 1 | Hybrid detection: cheap git checks in-process; LLM invoked only when real drift is found | Cheap path stays cheap; ~$0.005 per drift event in LLM cost |
| 2 | Opt-in tracking only — skills without declared upstream are ignored by the checker | Eliminates false positives; explicit declaration is the contract |
| 3 | Sidecar file `.arc-sync/upstream.toml` is the source of truth; CLI flags override | Travels with the skill source, persists across re-pushes, version-controllable |
| 4 | Git-only upstream type in v1 | Covers the dominant case (skills extracted from GitHub repos); other types deferred |
| 5 | Two-stage detection: `git log <stored>..<HEAD> -- <subpath>` filter, then content-hash diff | Filters out commits that don't touch the subpath and reverts that net to zero |
| 6 | Daily cron + on-demand `arc-sync skill check-updates [<slug>]` | Fresh enough for slowly-changing skills; on-demand path for impatience and CI |
| 7 | Status-flag surface only (no push notifications in v1) | Minimal new infra; pull model fits admin workflow; v2 can layer push channels |
| 8 | LLM produces structured output: severity / summary / recommended_action | Searchable, surface-able in list output; easier to render in CLIs |
| 9 | Never auto-republish | Drift only updates the status flag; admin runs push manually after review |
| 10 | LLM is optional — if no API key configured, fall back to `severity=unknown` + `git log --oneline` summary | Keeps the feature usable on relays without LLM credentials |
| 11 | Drop drift-report history; inline latest drift fields on `skill_upstreams` row | Simpler schema; `git log` upstream gets full history back if ever needed |
| 12 | Swap arc-relay's LLM client from Anthropic (`claude-haiku-4-5`) to OpenAI (`gpt-4o-mini`) as Phase 0 of this implementation | 6.7× cheaper input tokens; existing `mcp.OptimizeTools` callers keep working through the unchanged Go interface; production deploy needs an OpenAI API key swap at the same time |

## Architecture

```
                         ┌─────────────────────────┐
   .arc-sync/            │   arc-sync skill push   │
   upstream.toml ───────►│   (CLI flags override)  │
                         └────────────┬────────────┘
                                      │ stores upstream metadata
                                      ▼
                         ┌─────────────────────────┐
                         │   skill_upstreams       │ ◄── new table
                         │   (one row per skill)   │
                         └────────────┬────────────┘
                                      │
                              daily cron + on-demand
                                      │
                                      ▼
                         ┌─────────────────────────┐
                         │   drift checker         │
                         │   (Go, in-process)      │
                         │                         │
                         │   1. git fetch upstream │
                         │   2. log path diff      │
                         │   3. content hash diff  │
                         └────────────┬────────────┘
                                      │ if drift
                                      ▼
                         ┌─────────────────────────┐
                         │   LLM (gpt-4o-mini)     │
                         │   classify + summarize  │
                         │   (or git-log fallback) │
                         └────────────┬────────────┘
                                      ▼
                         ┌─────────────────────────┐
                         │  skill_upstreams.drift_*│
                         │  fields populated       │
                         └────────────┬────────────┘
                                      ▼
                         ┌─────────────────────────┐
                         │  skills.outdated = 1    │
                         └────────────┬────────────┘
                                      ▼
                         ┌─────────────────────────┐
                         │ arc-sync skill list     │
                         │ --remote shows STATUS:  │
                         │ outdated · <severity>   │
                         └─────────────────────────┘
```

The checker lives entirely in the arc-relay repo. New code in `internal/skills/checker/`, sidecar parsing and CLI extensions in `internal/cli/`, one new migration. The LLM client reuses the existing arc-relay LLM integration (the one already serving memory categorization).

## Data model

One new migration: `017_skill_upstreams.sql`. Purely additive.

```sql
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

PK is `skill_id` (1:1 with `skills`). Re-pointing upstream is an UPDATE.

`outdated = 1` is set when the checker writes drift fields. It's cleared back to `0` whenever `arc-sync skill push` succeeds for that skill (push always trumps drift).

### Subtree hash convention

`last_seen_hash`, `drift_relay_hash`, and `drift_upstream_hash` are computed the same deterministic way to allow byte-for-byte comparison between the relay's stored content and any upstream checkout:

- Walk the subpath in sorted-by-relative-path order (lexicographic, locale-independent).
- Skip directories themselves; include only regular files.
- Skip files matching `.gitignore` patterns inside the subpath, plus `.DS_Store` and `.git/` always.
- For each file: append `<relative-path>\n<file-mode-octal>\n<file-content-bytes>\n` to a hashing stream.
  - File mode = 0o100644 for regular files, 0o100755 if executable bit set, 0o120000 for symlinks (with link target as content). No other bits.
  - Mtime is not included (irrelevant to content).
- The SHA256 of the concatenated stream is the subtree hash.

This is independent of `skill_versions.archive_sha256`, which hashes the whole upload (manifest, packaging metadata) and is not directly comparable to a fresh upstream checkout.

The relay computes this subtree hash at push time and writes it to `last_seen_hash` so the very next cron run has a baseline.

## Cron + LLM behavior

Single-goroutine sequential cron in `internal/skills/checker/`, default daily.

1. Select all `skill_upstreams` rows with `upstream_type = 'git'`, ordered by `last_checked_at NULLS FIRST` (least-recently-checked first).
2. For each row:
   - On first run for this skill: `git clone --no-tags --filter=blob:none <git_url>` into `upstream_cache_dir/<skill_id>/`. Subsequent runs: `git fetch origin` in the existing cache dir. If the cache dir exists but is corrupt (any git command fails), delete and re-clone.
   - Resolve `git_ref` to a commit SHA.
   - **Skip path 1:** If `resolved_sha == last_seen_sha`, no upstream movement. Update `last_checked_at`. Done.
   - Run `git log <last_seen_sha>..<resolved_sha> -- <git_subpath> --oneline`.
   - **Skip path 2:** If empty, no commits touched the subpath. Update `last_seen_sha` and `last_checked_at`. Done.
   - Compute deterministic subtree hash at `<resolved_sha>:<git_subpath>`.
   - **Skip path 3:** If `new_hash == last_seen_hash`, commits touched the path but net content unchanged (revert, mtime-only, etc.). Update SHA pointer + `last_checked_at`. Done.
   - **Drift confirmed.** Build LLM input:
     - `git diff --stat last_seen_sha..resolved_sha -- git_subpath` (file list with +/-)
     - Truncated diff (first `llm_per_file_max_bytes` per file, capped at `llm_diff_max_bytes` total)
     - Skill name + description for context
   - Invoke LLM (default `gpt-4o-mini`) via the existing arc-relay LLM client. Structured output via JSON schema.
   - **LLM fallback.** If no LLM credentials configured (or call fails after retry), synthesize:
     - `severity = "unknown"`
     - `summary = "<N> commits touched <subpath> since the published version. Run `git log` upstream for details."` (plus the `git log --oneline` lines, truncated)
     - `recommended_action = "Review upstream commits manually before pulling."`
   - UPDATE `skill_upstreams` with all `drift_*` fields, `last_seen_sha`, `last_seen_hash`, `last_checked_at`. UPDATE `skills SET outdated = 1`.
3. Per-skill errors (network, auth, malformed repo) are logged + counted, never fatal to the cron run.

### Drift cleared on push

The existing `arc-sync skill push` handler is extended: after successful version insert, run:

```sql
UPDATE skill_upstreams SET
    drift_detected_at = NULL,
    drift_relay_version = NULL,
    drift_relay_hash = NULL,
    drift_upstream_sha = NULL,
    drift_upstream_hash = NULL,
    drift_commits_ahead = NULL,
    drift_severity = NULL,
    drift_summary = NULL,
    drift_recommended_action = NULL,
    drift_llm_model = NULL,
    last_seen_hash = ?,             -- subtree hash from the new upload
    updated_at = CURRENT_TIMESTAMP
WHERE skill_id = ?;

UPDATE skills SET outdated = 0 WHERE id = ?;
```

If the push call also provides upstream metadata (creating or updating the `skill_upstreams` row), `last_seen_sha` is left NULL and `last_seen_hash` is set to the subtree hash of the uploaded tarball — the next cron run will perform the first upstream fetch and populate `last_seen_sha`. If the push omits upstream metadata for an existing row, only `last_seen_hash` is updated (so the next drift check compares against what was actually shipped); `last_seen_sha` is left untouched and any in-flight drift fields are cleared as described above. If the skill has no `skill_upstreams` row, no upstream side-effects occur.

### LLM prompt (sketch)

```
You are reviewing a code change to a Claude Code skill.

Skill: <name>
Description: <description>
Files changed: <git diff --stat output>
Diff (truncated):
<diff>

Classify the severity of this change for a user who has the previous version
installed. Output JSON only:
{
  "severity": "cosmetic" | "minor" | "major" | "security",
  "summary": "<2-3 sentences>",
  "recommended_action": "<one sentence>"
}

Severity guide:
  cosmetic   = docs, formatting, comments only
  minor      = additive changes, new features, bugfixes that don't break existing usage
  major      = breaking changes to commands, args, or behavior; renames; deletions
  security   = fixes for vulnerabilities, exposed secrets, auth bypasses
```

### Cost ceiling

`gpt-4o-mini` at ~$0.15/M input, ~$0.60/M output. With ~32K input + ~200 output per drift event: ~$0.005 per drift. Cron over 100 skills with 1–2 daily drifts: pennies/month.

### Failure modes

| Failure | Behavior |
|---|---|
| Repo deleted / 404 / DNS fail | Log warning, increment counter, leave existing drift state untouched, skip skill. |
| Upstream requires auth | Same as above. v2 adds creds. |
| LLM call fails after retry | Use git-log fallback summary; still flag `outdated = 1` with `severity = unknown`. |
| Diff exceeds `llm_diff_max_bytes` | Truncate per-file at `llm_per_file_max_bytes`; mark `[truncated]` in prompt; LLM is told. |
| Cache dir corrupt | Delete and re-clone on next run. |

## API surface

All admin-scoped (matches `push`, `assign`, `unassign`).

### `POST /api/skills` (existing, extended)

Multipart form gains optional fields:
- `upstream` — JSON object: `{ "type": "git", "url": "...", "subpath": "...", "ref": "..." }`
- `clear_upstream` — boolean form field for `--no-upstream` from the CLI

If `upstream` is present and parses, UPSERT `skill_upstreams`. If `clear_upstream=true`, DELETE the row. If both absent: leave existing row untouched (re-push doesn't accidentally clear metadata).

Response gains `upstream_recorded: true|false`.

### `POST /api/skills/<slug>/check-drift` (new)

Body empty. Synchronously runs the same checker code path against this single slug.

| Code | Meaning | Body |
|---|---|---|
| `200 OK` | Drift detected (or already flagged) | Full drift report JSON |
| `204 No Content` | Up-to-date | empty |
| `404 Not Found` | Slug unknown | error JSON |
| `409 Conflict` | Skill has no `skill_upstreams` row | error JSON |
| `502 Bad Gateway` | Upstream fetch failed | error JSON |

Times out at 60s. Fail-soft on LLM (uses git-log fallback, returns 200 with `severity=unknown`).

### `GET /api/skills` and `GET /api/skills/<slug>` (existing, extended)

Response objects gain optional `drift` block when `outdated = 1`:
```json
{
  "slug": "odoo-toolbox",
  "version": "0.1.0",
  ...
  "drift": {
    "severity": "major",
    "summary": "Upstream renamed `odoo records` to `odoo crud` and removed the legacy alias.",
    "recommended_action": "Review usage before pulling — command rename will break existing scripts.",
    "detected_at": "2026-04-30T12:00:00Z",
    "commits_ahead": 7,
    "upstream_sha": "ec3e164…"
  }
}
```

Existing consumers ignore unknown fields. No shape change.

## CLI surface (`internal/cli/`)

Three changes to arc-sync. Implementation skill list (`arc-sync skill update <slug>`, `arc-sync skill upstream <slug>`) is deferred.

### `arc-sync skill push` (extended)

Reads `.arc-sync/upstream.toml` if present in the skill dir:
```toml
[upstream]
type    = "git"
url     = "https://github.com/marcfargas/odoo-toolbox"
subpath = "skills/odoo"
ref     = "master"
```

CLI flags override sidecar:
```
arc-sync skill push <dir> --version V \
  [--upstream-git URL] [--upstream-path SUBPATH] [--upstream-ref REF] \
  [--no-upstream]
```

Push uploads tarball + upstream metadata in one multipart request. The relay also computes the subtree hash from the uploaded tarball at push time and writes it to `last_seen_hash` for the baseline.

### `arc-sync skill list --remote` (extended display)

```
SLUG              VERSION    VISIBILITY    STATUS
arc-sync          1.0.3      public        active
odoo-toolbox      0.1.0      public        outdated · major
some-other-skill  2.1.0      public        active
yanked-thing      0.1.0      public        yanked
```

`--json` output includes the `drift` object from the API.

### `arc-sync skill check-updates [<slug>]` (new)

```
$ arc-sync skill check-updates odoo-toolbox
Checking odoo-toolbox against upstream marcfargas/odoo-toolbox#master...
Drift detected (major):
  Upstream renamed `odoo records` to `odoo crud` and removed the legacy alias.
  Recommended: Review usage before pulling — command rename will break existing scripts.
  7 commits ahead. New SHA: ec3e164.

$ arc-sync skill check-updates
Checking 12 skills...
  arc-sync           up-to-date
  odoo-toolbox       outdated (major)
  ...
2 skills outdated.
```

Hits `POST /api/skills/<slug>/check-drift`. No-slug form iterates skills with declared upstreams.

## Configuration

New keys in `config.example.toml`:

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

LLM credentials reused from arc-relay's existing `[llm]` block.

## Observability

New Prometheus metrics:
- `arc_relay_skill_checks_total{result="ok|skip|drift|error"}` (counter)
- `arc_relay_skill_drift_llm_calls_total{outcome="ok|fallback|error"}` (counter)
- `arc_relay_skill_check_duration_seconds` (histogram)

Structured logs at `info` for each check, `warn` for upstream fetch failures, `error` only for unexpected panics.

## Testing strategy

- **Unit tests**: subtree-hash determinism, sidecar TOML parsing, git-log filtering helper, drift severity de-serialization, push handler clears drift fields correctly, LLM fallback synthesizes valid report.
- **Integration test**: `git init` in `t.TempDir()`, push fixture commits, run the checker against a fixture skill row, assert drift fields populate correctly. No network. No LLM.
- **LLM mock**: reuse arc-relay's existing LLM client interface (already used by memory categorization tests).
- **Manual smoke** (one-off, not in CI): `arc-sync skill check-updates odoo-toolbox` against the real `marcfargas/odoo-toolbox`.

## Migration considerations

- Migration `017_skill_upstreams.sql` is purely additive.
- Existing skills work unchanged. No backfill.
- `outdated` defaults to `0` for all existing rows.
- For `odoo-toolbox@0.1.0` (the immediate motivating skill): drop `.arc-sync/upstream.toml` into `~/.agents/skills/odoo-toolbox/` pointing to `marcfargas/odoo-toolbox#master/skills/odoo`, push `0.1.1`, and the cron picks it up the next day.

## Out of scope (v2+)

- Private repo authentication (deploy keys, PATs)
- Non-git upstream sources (tarball URLs, npm, federated arc-relay catalogs)
- Push notifications (Discord, email, generic webhook)
- Auto-republish
- Drift report history / audit trail
- `arc-sync skill update <slug>` opinionated pull-and-push workflow
- `arc-sync skill upstream <slug> [--get|--set|--clear]` post-hoc upstream metadata management
- Per-user notification preferences
- Heuristic upstream detection for skills without declared metadata

## Implementation rough cuts

Suggested phasing for the implementation plan that follows this spec:

| Phase | Scope |
|---|---|
| 0 | Swap arc-relay LLM client from Anthropic (`claude-haiku-4-5`) to OpenAI (`gpt-4o-mini`). Re-implements `internal/llm/client.go` against OpenAI's chat completions API; existing `mcp.OptimizeTools` callers in `internal/web/handlers.go:3363` and `internal/server/http.go:1079` keep working through the unchanged Go interface. Update fixtures + production env var. |
| 1 | Migration `017_skill_upstreams.sql`, `skill_upstreams` repo + Go bindings, push handler accepts upstream metadata, push clears drift fields. No checker yet. |
| 2 | Subtree-hash function, sidecar TOML parser, CLI flags on `arc-sync skill push`. End-to-end push of a skill with upstream metadata. |
| 3 | Drift checker (`internal/skills/checker/`), git fetch + log + hash diff, no LLM. Cron registration. Prometheus metrics. |
| 4 | LLM integration (reuses Phase 0's swapped client), structured output schema, fallback path, retries. |
| 5 | `POST /api/skills/<slug>/check-drift` endpoint, `arc-sync skill check-updates` CLI command, list output extension. |
| 6 | Tests across phases, doc updates (`docs/skills.md` if it exists), config knobs landed in `config.example.toml`. |

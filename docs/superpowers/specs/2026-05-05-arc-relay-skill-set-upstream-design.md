# Arc Relay Skill Set-Upstream — Design

**Status:** Drafted 2026-05-05 to capture a follow-up gap discovered after shipping the dashboard drift surfacing patch (commit `b0cd71d`). Awaiting user review before implementation.

## Goal

Let an admin add, change, or remove the upstream-tracking row on an existing skill **without bumping its version or re-uploading the archive**. Surface this through three layers (HTTP API, `arc-sync` CLI, dashboard form) so it's reachable from automation and from the GUI.

## Motivation

The 2026-04-30 skill update checker design (see `2026-04-30-arc-relay-skill-update-checker-design.md`) wired `skill_upstreams` to be set as a *side effect* of a version upload — the `POST /api/skills/{slug}/versions/{version}` handler reads an `X-Upstream` header and calls `SkillStore.UpsertUpstream`.

That works fine the first time you push a skill *and* know the upstream up-front, but it leaves three gaps:

1. **Skills pushed with `--no-upstream` (or older skills predating the checker) can never start tracking.** To enable tracking today you have to bump the semver and re-upload an identical tarball, which is wrong on its face: nothing has changed about the bytes.
2. **No way to *fix* a wrong upstream pointer.** If the publisher typo'd the path or the source repo moved, today the only fix is another version bump.
3. **Dashboard dead end.** The new "Update tracking" card on `/skills/<slug>` (shipped 2026-05-05) tells the admin "this skill was pushed without upstream tracking" and stops there. There's no in-GUI fix path.

The relay already has `SkillStore.UpsertUpstream` and `SkillStore.ClearUpstream` — what's missing is exposing them outside the upload code path.

## Non-goals

- **Per-version upstream pointers.** `skill_upstreams.skill_id` is the primary key; one row per skill, not per version. Out of scope to change.
- **Cross-repo migrations.** Changing `git_url` is allowed (typo fix, repo move) but resets `last_seen_*` on the next checker run — that's expected.
- **Heuristic auto-detection.** Still strictly opt-in. The user supplies the URL, path, and ref.
- **Non-admin authoring.** Same authorization as upload: admin role OR `skills:write` capability (the `requireCapability` middleware already handles this — see `auth_capabilities.go`).
- **Recipe parity.** Recipes have no upstream concept (see `arc-sync — Skills & Recipes` Outline doc, "Recipes have no update detection" section). Not adding one here.

## Surface area

### 1. HTTP API — new endpoint

```
PUT /api/skills/{slug}/upstream
DELETE /api/skills/{slug}/upstream
```

Authorization: admin role OR API key with `skills:write` capability.

**PUT body** (JSON):

```json
{
  "type": "git",                                           // optional, defaults to "git"
  "git_url": "https://github.com/owner/repo",              // required, non-empty
  "git_subpath": "skills/foo",                             // optional, defaults to "" (repo root)
  "git_ref": "main"                                        // optional, defaults to "HEAD"
}
```

**PUT response** (200):

```json
{
  "skill_id": "...",
  "upstream_type": "git",
  "git_url": "...",
  "git_subpath": "...",
  "git_ref": "...",
  "last_checked_at": null,
  "drift": null,
  "created_at": "...",
  "updated_at": "..."
}
```

Reuses the existing `driftBlockFromUpstream` helper for the `drift` field — null on a freshly-set row, populated once the next checker run completes.

**Status codes:**

| HTTP | Meaning |
|---|---|
| 200 | Upstream set/replaced (PUT) or removed (DELETE) |
| 400 | Body parse error or missing `git_url` |
| 401 | No auth |
| 403 | Admin/capability gate failed |
| 404 | Skill not found |
| 415 | PUT without `Content-Type: application/json` |

**Side effects:**

- PUT calls `SkillStore.UpsertUpstream`. The store method already preserves `last_seen_*` and `drift_*` on conflict — that's the right semantic for re-pointing an existing row to a new ref/path. For a brand-new row, those fields stay NULL until the next checker tick.
- DELETE calls `SkillStore.ClearUpstream`, which also clears `skills.outdated`.

### 2. CLI — new arc-sync subcommands

```
arc-sync skill set-upstream <slug> \
    --git-url URL \
    [--path SUBPATH] \
    [--ref REF]

arc-sync skill clear-upstream <slug>
```

Wired into `cmd/arc-sync/skill.go`'s `runSkill` dispatcher (`set-upstream` / `clear-upstream` cases). Hits the new HTTP endpoints. Mirrors the flag style of `skill push --upstream-git/--upstream-path/--upstream-ref` but with `--git-url/--path/--ref` since there's only one upstream concept here (no need for the disambiguating prefix).

Help text gets a new section in `printSkillUsage()`:

```
  set-upstream <slug> --git-url URL [--path SUBPATH] [--ref REF]
                        Admin-only. Set or replace the upstream-tracking row
                        for an existing skill without re-uploading. Use this
                        to enable drift detection on a skill pushed with
                        --no-upstream, or to fix a typo'd path/ref.
  clear-upstream <slug>
                        Admin-only. Remove upstream tracking for a skill.
                        Future drift checker runs skip it silently.
```

### 3. Dashboard — form on `/skills/<slug>`

Replace the static "no source repo to compare against" message in the "Update tracking" card with an admin-only inline form when no upstream exists, and an "Edit" + "Clear tracking" button pair when one does.

Form POSTs to a new dashboard route `POST /skills/{slug}/upstream` (CSRF-checked, session auth, admin-gated) which calls `SkillStore.UpsertUpstream` directly — same store method as the API; the dashboard handler just does the form-encoded equivalent. Same for `POST /skills/{slug}/upstream/clear`.

After submit, redirect back to `/skills/{slug}` with a flash message, so the user immediately sees the new state in the same card.

## Schema reuse — nothing new

The existing `skill_upstreams` table (migration 017) already supports everything needed. `SkillStore.UpsertUpstream` and `SkillStore.ClearUpstream` are the only store methods called. Migration 018 (capabilities) is also untouched.

## Implementation outline

Estimated change set:

1. **`internal/web/skills_handlers.go`** (~60 lines): new `HandleUpstream` method dispatching on PUT/DELETE, reusing existing `requireCapability("skills:write")` and `driftBlockFromUpstream` helpers. Wire into the route registration in `RegisterRoutes`.
2. **`internal/web/skills_handlers_test.go`** (~150 lines): table-driven tests for happy path PUT, replace-existing PUT, DELETE, 400 (missing url), 403 (non-admin no cap), 404 (unknown slug), 415 (wrong content-type).
3. **`cmd/arc-sync/skill.go`** (~80 lines): two new dispatcher cases, two new `runSkillSetUpstream` / `runSkillClearUpstream` functions following the shape of `runSkillAssign`, plus a new `relay.Client.SetUpstream` / `ClearUpstream` method in `internal/cli/relay/skills.go`.
4. **`cmd/arc-sync/skill_test.go`** (~80 lines): tests with an `httptest` rig covering the same matrix as the HTTP tests.
5. **`internal/web/skills_dashboard.go`** (~50 lines): `HandleUpstreamForm` for POST `/skills/{slug}/upstream` (form-encoded, CSRF, admin-gated). Add to route registration.
6. **`internal/web/templates/skill_detail.html`** (~30 lines): replace the static no-upstream `<p>` with `{{if and .User (eq .User.Role "admin")}}<form>…</form>{{end}}`. When upstream exists, add an "Edit upstream" toggle that reveals the same form pre-filled.
7. **Outline doc patch** (`9tHaD7sMnU`): add a "Changing upstream tracking after the fact" subsection under "How update detection actually works".
8. **`docs/superpowers/plans/`**: not strictly needed for a change this small — direct execution from this design doc is fine.

Total: roughly 400 lines of code + tests, no schema work.

## Test plan

**Unit / handler tests:**
- PUT happy path on a skill with no upstream → 200, row created, `driftBlockFromUpstream(GetUpstream(...))` returns `nil`.
- PUT happy path replacing an existing row → 200, `last_seen_sha` and `drift_*` columns preserved across the upsert (verify via `GetUpstream`).
- PUT with empty `git_url` → 400.
- PUT with `type: "tarball"` → 400 (CHECK constraint enforces git-only; we pre-validate so the response is 400 not 500).
- PUT with no auth → 401; with non-admin/no-cap → 403.
- DELETE on existing → 200, `GetUpstream` returns `nil, nil`, `skills.outdated` cleared.
- DELETE on missing → 404.

**Integration:**
- `arc-sync skill set-upstream <slug> --git-url ... --path ... --ref ...` round-trip against an `httptest.Server` configured with a fake admin API key — assert exit 0 and exact stdout.
- `arc-sync skill clear-upstream <slug>` same shape.

**Manual smoke:**
- Mint a `skills:write` capability key (already supported via the v0.4.0 path), `set-upstream` a skill, `arc-sync skill check-updates <slug>`, observe drift status flow into both `arc-sync skill list --remote` and the dashboard's `/skills` and `/skills/<slug>` pages.

## Open questions

1. **Should DELETE also clear `skills.outdated`?** Yes — if there's no upstream to compare against, a stale "outdated" flag is misleading. `ClearUpstream` already does this; just verifying the test asserts it.
2. **Should PUT trigger an immediate drift check?** Out of scope for v1. Set-upstream + check-updates is two separate calls; that's fine. If automation wants both, run them in sequence.
3. **Should we support `application/x-www-form-urlencoded` on the API endpoint** so curl-without-jq is easier? No — keep the API JSON-only; the dashboard form route handles HTML form encoding separately.
4. **Should the dashboard form expose the type field?** No, hide it. v1 only supports `git` (CHECK constraint) and adding a dropdown with one option is noise. When a second `upstream_type` ships, expose it then.

## Surface comparison with existing flows

| Path to set upstream | Today | After this design |
|---|---|---|
| Initial upload | `arc-sync skill push <dir> --upstream-git ...` (works) | Unchanged |
| Existing skill, no upstream | Bump version + re-upload, OR direct SQL | `arc-sync skill set-upstream <slug>` OR dashboard form |
| Fix typo'd ref/path | Bump version + re-upload | Same CLI / form, replaces in place |
| Disable tracking entirely | `--no-upstream` on next push (but can't easily downgrade today) | `arc-sync skill clear-upstream <slug>` |

## Decision log

- **2026-05-05** — Drafted after shipping `b0cd71d` (dashboard drift surfacing). User hit the no-upstream message in the new GUI card and asked how to fix; the only paths today are version bump or raw SQL. Both are wrong defaults — this spec closes the gap with a single proper endpoint + CLI + form.

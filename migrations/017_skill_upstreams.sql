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

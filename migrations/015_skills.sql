-- Skill repository: centralized Claude Code skill bundles served by the relay.
--
-- Three tables mirror how `servers` + `endpoint_access_tiers` model gated content:
--   skills              - one row per skill, plus visibility + latest-version pointer
--   skill_versions      - one row per uploaded archive (immutable; yank ≠ delete)
--   skill_assignments   - explicit user grants for restricted skills, with
--                         optional version pin (NULL = follow latest)
--
-- Archives live on disk (host volume), not in SQLite — same precedent as the
-- memory pivot keeping bulk content out of the auth-critical DB.

CREATE TABLE IF NOT EXISTS skills (
    id             TEXT PRIMARY KEY,
    slug           TEXT UNIQUE NOT NULL,
    display_name   TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    visibility     TEXT NOT NULL DEFAULT 'restricted'
                       CHECK(visibility IN ('public', 'restricted')),
    latest_version TEXT,
    yanked_at      DATETIME,
    created_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS skill_versions (
    skill_id       TEXT NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    version        TEXT NOT NULL,
    archive_path   TEXT NOT NULL,
    archive_size   INTEGER NOT NULL,
    archive_sha256 TEXT NOT NULL,
    manifest       TEXT NOT NULL DEFAULT '{}',
    yanked_at      DATETIME,
    uploaded_by    TEXT REFERENCES users(id) ON DELETE SET NULL,
    uploaded_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (skill_id, version)
);

CREATE TABLE IF NOT EXISTS skill_assignments (
    skill_id    TEXT NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    version     TEXT,
    assigned_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    assigned_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (skill_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_skill_versions_skill ON skill_versions(skill_id);
CREATE INDEX IF NOT EXISTS idx_skill_assignments_user ON skill_assignments(user_id);

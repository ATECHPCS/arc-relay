-- 018_api_key_capabilities.sql
-- Adds per-API-key capability list so non-admin keys can be granted specific
-- write permissions (e.g. uploading skills) without granting full admin
-- powers. See docs/architecture/api-key-capabilities.md.
--
-- Capabilities are stored as a JSON array of short strings, matching the
-- "<resource>:<verb>" convention used throughout the codebase. Initial verbs
-- recognized by the relay:
--
--   skills:write    upload a new skill version (creates skill row if missing)
--   skills:yank     yank/unyank a version uploaded by THIS api key
--   recipes:write   push a new recipe version
--   recipes:yank    yank a recipe version uploaded by THIS api key
--
-- Admin keys (the user owning the key has role='admin') retain all powers
-- regardless of the capabilities column — capabilities are additive, not a
-- restriction. The admin bypass lives in middleware (requireCapability), not
-- in this schema.
--
-- The `uploaded_by_api_key_id` columns on skill_versions and (future)
-- recipe_versions are how we enforce "yank only your own uploads" for
-- capability-bearing non-admin keys.

ALTER TABLE api_keys ADD COLUMN capabilities TEXT NOT NULL DEFAULT '[]';

ALTER TABLE skill_versions ADD COLUMN uploaded_by_api_key_id TEXT
    REFERENCES api_keys(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_skill_versions_uploaded_by_api_key_id
    ON skill_versions(uploaded_by_api_key_id);

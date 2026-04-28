-- Setup recipes: machine-bootstrap automation for Claude Code clients.
--
-- Unlike `skills` (which stores user-authored skill bytes), recipes describe
-- HOW to install third-party content from upstream sources via the supported
-- `claude plugin` CLI. Two tables mirror the skills shape:
--
--   setup_recipes              one row per recipe; recipe_data holds the
--                              type-specific JSON payload
--   setup_recipe_assignments   per-user grants for restricted recipes
--
-- v1 ships exactly one recipe_type: `claude_plugin`. Other types (git_skill,
-- shell_script) are intentionally rejected by the CHECK constraint so a future
-- migration must explicitly opt them in — important for recipes whose execution
-- model has different security characteristics.

CREATE TABLE IF NOT EXISTS setup_recipes (
    id           TEXT PRIMARY KEY,
    slug         TEXT UNIQUE NOT NULL,
    display_name TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    recipe_type  TEXT NOT NULL CHECK(recipe_type IN ('claude_plugin')),
    recipe_data  TEXT NOT NULL DEFAULT '{}',
    visibility   TEXT NOT NULL DEFAULT 'restricted'
                     CHECK(visibility IN ('public', 'restricted')),
    yanked_at    DATETIME,
    created_by   TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS setup_recipe_assignments (
    recipe_id   TEXT NOT NULL REFERENCES setup_recipes(id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    assigned_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    assigned_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (recipe_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_setup_recipe_assignments_user
    ON setup_recipe_assignments(user_id);

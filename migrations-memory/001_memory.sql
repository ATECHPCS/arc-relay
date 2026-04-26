-- migrations/015_memory.sql
-- Centralized transcript memory: sessions + messages + FTS5 index.
-- Schema mirrors pcvelz/cc-search-chats-plugin so local-plugin UX maps 1:1.

CREATE TABLE IF NOT EXISTS memory_sessions (
    session_id   TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    project_dir  TEXT NOT NULL,
    file_path    TEXT NOT NULL,
    file_mtime   REAL NOT NULL,
    indexed_at   REAL NOT NULL,
    last_seen_at REAL NOT NULL,
    custom_title TEXT,
    platform     TEXT NOT NULL DEFAULT 'claude-code',
    bytes_seen   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_memory_sessions_user
    ON memory_sessions(user_id);

CREATE INDEX IF NOT EXISTS idx_memory_sessions_user_project
    ON memory_sessions(user_id, project_dir);

CREATE TABLE IF NOT EXISTS memory_messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid        TEXT,
    session_id  TEXT NOT NULL REFERENCES memory_sessions(session_id) ON DELETE CASCADE,
    parent_uuid TEXT,
    epoch       INTEGER NOT NULL DEFAULT 0,
    timestamp   TEXT NOT NULL,
    role        TEXT NOT NULL,
    content     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_memory_messages_session
    ON memory_messages(session_id);

CREATE INDEX IF NOT EXISTS idx_memory_messages_session_epoch
    ON memory_messages(session_id, epoch);

CREATE INDEX IF NOT EXISTS idx_memory_messages_uuid
    ON memory_messages(uuid)
    WHERE uuid IS NOT NULL;

CREATE TABLE IF NOT EXISTS memory_compact_events (
    uuid               TEXT PRIMARY KEY,
    session_id         TEXT NOT NULL REFERENCES memory_sessions(session_id) ON DELETE CASCADE,
    epoch              INTEGER NOT NULL,
    timestamp          TEXT NOT NULL,
    trigger_type       TEXT,
    token_count_before INTEGER
);

-- External-content FTS5 — content lives in memory_messages, FTS5 holds only the index.
CREATE VIRTUAL TABLE IF NOT EXISTS memory_messages_fts USING fts5(
    content,
    content='memory_messages',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);

-- Sync triggers
CREATE TRIGGER IF NOT EXISTS memory_messages_ai AFTER INSERT ON memory_messages BEGIN
    INSERT INTO memory_messages_fts(rowid, content) VALUES (new.id, new.content);
END;

CREATE TRIGGER IF NOT EXISTS memory_messages_ad AFTER DELETE ON memory_messages BEGIN
    INSERT INTO memory_messages_fts(memory_messages_fts, rowid, content)
    VALUES ('delete', old.id, old.content);
END;

CREATE TRIGGER IF NOT EXISTS memory_messages_au AFTER UPDATE ON memory_messages BEGIN
    INSERT INTO memory_messages_fts(memory_messages_fts, rowid, content)
    VALUES ('delete', old.id, old.content);
    INSERT INTO memory_messages_fts(rowid, content) VALUES (new.id, new.content);
END;

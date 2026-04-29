-- migrations-memory/002_memory_extractions.sql
-- Phase B: LLM extraction → mem0 with provenance log.
--
-- memory_extractions is a shadow table that records WHICH messages went
-- into WHICH mem0 memories. mem0 itself is dedup-only (no UPDATE merge),
-- so the source-of-truth for "where did this memory come from?" lives
-- here, not in mem0.

ALTER TABLE memory_sessions ADD COLUMN last_extracted_at REAL;

CREATE TABLE IF NOT EXISTS memory_extractions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT NOT NULL REFERENCES memory_sessions(session_id) ON DELETE CASCADE,
    extracted_at    REAL NOT NULL,
    chunk_index     INTEGER NOT NULL,
    chunk_msg_uuids TEXT NOT NULL,           -- JSON array of source message UUIDs
    chunk_chars     INTEGER NOT NULL,
    mem0_memory_ids TEXT NOT NULL DEFAULT '[]',  -- JSON array of mem0 IDs returned
    mem0_count      INTEGER NOT NULL DEFAULT 0,
    error           TEXT                      -- non-null if extraction failed
);

CREATE INDEX IF NOT EXISTS idx_memory_extractions_session
    ON memory_extractions(session_id);

CREATE INDEX IF NOT EXISTS idx_memory_extractions_at
    ON memory_extractions(extracted_at DESC);

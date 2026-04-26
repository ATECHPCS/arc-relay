package store

import (
	"fmt"
)

// MemorySession is one row in memory_sessions — metadata about a transcript file.
// One session_id maps 1:1 to one ~/.claude/projects/.../<uuid>.jsonl file.
type MemorySession struct {
	SessionID   string
	UserID      string
	ProjectDir  string
	FilePath    string
	FileMtime   float64
	IndexedAt   float64
	LastSeenAt  float64
	CustomTitle string
	Platform    string
	BytesSeen   int64
}

// SessionMemoryStore manages memory_sessions rows. Companion to MessageStore.
type SessionMemoryStore struct {
	db *DB
}

// NewSessionMemoryStore mirrors the NewMessageStore and NewServerStore patterns.
func NewSessionMemoryStore(db *DB) *SessionMemoryStore {
	return &SessionMemoryStore{db: db}
}

const upsertSessionSQL = `
INSERT INTO memory_sessions
    (session_id, user_id, project_dir, file_path, file_mtime, indexed_at,
     last_seen_at, custom_title, platform, bytes_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
    file_mtime   = excluded.file_mtime,
    last_seen_at = excluded.last_seen_at,
    bytes_seen   = excluded.bytes_seen,
    custom_title = COALESCE(NULLIF(excluded.custom_title, ''), memory_sessions.custom_title)
`

// Upsert is idempotent on session_id. On conflict it advances mtime/seen/bytes
// and preserves a previously-set CustomTitle when the incoming value is empty.
// user_id, project_dir, file_path, indexed_at, and platform are insert-only —
// changing them retroactively would silently rewrite history.
func (s *SessionMemoryStore) Upsert(m *MemorySession) error {
	_, err := s.db.Exec(upsertSessionSQL,
		m.SessionID, m.UserID, m.ProjectDir, m.FilePath, m.FileMtime,
		m.IndexedAt, m.LastSeenAt, nullableString(m.CustomTitle), m.Platform, m.BytesSeen,
	)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// Get returns the row for sessionID, or sql.ErrNoRows if not present.
// The error is intentionally NOT wrapped — callers can use errors.Is.
func (s *SessionMemoryStore) Get(sessionID string) (*MemorySession, error) {
	row := s.db.QueryRow(`
SELECT session_id, user_id, project_dir, file_path, file_mtime, indexed_at,
       last_seen_at, COALESCE(custom_title, ''), platform, bytes_seen
FROM memory_sessions WHERE session_id = ?`, sessionID)
	var m MemorySession
	err := row.Scan(&m.SessionID, &m.UserID, &m.ProjectDir, &m.FilePath,
		&m.FileMtime, &m.IndexedAt, &m.LastSeenAt, &m.CustomTitle,
		&m.Platform, &m.BytesSeen)
	if err != nil {
		return nil, err // do not wrap so errors.Is(err, sql.ErrNoRows) works
	}
	return &m, nil
}

// ListByUser returns the most-recent-first sessions for userID, capped at limit.
// limit <= 0 or > 200 defaults to 50; max is 200.
func (s *SessionMemoryStore) ListByUser(userID string, limit int) ([]*MemorySession, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`
SELECT session_id, user_id, project_dir, file_path, file_mtime, indexed_at,
       last_seen_at, COALESCE(custom_title, ''), platform, bytes_seen
FROM memory_sessions
WHERE user_id = ?
ORDER BY last_seen_at DESC
LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []*MemorySession
	for rows.Next() {
		var m MemorySession
		if err := rows.Scan(&m.SessionID, &m.UserID, &m.ProjectDir, &m.FilePath,
			&m.FileMtime, &m.IndexedAt, &m.LastSeenAt, &m.CustomTitle,
			&m.Platform, &m.BytesSeen); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// Touch advances the watermark fields (mtime, last_seen_at, bytes_seen) without
// rewriting any metadata. Returns nil for a missing session (no-op semantics) —
// the watcher's flow always Upserts before Touching, but a transient gap should
// not crash it.
func (s *SessionMemoryStore) Touch(sessionID string, mtime float64, bytes int64) error {
	_, err := s.db.Exec(`
UPDATE memory_sessions
SET file_mtime = ?, last_seen_at = ?, bytes_seen = ?
WHERE session_id = ?`, mtime, mtime, bytes, sessionID)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

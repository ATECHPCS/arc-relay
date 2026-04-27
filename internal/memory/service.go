// Package memory orchestrates transcript ingestion: it routes JSONL through
// the appropriate platform parser and persists the resulting rows.
package memory

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/memory/parser"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// IngestRequest is the wire shape posted by the watcher (and by tests).
// UserID is intentionally NOT in this struct — the handler derives it from the
// authenticated user in context. Trusting a client-supplied user_id would be a
// security hole (any API key holder could write into another user's memory).
type IngestRequest struct {
	SessionID  string  `json:"session_id"`
	ProjectDir string  `json:"project_dir"`
	FilePath   string  `json:"file_path"`
	FileMtime  float64 `json:"file_mtime"`
	BytesSeen  int64   `json:"bytes_seen"`
	Platform   string  `json:"platform"`
	// JSONL is the raw transcript bytes. Go's encoding/json marshals []byte as
	// base64 on the wire; the watcher and the handler both rely on that default.
	JSONL []byte `json:"jsonl"`
}

// IngestResponse is returned to the caller on success.
type IngestResponse struct {
	MessagesAdded int   `json:"messages_added"`
	EventsAdded   int   `json:"events_added"`
	BytesSeen     int64 `json:"bytes_seen"`
}

// Service is the seam between HTTP/MCP handlers and the storage layer.
type Service struct {
	sessions *store.SessionMemoryStore
	messages *store.MessageStore
}

// NewService creates a Service backed by the given stores.
func NewService(sessions *store.SessionMemoryStore, messages *store.MessageStore) *Service {
	return &Service{sessions: sessions, messages: messages}
}

// Ingest parses a JSONL chunk under the calling user's identity and persists
// rows. Idempotent: messages with a uuid that already exists are dropped via
// SQLite's unique index on memory_messages.uuid.
func (s *Service) Ingest(userID string, req *IngestRequest) (*IngestResponse, error) {
	if req.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if req.Platform == "" {
		return nil, fmt.Errorf("platform is required")
	}
	p := parser.Get(req.Platform)
	if p == nil {
		return nil, fmt.Errorf("unknown platform %q", req.Platform)
	}

	if err := s.sessions.Upsert(&store.MemorySession{
		SessionID:  req.SessionID,
		UserID:     userID,
		ProjectDir: req.ProjectDir,
		FilePath:   req.FilePath,
		FileMtime:  req.FileMtime,
		IndexedAt:  req.FileMtime,
		LastSeenAt: req.FileMtime,
		Platform:   req.Platform,
		BytesSeen:  req.BytesSeen,
	}); err != nil {
		return nil, fmt.Errorf("upsert session: %w", err)
	}

	msgs, events, err := p.Parse(bytes.NewReader(req.JSONL))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	for _, m := range msgs {
		m.SessionID = req.SessionID
	}
	if err := s.messages.BulkInsert(msgs); err != nil {
		return nil, fmt.Errorf("bulk insert: %w", err)
	}
	// CompactEvents persistence is Phase 4 (LLM observation layer). v1 returns
	// the count for diagnostics but does not write a memory_compact_events row.
	return &IngestResponse{
		MessagesAdded: len(msgs),
		EventsAdded:   len(events),
		BytesSeen:     req.BytesSeen,
	}, nil
}

// Search routes the query through FTS5 BM25 by default, falling back to a Go
// regexp scan when the query contains regex metacharacters and is not wrapped
// in double quotes (escape hatch for users who want a literal match).
//
// Three-tier escalation handles FTS5 syntax errors (e.g. hyphens parsed as NOT,
// colons as column scopes):
//  1. Try FTS5 with the raw query.
//  2. On error, retry as a quoted phrase ("...") which treats all metacharacters
//     literally. Embedded double quotes are doubled per FTS5 phrase-string rules.
//  3. On second error, fall back to Go regex scan. If that also fails, return the
//     original FTS5 error — it is the most informative.
func (s *Service) Search(userID, query string, opts store.SearchOpts) ([]*store.SearchHit, error) {
	if hasRegexMeta(query) {
		return s.messages.SearchRegex(userID, query, opts)
	}
	// First try: raw query as FTS5 input.
	hits, err := s.messages.Search(userID, query, opts)
	if err == nil {
		return hits, nil
	}
	// Second try: wrap as a phrase (handles hyphens, colons, and other FTS5
	// metacharacters that the user didn't intend as syntax).
	quoted := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	slog.Debug("memory search: FTS5 raw query failed, retrying as phrase",
		"query", query, "quoted", quoted, "err", err)
	hits, err2 := s.messages.Search(userID, quoted, opts)
	if err2 == nil {
		return hits, nil
	}
	// Third try: regex fallback.
	slog.Debug("memory search: phrase retry also failed, falling back to regex",
		"query", query, "err", err2)
	hits, err3 := s.messages.SearchRegex(userID, query, opts)
	if err3 != nil {
		// Surface the ORIGINAL FTS5 error since it's the most informative.
		return nil, fmt.Errorf("fts5 + phrase + regex all failed; first error: %w", err)
	}
	return hits, nil
}

// SessionExtract returns the messages of one session, with a user-scope check
// (don't leak the existence of another user's session).
func (s *Service) SessionExtract(userID, sessionID string, fromEpoch int) ([]*store.Message, error) {
	sess, err := s.sessions.Get(sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("session not found")
		}
		return nil, fmt.Errorf("get session: %w", err)
	}
	if sess.UserID != userID {
		// Same error as missing — don't reveal existence to wrong user.
		return nil, fmt.Errorf("session not found")
	}
	return s.messages.GetSession(sessionID, fromEpoch)
}

// Recent lists the calling user's most-recent sessions.
func (s *Service) Recent(userID string, limit int) ([]*store.MemorySession, error) {
	return s.sessions.ListByUser(userID, limit)
}

// Stats is the diagnostic shape returned by HandleStats — global counts,
// not user-scoped (count != content; safe to surface).
type Stats struct {
	DBBytes      int64    `json:"db_bytes"`
	Sessions     int64    `json:"sessions"`
	Messages     int64    `json:"messages"`
	LastIngestAt float64  `json:"last_ingest_at"`
	Platforms    []string `json:"platforms"`
}

// Stats returns DB-level counts + last-ingest timestamp + the parser registry's
// supported platforms. Used by `arc-sync memory stats` and (future) the
// dashboard.
func (s *Service) Stats() (*Stats, error) {
	st := &Stats{Platforms: parser.Platforms()}

	// page_count * page_size — the actual on-disk database size.
	if err := s.messages.DB().QueryRow(
		`SELECT (SELECT page_count FROM pragma_page_count) * (SELECT page_size FROM pragma_page_size)`,
	).Scan(&st.DBBytes); err != nil {
		return nil, fmt.Errorf("db bytes: %w", err)
	}
	if err := s.messages.DB().QueryRow(
		`SELECT count(*) FROM memory_sessions`,
	).Scan(&st.Sessions); err != nil {
		return nil, fmt.Errorf("sessions count: %w", err)
	}
	if err := s.messages.DB().QueryRow(
		`SELECT count(*) FROM memory_messages`,
	).Scan(&st.Messages); err != nil {
		return nil, fmt.Errorf("messages count: %w", err)
	}
	if err := s.messages.DB().QueryRow(
		`SELECT COALESCE(MAX(last_seen_at), 0) FROM memory_sessions`,
	).Scan(&st.LastIngestAt); err != nil {
		return nil, fmt.Errorf("last ingest: %w", err)
	}
	return st, nil
}

// hasRegexMeta detects FTS5-incompatible characters that should route to the
// regex fallback. Quoted strings bypass detection so users can search for
// literal punctuation by quoting it.
func hasRegexMeta(q string) bool {
	if len(q) >= 2 && q[0] == '"' && q[len(q)-1] == '"' {
		return false
	}
	for _, r := range q {
		switch r {
		case '\\', '.', '*', '+', '?', '[', ']', '{', '}', '(', ')', '|', '^', '$':
			return true
		}
	}
	return false
}

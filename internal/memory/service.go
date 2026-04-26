// Package memory orchestrates transcript ingestion: it routes JSONL through
// the appropriate platform parser and persists the resulting rows.
package memory

import (
	"bytes"
	"fmt"

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

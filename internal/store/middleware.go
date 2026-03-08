package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// MiddlewareConfig represents a middleware configuration for a server (or global default).
type MiddlewareConfig struct {
	ID         string          `json:"id"`
	ServerID   *string         `json:"server_id"` // nil = global default
	Middleware string          `json:"middleware"`
	Enabled    bool            `json:"enabled"`
	Config     json.RawMessage `json:"config"`
	Priority   int             `json:"priority"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// MiddlewareEvent is a logged event from middleware processing.
type MiddlewareEvent struct {
	ID            string    `json:"id"`
	Timestamp     time.Time `json:"timestamp"`
	ServerID      string    `json:"server_id"`
	Middleware    string    `json:"middleware"`
	EventType     string    `json:"event_type"` // redacted, blocked, truncated, alert
	Summary       string    `json:"summary"`
	RequestMethod string    `json:"request_method"`
	EndpointName  string    `json:"endpoint_name"`
	UserID        string    `json:"user_id"`
}

// MiddlewareStore handles CRUD for middleware configurations and events.
type MiddlewareStore struct {
	db *DB
}

func NewMiddlewareStore(db *DB) *MiddlewareStore {
	return &MiddlewareStore{db: db}
}

// GetForServer returns the effective middleware configs for a server,
// merging global defaults with server-specific overrides.
func (s *MiddlewareStore) GetForServer(serverID string) ([]*MiddlewareConfig, error) {
	// Get global defaults (server_id IS NULL) and server-specific configs.
	// Server-specific configs override globals for the same middleware name.
	rows, err := s.db.Query(`
		SELECT id, server_id, middleware, enabled, config, priority, created_at, updated_at
		FROM middleware_configs
		WHERE server_id IS NULL OR server_id = ?
		ORDER BY priority ASC, middleware ASC
	`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	globals := make(map[string]*MiddlewareConfig)
	serverConfigs := make(map[string]*MiddlewareConfig)

	for rows.Next() {
		mc := &MiddlewareConfig{}
		var serverIDVal sql.NullString
		if err := rows.Scan(&mc.ID, &serverIDVal, &mc.Middleware, &mc.Enabled, &mc.Config, &mc.Priority, &mc.CreatedAt, &mc.UpdatedAt); err != nil {
			return nil, err
		}
		if serverIDVal.Valid {
			mc.ServerID = &serverIDVal.String
			serverConfigs[mc.Middleware] = mc
		} else {
			globals[mc.Middleware] = mc
		}
	}

	// Merge: server-specific overrides global
	merged := make(map[string]*MiddlewareConfig)
	for name, gc := range globals {
		merged[name] = gc
	}
	for name, sc := range serverConfigs {
		merged[name] = sc
	}

	// Sort by priority
	result := make([]*MiddlewareConfig, 0, len(merged))
	for _, mc := range merged {
		result = append(result, mc)
	}
	// Sort by priority (stable)
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].Priority < result[j-1].Priority; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}

	return result, nil
}

// Get returns a single middleware config by ID.
func (s *MiddlewareStore) Get(id string) (*MiddlewareConfig, error) {
	mc := &MiddlewareConfig{}
	var serverIDVal sql.NullString
	err := s.db.QueryRow(`
		SELECT id, server_id, middleware, enabled, config, priority, created_at, updated_at
		FROM middleware_configs WHERE id = ?
	`, id).Scan(&mc.ID, &serverIDVal, &mc.Middleware, &mc.Enabled, &mc.Config, &mc.Priority, &mc.CreatedAt, &mc.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if serverIDVal.Valid {
		mc.ServerID = &serverIDVal.String
	}
	return mc, nil
}

// Upsert creates or updates a middleware config for a server (or global if serverID is nil).
func (s *MiddlewareStore) Upsert(mc *MiddlewareConfig) error {
	if mc.ID == "" {
		b := make([]byte, 16)
		rand.Read(b)
		mc.ID = hex.EncodeToString(b)
	}
	now := time.Now()
	mc.UpdatedAt = now

	var serverID sql.NullString
	if mc.ServerID != nil {
		serverID = sql.NullString{String: *mc.ServerID, Valid: true}
	}

	_, err := s.db.Exec(`
		INSERT INTO middleware_configs (id, server_id, middleware, enabled, config, priority, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(server_id, middleware) DO UPDATE SET
			enabled = excluded.enabled,
			config = excluded.config,
			priority = excluded.priority,
			updated_at = excluded.updated_at
	`, mc.ID, serverID, mc.Middleware, mc.Enabled, string(mc.Config), mc.Priority, now, now)
	return err
}

// Delete removes a middleware config.
func (s *MiddlewareStore) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM middleware_configs WHERE id = ?", id)
	return err
}

// LogEvent records a middleware event.
func (s *MiddlewareStore) LogEvent(evt *MiddlewareEvent) error {
	if evt.ID == "" {
		b := make([]byte, 16)
		rand.Read(b)
		evt.ID = hex.EncodeToString(b)
	}
	_, err := s.db.Exec(`
		INSERT INTO middleware_events (id, timestamp, server_id, middleware, event_type, summary, request_method, endpoint_name, user_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, evt.ID, time.Now(), evt.ServerID, evt.Middleware, evt.EventType, evt.Summary, evt.RequestMethod, evt.EndpointName, evt.UserID)
	return err
}

// RecentEvents returns recent middleware events for a server (or all if serverID is empty).
func (s *MiddlewareStore) RecentEvents(serverID string, limit int) ([]*MiddlewareEvent, error) {
	var rows *sql.Rows
	var err error
	if serverID != "" {
		rows, err = s.db.Query(`
			SELECT id, timestamp, server_id, middleware, event_type, summary, request_method, endpoint_name, user_id
			FROM middleware_events WHERE server_id = ? ORDER BY timestamp DESC LIMIT ?
		`, serverID, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT id, timestamp, server_id, middleware, event_type, summary, request_method, endpoint_name, user_id
			FROM middleware_events ORDER BY timestamp DESC LIMIT ?
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*MiddlewareEvent
	for rows.Next() {
		e := &MiddlewareEvent{}
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.ServerID, &e.Middleware, &e.EventType, &e.Summary, &e.RequestMethod, &e.EndpointName, &e.UserID); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

// EventCounts returns counts of middleware events by type for a server in the last N hours.
func (s *MiddlewareStore) EventCounts(serverID string, hours int) (map[string]int, error) {
	rows, err := s.db.Query(`
		SELECT event_type, COUNT(*) FROM middleware_events
		WHERE server_id = ? AND timestamp > datetime('now', ?)
		GROUP BY event_type
	`, serverID, fmt.Sprintf("-%d hours", hours))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		counts[eventType] = count
	}
	return counts, nil
}

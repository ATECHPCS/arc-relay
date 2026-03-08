package store

import (
	"time"
)

// SessionStore persists web UI sessions to SQLite so they survive restarts.
type SessionStore struct {
	db *DB
}

func NewSessionStore(db *DB) *SessionStore {
	return &SessionStore{db: db}
}

// Create stores a new session.
func (s *SessionStore) Create(id, userID string, expiresAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`,
		id, userID, expiresAt,
	)
	return err
}

// Get returns the user for a valid (non-expired) session, or nil.
func (s *SessionStore) Get(id string) (*User, time.Time, bool) {
	var user User
	var expiresAt time.Time
	err := s.db.QueryRow(`
		SELECT u.id, u.username, u.role, s.expires_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.id = ? AND s.expires_at > ?`,
		id, time.Now(),
	).Scan(&user.ID, &user.Username, &user.Role, &expiresAt)
	if err != nil {
		return nil, time.Time{}, false
	}
	return &user, expiresAt, true
}

// Delete removes a session (logout).
func (s *SessionStore) Delete(id string) {
	s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
}

// Cleanup removes all expired sessions.
func (s *SessionStore) Cleanup() {
	s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now())
}

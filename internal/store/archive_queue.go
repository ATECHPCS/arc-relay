package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

const sqliteDateFmt = "2006-01-02 15:04:05"

func sqliteTime(t time.Time) string {
	return t.UTC().Format(sqliteDateFmt)
}

// ArchiveQueueItem represents a queued archive payload awaiting delivery.
type ArchiveQueueItem struct {
	ID            string
	ServerID      string
	CreatedAt     time.Time
	Payload       string
	URL           string
	AuthType      string
	AuthValue     string
	APIKeyHeader  string
	Status        string // "pending" or "hold"
	Attempts      int
	NextAttemptAt time.Time
	LastAttemptAt *time.Time
	LastError     string
}

// ArchiveQueueStatus summarizes the queue state for display.
type ArchiveQueueStatus struct {
	PendingCount int    `json:"pending_count"`
	HoldCount    int    `json:"hold_count"`
	TotalCount   int    `json:"total_count"`
	LastError    string `json:"last_error,omitempty"`
	LastErrorAt  string `json:"last_error_at,omitempty"`
}

// ArchiveQueueStore provides CRUD for the archive_queue table.
type ArchiveQueueStore struct {
	db        *DB
	encryptor *ConfigEncryptor
}

// NewArchiveQueueStore creates a new ArchiveQueueStore.
// The encryptor is used to encrypt/decrypt auth_value at rest.
func NewArchiveQueueStore(db *DB, encryptor *ConfigEncryptor) *ArchiveQueueStore {
	return &ArchiveQueueStore{db: db, encryptor: encryptor}
}

// Enqueue inserts a new payload into the queue for immediate delivery.
func (s *ArchiveQueueStore) Enqueue(item *ArchiveQueueItem) error {
	if item.ID == "" {
		id, err := generateID()
		if err != nil {
			return err
		}
		item.ID = id
	}
	// Encrypt auth_value before storing
	authValue := item.AuthValue
	if authValue != "" && s.encryptor != nil {
		encrypted, err := s.encryptor.Encrypt([]byte(authValue))
		if err != nil {
			return fmt.Errorf("encrypting auth_value: %w", err)
		}
		authValue = string(encrypted)
	}
	now := sqliteTime(time.Now())
	_, err := s.db.Exec(`
		INSERT INTO archive_queue (id, server_id, created_at, payload, url, auth_type, auth_value, api_key_header, status, attempts, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?)
	`, item.ID, item.ServerID, now, item.Payload, item.URL, item.AuthType, authValue, item.APIKeyHeader, now)
	return err
}

// DequeueDue returns up to limit items that are due for delivery attempt.
func (s *ArchiveQueueStore) DequeueDue(limit int) ([]*ArchiveQueueItem, error) {
	now := sqliteTime(time.Now())
	rows, err := s.db.Query(`
		SELECT id, server_id, created_at, payload, url, auth_type, auth_value, api_key_header, status, attempts, next_attempt_at, last_attempt_at, last_error
		FROM archive_queue
		WHERE status = 'pending' AND next_attempt_at <= ?
		ORDER BY next_attempt_at ASC, created_at ASC
		LIMIT ?
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []*ArchiveQueueItem
	for rows.Next() {
		item := &ArchiveQueueItem{}
		var serverID sql.NullString
		var lastAttemptAt sql.NullTime
		var lastError sql.NullString
		if err := rows.Scan(
			&item.ID, &serverID, &item.CreatedAt, &item.Payload,
			&item.URL, &item.AuthType, &item.AuthValue, &item.APIKeyHeader,
			&item.Status, &item.Attempts, &item.NextAttemptAt,
			&lastAttemptAt, &lastError,
		); err != nil {
			return nil, err
		}
		if serverID.Valid {
			item.ServerID = serverID.String
		}
		if lastAttemptAt.Valid {
			item.LastAttemptAt = &lastAttemptAt.Time
		}
		if lastError.Valid {
			item.LastError = lastError.String
		}
		// Decrypt auth_value if encrypted
		if item.AuthValue != "" && s.encryptor != nil {
			decrypted, err := s.encryptor.Decrypt([]byte(item.AuthValue))
			if err != nil {
				// Log and skip this row rather than blocking the entire batch.
				// This handles key rotation or corruption gracefully.
				slog.Error("archive queue: cannot decrypt auth_value, holding item", "item_id", item.ID, "error", err)
				_ = s.MarkHold(item.ID, "decrypt failed: "+err.Error())
				continue
			}
			item.AuthValue = string(decrypted)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// MarkDelivered removes a successfully delivered item from the queue.
func (s *ArchiveQueueStore) MarkDelivered(id string) error {
	_, err := s.db.Exec(`DELETE FROM archive_queue WHERE id = ?`, id)
	return err
}

// Reschedule updates a failed item for retry at the specified time.
func (s *ArchiveQueueStore) Reschedule(id string, nextAttempt time.Time, errMsg string) error {
	_, err := s.db.Exec(`
		UPDATE archive_queue
		SET attempts = attempts + 1, next_attempt_at = ?, last_attempt_at = ?, last_error = ?
		WHERE id = ?
	`, sqliteTime(nextAttempt), sqliteTime(time.Now()), errMsg, id)
	return err
}

// MarkHold moves an item to hold status for manual retry.
func (s *ArchiveQueueStore) MarkHold(id string, errMsg string) error {
	now := sqliteTime(time.Now())
	_, err := s.db.Exec(`
		UPDATE archive_queue
		SET status = 'hold', attempts = attempts + 1, last_attempt_at = ?, last_error = ?
		WHERE id = ?
	`, now, errMsg, id)
	return err
}

// RetryHeld resets all held items to pending for immediate retry.
func (s *ArchiveQueueStore) RetryHeld() (int64, error) {
	now := sqliteTime(time.Now())
	result, err := s.db.Exec(`
		UPDATE archive_queue
		SET status = 'pending', next_attempt_at = ?, attempts = 0
		WHERE status = 'hold'
	`, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Status returns an aggregate summary of the queue.
func (s *ArchiveQueueStore) Status() (*ArchiveQueueStatus, error) {
	st := &ArchiveQueueStatus{}

	// Counts by status
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM archive_queue GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		switch status {
		case "pending":
			st.PendingCount = count
		case "hold":
			st.HoldCount = count
		}
		st.TotalCount += count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Last error
	var lastError sql.NullString
	var lastErrorAt sql.NullString
	err = s.db.QueryRow(`
		SELECT last_error, last_attempt_at
		FROM archive_queue
		WHERE last_error IS NOT NULL AND last_error != ''
		ORDER BY last_attempt_at DESC
		LIMIT 1
	`).Scan(&lastError, &lastErrorAt)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if lastError.Valid {
		st.LastError = lastError.String
	}
	if lastErrorAt.Valid {
		st.LastErrorAt = lastErrorAt.String
	}

	return st, nil
}

// StatusForServer returns queue status filtered to a specific server.
func (s *ArchiveQueueStore) StatusForServer(serverID string) (*ArchiveQueueStatus, error) {
	st := &ArchiveQueueStatus{}

	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM archive_queue WHERE server_id = ? GROUP BY status`, serverID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		switch status {
		case "pending":
			st.PendingCount = count
		case "hold":
			st.HoldCount = count
		}
		st.TotalCount += count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return st, nil
}

// Prune removes old items (both delivered and held) older than the given duration.
func (s *ArchiveQueueStore) Prune(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	result, err := s.db.Exec(`DELETE FROM archive_queue WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MarshalJSON makes ArchiveQueueStatus JSON-serializable for API responses.
func (s *ArchiveQueueStatus) MarshalJSON() ([]byte, error) {
	type Alias ArchiveQueueStatus
	return json.Marshal((*Alias)(s))
}

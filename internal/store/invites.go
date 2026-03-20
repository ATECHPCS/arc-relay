package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// InviteToken represents a one-time token for CLI onboarding.
type InviteToken struct {
	ID         string    `json:"id"`
	TokenHash  string    `json:"-"`
	UserID     string    `json:"user_id"`
	Username   string    `json:"username,omitempty"` // populated on read, not stored
	ProfileID  *string   `json:"profile_id,omitempty"`
	CreatedBy  string    `json:"created_by"`
	ExpiresAt  time.Time `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
	Status     string    `json:"status"`
}

// InviteStore manages invite tokens.
type InviteStore struct {
	db *DB
}

func NewInviteStore(db *DB) *InviteStore {
	return &InviteStore{db: db}
}

func hashInviteToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Create generates a new invite token for a user. Returns the raw token (shown once).
func (s *InviteStore) Create(userID string, profileID *string, createdBy string, expiresAt time.Time) (string, *InviteToken, error) {
	rawToken := uuid.New().String()
	tokenHash := hashInviteToken(rawToken)

	t := &InviteToken{
		ID:        uuid.New().String(),
		TokenHash: tokenHash,
		UserID:    userID,
		ProfileID: profileID,
		CreatedBy: createdBy,
		ExpiresAt: expiresAt,
		Status:    "pending",
	}

	_, err := s.db.Exec(`
		INSERT INTO invite_tokens (id, token_hash, user_id, profile_id, created_by, expires_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.TokenHash, t.UserID, t.ProfileID, t.CreatedBy, t.ExpiresAt, t.Status,
	)
	if err != nil {
		return "", nil, fmt.Errorf("creating invite token: %w", err)
	}
	return rawToken, t, nil
}

// ValidateAndConsume atomically checks a raw token, marks it used, and returns the invite details.
// The UPDATE uses WHERE status='pending' AND expires_at > now to prevent races.
// Returns nil if invalid, expired, or already used.
func (s *InviteStore) ValidateAndConsume(rawToken string) (*InviteToken, error) {
	tokenHash := hashInviteToken(rawToken)
	now := time.Now()

	// Atomically claim the token: only one concurrent caller can succeed
	// because the WHERE clause ensures only a pending, non-expired token matches.
	result, err := s.db.Exec(`
		UPDATE invite_tokens SET status = 'used', used_at = ?
		WHERE token_hash = ? AND status = 'pending' AND expires_at > ?`,
		now, tokenHash, now,
	)
	if err != nil {
		return nil, fmt.Errorf("consuming invite token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("checking invite token update: %w", err)
	}
	if affected == 0 {
		// Token doesn't exist, is expired, or already used
		// Mark any expired tokens while we're here
		s.db.Exec("UPDATE invite_tokens SET status = 'expired' WHERE token_hash = ? AND status = 'pending' AND expires_at <= ?", tokenHash, now)
		return nil, nil
	}

	// Token was claimed - now read the full record
	t := &InviteToken{}
	var storedHash string
	var profileID sql.NullString
	err = s.db.QueryRow(`
		SELECT id, token_hash, user_id, profile_id, created_by, expires_at, used_at, status
		FROM invite_tokens WHERE token_hash = ?`, tokenHash,
	).Scan(&t.ID, &storedHash, &t.UserID, &profileID, &t.CreatedBy, &t.ExpiresAt, &t.UsedAt, &t.Status)
	if err != nil {
		return nil, fmt.Errorf("reading consumed invite token: %w", err)
	}

	// Constant-time comparison (defense in depth)
	if subtle.ConstantTimeCompare([]byte(tokenHash), []byte(storedHash)) != 1 {
		return nil, nil
	}

	if profileID.Valid {
		t.ProfileID = &profileID.String
	}

	return t, nil
}

// ListForUser returns all invite tokens created for a specific user.
func (s *InviteStore) ListForUser(userID string) ([]*InviteToken, error) {
	rows, err := s.db.Query(`
		SELECT it.id, it.user_id, u.username, it.profile_id, it.created_by, it.expires_at, it.used_at, it.status
		FROM invite_tokens it
		JOIN users u ON it.user_id = u.id
		WHERE it.user_id = ?
		ORDER BY it.expires_at DESC`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing invite tokens: %w", err)
	}
	defer rows.Close()
	return scanInviteTokens(rows)
}

// ListAll returns all invite tokens (admin view).
func (s *InviteStore) ListAll() ([]*InviteToken, error) {
	rows, err := s.db.Query(`
		SELECT it.id, it.user_id, u.username, it.profile_id, it.created_by, it.expires_at, it.used_at, it.status
		FROM invite_tokens it
		JOIN users u ON it.user_id = u.id
		ORDER BY it.expires_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing all invite tokens: %w", err)
	}
	defer rows.Close()
	return scanInviteTokens(rows)
}

func scanInviteTokens(rows *sql.Rows) ([]*InviteToken, error) {
	var tokens []*InviteToken
	for rows.Next() {
		t := &InviteToken{}
		var profileID sql.NullString
		if err := rows.Scan(&t.ID, &t.UserID, &t.Username, &profileID, &t.CreatedBy, &t.ExpiresAt, &t.UsedAt, &t.Status); err != nil {
			return nil, fmt.Errorf("scanning invite token: %w", err)
		}
		if profileID.Valid {
			t.ProfileID = &profileID.String
		}
		tokens = append(tokens, t)
	}
	return tokens, nil
}

// Delete removes an invite token.
func (s *InviteStore) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM invite_tokens WHERE id = ?", id)
	return err
}

// CleanupExpired marks expired pending tokens.
func (s *InviteStore) CleanupExpired() error {
	_, err := s.db.Exec(`
		UPDATE invite_tokens SET status = 'expired'
		WHERE status = 'pending' AND expires_at < ?`, time.Now())
	return err
}

// PendingCountForUser returns the number of pending invite tokens for a user.
func (s *InviteStore) PendingCountForUser(userID string) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM invite_tokens
		WHERE user_id = ? AND status = 'pending' AND expires_at > ?`,
		userID, time.Now(),
	).Scan(&count)
	return count, err
}

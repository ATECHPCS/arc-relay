package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	AccessLevel  string    `json:"access_level"`
	CreatedAt    time.Time `json:"created_at"`
}

type APIKey struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	KeyHash   string    `json:"-"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	Revoked   bool      `json:"revoked"`
}

type UserStore struct {
	db *DB
}

func NewUserStore(db *DB) *UserStore {
	return &UserStore{db: db}
}

func (s *UserStore) Create(username, password, role string) (*User, error) {
	return s.CreateWithAccessLevel(username, password, role, "")
}

func (s *UserStore) CreateWithAccessLevel(username, password, role, accessLevel string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}

	// Force admin access level for admin role
	if role == "admin" {
		accessLevel = "admin"
	}
	if accessLevel == "" {
		accessLevel = "write"
	}

	user := &User{
		ID:           uuid.New().String(),
		Username:     username,
		PasswordHash: string(hash),
		Role:         role,
		AccessLevel:  accessLevel,
		CreatedAt:    time.Now(),
	}

	_, err = s.db.Exec(`
		INSERT INTO users (id, username, password_hash, role, access_level, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, user.PasswordHash, user.Role, user.AccessLevel, user.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("creating user: %w", err)
	}
	return user, nil
}

func (s *UserStore) Authenticate(username, password string) (*User, error) {
	user := &User{}
	err := s.db.QueryRow(`
		SELECT id, username, password_hash, role, access_level, created_at
		FROM users WHERE username = ?`, username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role, &user.AccessLevel, &user.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, nil
	}

	return user, nil
}

func (s *UserStore) Get(id string) (*User, error) {
	user := &User{}
	err := s.db.QueryRow(`
		SELECT id, username, password_hash, role, access_level, created_at
		FROM users WHERE id = ?`, id,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role, &user.AccessLevel, &user.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting user: %w", err)
	}
	return user, nil
}

func (s *UserStore) GetByUsername(username string) (*User, error) {
	user := &User{}
	err := s.db.QueryRow(`
		SELECT id, username, password_hash, role, access_level, created_at
		FROM users WHERE username = ?`, username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role, &user.AccessLevel, &user.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting user by username: %w", err)
	}
	return user, nil
}

func (s *UserStore) List() ([]*User, error) {
	rows, err := s.db.Query(`SELECT id, username, password_hash, role, access_level, created_at FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.AccessLevel, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning user: %w", err)
		}
		users = append(users, u)
	}
	return users, nil
}

func (s *UserStore) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM users WHERE id = ?", id)
	return err
}

// EnsureAdmin creates the default admin user if no users exist.
// Also ensures existing admin users have access_level = 'admin'.
func (s *UserStore) EnsureAdmin(password string) error {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return fmt.Errorf("counting users: %w", err)
	}
	if count > 0 {
		// Ensure all admin-role users have admin access level
		s.db.Exec(`UPDATE users SET access_level = 'admin' WHERE role = 'admin' AND access_level != 'admin'`)
		return nil
	}
	_, err := s.Create("admin", password, "admin")
	return err
}

// API Key operations

func hashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// CreateAPIKey generates a new API key and returns it (plaintext shown once).
func (s *UserStore) CreateAPIKey(userID, name string) (string, *APIKey, error) {
	rawKey := uuid.New().String() // the plaintext key
	keyHash := hashAPIKey(rawKey)

	ak := &APIKey{
		ID:        uuid.New().String(),
		UserID:    userID,
		KeyHash:   keyHash,
		Name:      name,
		CreatedAt: time.Now(),
	}

	_, err := s.db.Exec(`
		INSERT INTO api_keys (id, user_id, key_hash, name, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		ak.ID, ak.UserID, ak.KeyHash, ak.Name, ak.CreatedAt,
	)
	if err != nil {
		return "", nil, fmt.Errorf("creating api key: %w", err)
	}
	return rawKey, ak, nil
}

// ValidateAPIKey checks a raw API key and returns the associated user.
func (s *UserStore) ValidateAPIKey(rawKey string) (*User, error) {
	keyHash := hashAPIKey(rawKey)

	var userID string
	var storedHash string
	var revoked bool
	err := s.db.QueryRow(`
		SELECT user_id, key_hash, revoked FROM api_keys WHERE key_hash = ?`, keyHash,
	).Scan(&userID, &storedHash, &revoked)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up api key: %w", err)
	}

	// Constant-time comparison
	if subtle.ConstantTimeCompare([]byte(keyHash), []byte(storedHash)) != 1 {
		return nil, nil
	}
	if revoked {
		return nil, nil
	}

	// Update last_used
	s.db.Exec("UPDATE api_keys SET last_used = ? WHERE key_hash = ?", time.Now(), keyHash)

	return s.Get(userID)
}

func (s *UserStore) ListAPIKeys(userID string) ([]*APIKey, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, name, created_at, last_used, revoked
		FROM api_keys WHERE user_id = ? ORDER BY created_at`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	defer rows.Close()

	var keys []*APIKey
	for rows.Next() {
		k := &APIKey{}
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.CreatedAt, &k.LastUsed, &k.Revoked); err != nil {
			return nil, fmt.Errorf("scanning api key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *UserStore) RevokeAPIKey(id string) error {
	_, err := s.db.Exec("UPDATE api_keys SET revoked = TRUE WHERE id = ?", id)
	return err
}

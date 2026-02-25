package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type ServerType string

const (
	ServerTypeStdio  ServerType = "stdio"
	ServerTypeHTTP   ServerType = "http"
	ServerTypeRemote ServerType = "remote"
)

type ServerStatus string

const (
	StatusStopped  ServerStatus = "stopped"
	StatusStarting ServerStatus = "starting"
	StatusRunning  ServerStatus = "running"
	StatusError    ServerStatus = "error"
)

type Server struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	DisplayName string          `json:"display_name"`
	ServerType  ServerType      `json:"server_type"`
	Config      json.RawMessage `json:"config"`
	Status      ServerStatus    `json:"status"`
	ErrorMsg    string          `json:"error_msg,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// StdioConfig holds config for Docker-managed stdio servers.
type StdioConfig struct {
	Image      string            `json:"image"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// HTTPConfig holds config for Docker-managed or external HTTP servers.
type HTTPConfig struct {
	Image       string            `json:"image,omitempty"`
	Port        int               `json:"port,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	HealthCheck string            `json:"health_check,omitempty"`
	URL         string            `json:"url,omitempty"` // for external HTTP servers
}

// RemoteConfig holds config for remote servers.
type RemoteConfig struct {
	URL  string     `json:"url"`
	Auth RemoteAuth `json:"auth"`
}

type RemoteAuth struct {
	Type         string `json:"type"` // "none", "private_url", "bearer", "api_key", "oauth"
	Token        string `json:"token,omitempty"`
	HeaderName   string `json:"header_name,omitempty"` // for api_key type
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	AuthURL      string `json:"auth_url,omitempty"`
	TokenURL     string `json:"token_url,omitempty"`
	Scopes       string `json:"scopes,omitempty"`
}

type ServerStore struct {
	db *DB
}

func NewServerStore(db *DB) *ServerStore {
	return &ServerStore{db: db}
}

func (s *ServerStore) Create(srv *Server) error {
	if srv.ID == "" {
		srv.ID = uuid.New().String()
	}
	srv.Status = StatusStopped
	srv.CreatedAt = time.Now()
	srv.UpdatedAt = time.Now()

	_, err := s.db.Exec(`
		INSERT INTO servers (id, name, display_name, server_type, config, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		srv.ID, srv.Name, srv.DisplayName, srv.ServerType, srv.Config, srv.Status, srv.CreatedAt, srv.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("creating server: %w", err)
	}
	return nil
}

func (s *ServerStore) Get(id string) (*Server, error) {
	srv := &Server{}
	err := s.db.QueryRow(`
		SELECT id, name, display_name, server_type, config, status, COALESCE(error_msg, ''), created_at, updated_at
		FROM servers WHERE id = ?`, id,
	).Scan(&srv.ID, &srv.Name, &srv.DisplayName, &srv.ServerType, &srv.Config, &srv.Status, &srv.ErrorMsg, &srv.CreatedAt, &srv.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting server: %w", err)
	}
	return srv, nil
}

func (s *ServerStore) GetByName(name string) (*Server, error) {
	srv := &Server{}
	err := s.db.QueryRow(`
		SELECT id, name, display_name, server_type, config, status, COALESCE(error_msg, ''), created_at, updated_at
		FROM servers WHERE name = ?`, name,
	).Scan(&srv.ID, &srv.Name, &srv.DisplayName, &srv.ServerType, &srv.Config, &srv.Status, &srv.ErrorMsg, &srv.CreatedAt, &srv.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting server by name: %w", err)
	}
	return srv, nil
}

func (s *ServerStore) List() ([]*Server, error) {
	rows, err := s.db.Query(`
		SELECT id, name, display_name, server_type, config, status, COALESCE(error_msg, ''), created_at, updated_at
		FROM servers ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("listing servers: %w", err)
	}
	defer rows.Close()

	var servers []*Server
	for rows.Next() {
		srv := &Server{}
		if err := rows.Scan(&srv.ID, &srv.Name, &srv.DisplayName, &srv.ServerType, &srv.Config, &srv.Status, &srv.ErrorMsg, &srv.CreatedAt, &srv.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning server: %w", err)
		}
		servers = append(servers, srv)
	}
	return servers, nil
}

func (s *ServerStore) Update(srv *Server) error {
	srv.UpdatedAt = time.Now()
	_, err := s.db.Exec(`
		UPDATE servers SET name = ?, display_name = ?, server_type = ?, config = ?, status = ?, error_msg = ?, updated_at = ?
		WHERE id = ?`,
		srv.Name, srv.DisplayName, srv.ServerType, srv.Config, srv.Status, srv.ErrorMsg, srv.UpdatedAt, srv.ID,
	)
	if err != nil {
		return fmt.Errorf("updating server: %w", err)
	}
	return nil
}

func (s *ServerStore) UpdateStatus(id string, status ServerStatus, errMsg string) error {
	_, err := s.db.Exec(`
		UPDATE servers SET status = ?, error_msg = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, time.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("updating server status: %w", err)
	}
	return nil
}

func (s *ServerStore) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM servers WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("deleting server: %w", err)
	}
	return nil
}

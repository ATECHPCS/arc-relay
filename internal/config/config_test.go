package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddr(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 9090},
	}
	got := cfg.Addr()
	want := "127.0.0.1:9090"
	if got != want {
		t.Errorf("Addr() = %q, want %q", got, want)
	}
}

func TestPublicBaseURL(t *testing.T) {
	t.Run("returns BaseURL when set", func(t *testing.T) {
		cfg := &Config{
			Server: ServerConfig{Port: 8080, BaseURL: "https://mcp.example.com"},
		}
		got := cfg.PublicBaseURL()
		want := "https://mcp.example.com"
		if got != want {
			t.Errorf("PublicBaseURL() = %q, want %q", got, want)
		}
	})

	t.Run("falls back to localhost", func(t *testing.T) {
		cfg := &Config{
			Server: ServerConfig{Port: 3000},
		}
		got := cfg.PublicBaseURL()
		want := "http://localhost:3000"
		if got != want {
			t.Errorf("PublicBaseURL() = %q, want %q", got, want)
		}
	})
}

func TestLoad(t *testing.T) {
	t.Run("defaults with empty path", func(t *testing.T) {
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Server.Host != "0.0.0.0" {
			t.Errorf("Server.Host = %q, want %q", cfg.Server.Host, "0.0.0.0")
		}
		if cfg.Server.Port != 8080 {
			t.Errorf("Server.Port = %d, want %d", cfg.Server.Port, 8080)
		}
		if cfg.Database.Path != "mcp-wrangler.db" {
			t.Errorf("Database.Path = %q, want %q", cfg.Database.Path, "mcp-wrangler.db")
		}
		if cfg.Docker.Network != "mcp-wrangler" {
			t.Errorf("Docker.Network = %q, want %q", cfg.Docker.Network, "mcp-wrangler")
		}
	})

	t.Run("loads TOML file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		content := `
[server]
host = "10.0.0.1"
port = 9999

[database]
path = "/tmp/test.db"
`
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("writing config file: %v", err)
		}

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Server.Host != "10.0.0.1" {
			t.Errorf("Server.Host = %q, want %q", cfg.Server.Host, "10.0.0.1")
		}
		if cfg.Server.Port != 9999 {
			t.Errorf("Server.Port = %d, want %d", cfg.Server.Port, 9999)
		}
		if cfg.Database.Path != "/tmp/test.db" {
			t.Errorf("Database.Path = %q, want %q", cfg.Database.Path, "/tmp/test.db")
		}
	})

	t.Run("env var overrides", func(t *testing.T) {
		t.Setenv("MCP_WRANGLER_PORT", "4444")
		t.Setenv("MCP_WRANGLER_BASE_URL", "https://override.example.com")
		t.Setenv("MCP_WRANGLER_DB_PATH", "/override/db.sqlite")
		t.Setenv("MCP_WRANGLER_ENCRYPTION_KEY", "secret-key")
		t.Setenv("MCP_WRANGLER_SESSION_SECRET", "session-secret")
		t.Setenv("MCP_WRANGLER_ADMIN_PASSWORD", "admin-pass")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Server.Port != 4444 {
			t.Errorf("Server.Port = %d, want %d", cfg.Server.Port, 4444)
		}
		if cfg.Server.BaseURL != "https://override.example.com" {
			t.Errorf("Server.BaseURL = %q, want %q", cfg.Server.BaseURL, "https://override.example.com")
		}
		if cfg.Database.Path != "/override/db.sqlite" {
			t.Errorf("Database.Path = %q, want %q", cfg.Database.Path, "/override/db.sqlite")
		}
		if cfg.Encryption.Key != "secret-key" {
			t.Errorf("Encryption.Key = %q, want %q", cfg.Encryption.Key, "secret-key")
		}
		if cfg.Auth.SessionSecret != "session-secret" {
			t.Errorf("Auth.SessionSecret = %q, want %q", cfg.Auth.SessionSecret, "session-secret")
		}
		if cfg.Auth.AdminPassword != "admin-pass" {
			t.Errorf("Auth.AdminPassword = %q, want %q", cfg.Auth.AdminPassword, "admin-pass")
		}
	})

	t.Run("missing file error", func(t *testing.T) {
		_, err := Load("/nonexistent/config.toml")
		if err == nil {
			t.Error("Load() should return error for missing file")
		}
	})
}

package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server     ServerConfig     `toml:"server"`
	Database   DatabaseConfig   `toml:"database"`
	Docker     DockerConfig     `toml:"docker"`
	Encryption EncryptionConfig `toml:"encryption"`
	Auth       AuthConfig       `toml:"auth"`
	SentryDSN  string           `toml:"sentry_dsn"`
}

type ServerConfig struct {
	Host    string `toml:"host"`
	Port    int    `toml:"port"`
	BaseURL string `toml:"base_url"`
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

type DockerConfig struct {
	Socket  string `toml:"socket"`
	Network string `toml:"network"`
}

type EncryptionConfig struct {
	Key string `toml:"key"`
}

type AuthConfig struct {
	SessionSecret string `toml:"session_secret"`
	AdminPassword string `toml:"admin_password"`
}

func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// PublicBaseURL returns the externally-reachable base URL for this server.
// Used to construct OAuth callback URLs.
func (c *Config) PublicBaseURL() string {
	if c.Server.BaseURL != "" {
		return c.Server.BaseURL
	}
	return fmt.Sprintf("http://localhost:%d", c.Server.Port)
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Database: DatabaseConfig{
			Path: "mcp-wrangler.db",
		},
		Docker: DockerConfig{
			Socket:  "unix:///var/run/docker.sock",
			Network: "mcp-wrangler",
		},
	}

	if path != "" {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("loading config %s: %w", path, err)
		}
	}

	// Environment variable overrides
	if v := os.Getenv("MCP_WRANGLER_ENCRYPTION_KEY"); v != "" {
		cfg.Encryption.Key = v
	}
	if v := os.Getenv("MCP_WRANGLER_SESSION_SECRET"); v != "" {
		cfg.Auth.SessionSecret = v
	}
	if v := os.Getenv("MCP_WRANGLER_ADMIN_PASSWORD"); v != "" {
		cfg.Auth.AdminPassword = v
	}
	if v := os.Getenv("MCP_WRANGLER_DB_PATH"); v != "" {
		cfg.Database.Path = v
	}
	if v := os.Getenv("MCP_WRANGLER_BASE_URL"); v != "" {
		cfg.Server.BaseURL = v
	}
	if v := os.Getenv("MCP_WRANGLER_SENTRY_DSN"); v != "" {
		cfg.SentryDSN = v
	}
	if v := os.Getenv("MCP_WRANGLER_PORT"); v != "" {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil {
			cfg.Server.Port = port
		}
	}

	return cfg, nil
}

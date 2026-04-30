package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server     ServerConfig     `toml:"server"`
	Database   DatabaseConfig   `toml:"database"`
	Docker     DockerConfig     `toml:"docker"`
	Encryption EncryptionConfig `toml:"encryption"`
	Auth       AuthConfig       `toml:"auth"`
	LLM        LLMConfig        `toml:"llm"`
	Skills     SkillsConfig     `toml:"skills"`
	SentryDSN  string           `toml:"sentry_dsn"`
	LogLevel   string           `toml:"log_level"`
}

// SkillsConfig holds the on-disk location for skill bundle archives. Defaults
// to "<dataDir>/skills" where dataDir is the directory containing Database.Path.
// Override via TOML [skills] bundles_dir or env ARC_RELAY_SKILLS_DIR.
type SkillsConfig struct {
	BundlesDir string              `toml:"bundles_dir"`
	Checker    SkillsCheckerConfig `toml:"checker"`
}

// SkillsCheckerConfig configures the upstream-update checker that periodically
// scans skill upstreams for drift.
//
// LLMModel is an optional per-checker override. When empty, classification
// uses whatever model the relay's shared llm.Client was constructed with
// (which in turn defaults to gpt-4o-mini). The field exists so operators can
// pin a different model for skill-drift classification without touching the
// global LLM model used elsewhere.
//
// LLMPerFileMaxBytes is currently unused — the LLM classifier only sees the
// pre-truncated `git diff --stat` summary (governed by LLMDiffMaxBytes), not
// individual file diffs. The field is reserved for future per-file truncation
// in the LLM prompt builder so operators can set it ahead of that change
// without rotating their config later.
type SkillsCheckerConfig struct {
	Enabled            bool          `toml:"enabled"`
	Interval           time.Duration `toml:"interval"`
	UpstreamCacheDir   string        `toml:"upstream_cache_dir"`
	GitCloneTimeout    time.Duration `toml:"git_clone_timeout"`
	LLMModel           string        `toml:"llm_model"`
	LLMDiffMaxBytes    int           `toml:"llm_diff_max_bytes"`
	LLMPerFileMaxBytes int           `toml:"llm_per_file_max_bytes"`
}

type LLMConfig struct {
	APIKey string `toml:"api_key"`
	Model  string `toml:"model"`
}

type ServerConfig struct {
	Host    string `toml:"host"`
	Port    int    `toml:"port"`
	BaseURL string `toml:"base_url"`
}

type DatabaseConfig struct {
	Path       string `toml:"path"`
	MemoryPath string `toml:"memory_path"`
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
			Path: "arc-relay.db",
		},
		Docker: DockerConfig{
			Socket:  "unix:///var/run/docker.sock",
			Network: "arc-relay",
		},
	}

	if path != "" {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("loading config %s: %w", path, err)
		}
	}

	// Environment variable overrides
	if v := os.Getenv("ARC_RELAY_ENCRYPTION_KEY"); v != "" {
		cfg.Encryption.Key = v
	}
	if v := os.Getenv("ARC_RELAY_SESSION_SECRET"); v != "" {
		cfg.Auth.SessionSecret = v
	}
	if v := os.Getenv("ARC_RELAY_ADMIN_PASSWORD"); v != "" {
		cfg.Auth.AdminPassword = v
	}
	if v := os.Getenv("ARC_RELAY_DB_PATH"); v != "" {
		cfg.Database.Path = v
	}
	if v := os.Getenv("ARC_RELAY_MEMORY_DB_PATH"); v != "" {
		cfg.Database.MemoryPath = v
	}
	if v := os.Getenv("ARC_RELAY_SKILLS_DIR"); v != "" {
		cfg.Skills.BundlesDir = v
	}
	if v := os.Getenv("ARC_RELAY_BASE_URL"); v != "" {
		cfg.Server.BaseURL = v
	} else if v := os.Getenv("RENDER_EXTERNAL_URL"); v != "" {
		// Render exposes the full https URL at this env var.
		cfg.Server.BaseURL = v
	} else if v := os.Getenv("RAILWAY_PUBLIC_DOMAIN"); v != "" {
		// Railway exposes only the hostname; assume https.
		cfg.Server.BaseURL = "https://" + v
	}
	if v := os.Getenv("ARC_RELAY_LLM_API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := os.Getenv("ARC_RELAY_LLM_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("ARC_RELAY_SENTRY_DSN"); v != "" {
		cfg.SentryDSN = v
	}
	if v := os.Getenv("ARC_RELAY_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	// Skill checker env overrides. Applied before defaults so a parseable
	// override of zero/empty falls through to the default rather than being
	// preserved as an explicit override.
	if v := os.Getenv("ARC_RELAY_SKILLS_CHECKER_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			cfg.Skills.Checker.Enabled = true
		case "0", "false", "no", "off":
			cfg.Skills.Checker.Enabled = false
		}
	}
	if v := os.Getenv("ARC_RELAY_SKILLS_CHECKER_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > 0 {
			cfg.Skills.Checker.Interval = d
		}
	}
	if v := os.Getenv("ARC_RELAY_SKILLS_CHECKER_UPSTREAM_CACHE_DIR"); v != "" {
		cfg.Skills.Checker.UpstreamCacheDir = v
	}

	// Apply checker defaults. Cache dir default mirrors how skill bundles
	// resolve <dataDir>/skills — we use <dataDir>/upstream-cache here.
	// LLMModel intentionally has no default: empty string falls back to the
	// shared llm.Client's own default (gpt-4o-mini).
	if cfg.Skills.Checker.Interval <= 0 {
		cfg.Skills.Checker.Interval = 24 * time.Hour
	}
	if cfg.Skills.Checker.UpstreamCacheDir == "" {
		cfg.Skills.Checker.UpstreamCacheDir = filepath.Join(filepath.Dir(cfg.Database.Path), "upstream-cache")
	}
	if cfg.Skills.Checker.GitCloneTimeout <= 0 {
		cfg.Skills.Checker.GitCloneTimeout = 60 * time.Second
	}
	if cfg.Skills.Checker.LLMDiffMaxBytes <= 0 {
		cfg.Skills.Checker.LLMDiffMaxBytes = 32768
	}
	if cfg.Skills.Checker.LLMPerFileMaxBytes <= 0 {
		cfg.Skills.Checker.LLMPerFileMaxBytes = 4096
	}

	if v := os.Getenv("ARC_RELAY_PORT"); v != "" {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil {
			cfg.Server.Port = port
		}
	} else if v := os.Getenv("PORT"); v != "" {
		// PaaS platforms (Render, Heroku, Railway, Fly) inject PORT.
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil {
			cfg.Server.Port = port
		}
	}

	return cfg, nil
}

// SlogLevel parses the LogLevel string into a slog.Level.
// Defaults to Info if unset or unrecognized.
func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

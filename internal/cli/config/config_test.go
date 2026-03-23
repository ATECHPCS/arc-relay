package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveAndLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		WranglerURL: "http://10.10.69.50:8080",
		APIKey:      "test-token-123",
	}

	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if loaded.WranglerURL != cfg.WranglerURL {
		t.Errorf("WranglerURL = %q, want %q", loaded.WranglerURL, cfg.WranglerURL)
	}
	if loaded.APIKey != cfg.APIKey {
		t.Errorf("APIKey = %q, want %q", loaded.APIKey, cfg.APIKey)
	}
}

func TestSaveConfigPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission checks not applicable on Windows")
	}

	dir := t.TempDir()
	cfg := &Config{WranglerURL: "http://example.com", APIKey: "key"}

	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	info, err := os.Stat(ConfigPath(dir))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("config file permissions = %04o, want 0600", perm)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	// TempDir creates with its own perms, so we check the nested dir if created
	_ = dirInfo
}

func TestLoadConfigNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for missing config, got nil")
	}
}

func TestLoadConfigMissingURL(t *testing.T) {
	dir := t.TempDir()
	data := `{"api_key": "key"}`
	os.WriteFile(filepath.Join(dir, configFileName), []byte(data), 0600)

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for missing wrangler_url, got nil")
	}
}

func TestLoadConfigMissingKey(t *testing.T) {
	dir := t.TempDir()
	data := `{"wrangler_url": "http://example.com"}`
	os.WriteFile(filepath.Join(dir, configFileName), []byte(data), 0600)

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for missing api_key, got nil")
	}
}

func TestLoadConfigMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, configFileName), []byte("{not json"), 0600)

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestResolveCredentialsFromEnv(t *testing.T) {
	t.Setenv("MCP_SYNC_URL", "http://env-wrangler:8080")
	t.Setenv("MCP_SYNC_API_KEY", "env-token")

	creds, err := ResolveCredentials(t.TempDir())
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}

	if creds.Source != "environment" {
		t.Errorf("Source = %q, want %q", creds.Source, "environment")
	}
	if creds.WranglerURL != "http://env-wrangler:8080" {
		t.Errorf("WranglerURL = %q, want http://env-wrangler:8080", creds.WranglerURL)
	}
	if creds.APIKey != "env-token" {
		t.Errorf("APIKey = %q, want env-token", creds.APIKey)
	}
}

func TestResolveCredentialsFromFile(t *testing.T) {
	// Ensure env vars are cleared
	t.Setenv("MCP_SYNC_URL", "")
	t.Setenv("MCP_SYNC_API_KEY", "")

	dir := t.TempDir()
	cfg := &Config{WranglerURL: "http://file-wrangler:8080", APIKey: "file-token"}
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	creds, err := ResolveCredentials(dir)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}

	if creds.Source == "environment" {
		t.Error("expected source to be config file, got environment")
	}
	if creds.WranglerURL != "http://file-wrangler:8080" {
		t.Errorf("WranglerURL = %q, want http://file-wrangler:8080", creds.WranglerURL)
	}
}

func TestResolveCredentialsEnvPartial(t *testing.T) {
	// Only URL set in env, no key — should fall through to config file
	t.Setenv("MCP_SYNC_URL", "http://partial:8080")
	t.Setenv("MCP_SYNC_API_KEY", "")

	dir := t.TempDir()
	cfg := &Config{WranglerURL: "http://file:8080", APIKey: "file-key"}
	SaveConfig(dir, cfg)

	creds, err := ResolveCredentials(dir)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}

	// Should fall through to file since both env vars must be set
	if creds.Source == "environment" {
		t.Error("expected file source when only partial env vars set")
	}
}

func TestCheckPermissionsSecure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission checks not applicable on Windows")
	}

	dir := t.TempDir()
	cfg := &Config{WranglerURL: "http://example.com", APIKey: "key"}
	SaveConfig(dir, cfg)

	warning := CheckPermissions(dir)
	if warning != "" {
		t.Errorf("expected no warning for 0600 file, got: %s", warning)
	}
}

func TestCheckPermissionsInsecure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission checks not applicable on Windows")
	}

	dir := t.TempDir()
	cfg := &Config{WranglerURL: "http://example.com", APIKey: "key"}
	SaveConfig(dir, cfg)

	// Make insecure
	os.Chmod(ConfigPath(dir), 0644)

	warning := CheckPermissions(dir)
	if warning == "" {
		t.Error("expected warning for 0644 file, got empty")
	}
}

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	dockermgr "github.com/comma-compliance/arc-relay/internal/docker"
	"github.com/comma-compliance/arc-relay/internal/mcp"
	"github.com/comma-compliance/arc-relay/internal/oauth"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// Backend is the interface for sending MCP requests to a backend server.
type Backend interface {
	Send(ctx context.Context, req *mcp.Request) (*mcp.Response, error)
}

// Manager manages proxy backends for all configured MCP servers.
type Manager struct {
	mu       sync.RWMutex
	backends map[string]Backend // server ID -> backend
	servers  *store.ServerStore
	docker   *dockermgr.Manager

	// Track container IDs for managed servers
	containers map[string]string // server ID -> container ID

	// Endpoint cache
	Endpoints *mcp.EndpointCache

	// Access tier store for endpoint-level access control
	AccessStore *store.AccessStore

	// OAuth manager for remote servers with OAuth auth
	OAuthManager *oauth.Manager

	// Per-server build locks to prevent concurrent rebuild races
	buildLocks sync.Map // server ID -> *sync.Mutex
}

// NewManager creates a new proxy manager.
func NewManager(servers *store.ServerStore, docker *dockermgr.Manager, oauthMgr *oauth.Manager, accessStore *store.AccessStore) *Manager {
	return &Manager{
		backends:     make(map[string]Backend),
		servers:      servers,
		docker:       docker,
		containers:   make(map[string]string),
		Endpoints:    mcp.NewEndpointCache(),
		AccessStore:  accessStore,
		OAuthManager: oauthMgr,
	}
}

// StartServer starts a managed server and creates the proxy backend.
func (m *Manager) StartServer(ctx context.Context, srv *store.Server) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.backends[srv.ID]; exists {
		return fmt.Errorf("server %s is already running", srv.Name)
	}

	switch srv.ServerType {
	case store.ServerTypeStdio:
		return m.startStdio(ctx, srv)
	case store.ServerTypeHTTP:
		return m.startHTTP(ctx, srv)
	case store.ServerTypeRemote:
		return m.startRemote(ctx, srv)
	default:
		return fmt.Errorf("unknown server type: %s", srv.ServerType)
	}
}

// RetryServer attempts to reconnect a stateless server (remote or external HTTP).
// It removes any stale backend and creates a fresh connection.
// For remote servers, we skip verification ping because enumerateAsync (which
// establishes the MCP session) runs concurrently and the ping would race with it.
// The next health check cycle will verify the connection is actually working.
func (m *Manager) RetryServer(ctx context.Context, srv *store.Server) error {
	m.mu.Lock()
	delete(m.backends, srv.ID)
	m.mu.Unlock()

	return m.StartServer(ctx, srv)
}

func (m *Manager) startStdio(ctx context.Context, srv *store.Server) error {
	var cfg store.StdioConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return fmt.Errorf("parsing stdio config: %w", err)
	}

	m.servers.UpdateStatus(srv.ID, store.StatusStarting, "")

	// Auto-build image from package if Build config is set
	if cfg.Build != nil {
		tag := cfg.Build.BuildImageTag()
		if cfg.Image == "" {
			cfg.Image = tag
		}
		// Build (or rebuild) if the image doesn't exist locally
		if err := m.buildImageIfNeeded(ctx, srv, &cfg, tag, false); err != nil {
			return err
		}
	}

	// Ensure image exists (pull from registry for non-build images, verify for build images)
	log.Printf("Pulling image %s for server %s...", cfg.Image, srv.Name)
	if err := m.docker.EnsureImage(ctx, cfg.Image); err != nil {
		m.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
		return fmt.Errorf("pulling image: %w", err)
	}

	// Start container
	containerID, err := m.docker.StartContainer(ctx, dockermgr.ContainerConfig{
		Name:       srv.Name,
		Image:      cfg.Image,
		Entrypoint: cfg.Entrypoint,
		Command:    cfg.Command,
		Env:        cfg.Env,
		Port:       0, // stdio, no port
	})
	if err != nil {
		m.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
		return fmt.Errorf("starting container: %w", err)
	}

	// Attach to stdin/stdout
	stdin, stdout, err := m.docker.AttachStdio(ctx, containerID)
	if err != nil {
		m.docker.StopContainer(ctx, containerID)
		m.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
		return fmt.Errorf("attaching to container: %w", err)
	}

	bridge := NewStdioBridge(stdin, stdout)
	m.backends[srv.ID] = bridge
	m.containers[srv.ID] = containerID
	m.servers.UpdateStatus(srv.ID, store.StatusRunning, "")

	log.Printf("Started stdio server %s (container %s)", srv.Name, containerID[:12])
	m.enumerateAsync(srv.ID, srv.Name)
	return nil
}

func (m *Manager) startHTTP(ctx context.Context, srv *store.Server) error {
	var cfg store.HTTPConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return fmt.Errorf("parsing http config: %w", err)
	}

	// External HTTP server (no Docker management)
	if cfg.URL != "" {
		m.backends[srv.ID] = NewHTTPProxy(cfg.URL)
		m.servers.UpdateStatus(srv.ID, store.StatusRunning, "")
		log.Printf("Connected to external HTTP server %s at %s", srv.Name, cfg.URL)
		m.enumerateAsync(srv.ID, srv.Name)
		return nil
	}

	// Docker-managed HTTP server
	m.servers.UpdateStatus(srv.ID, store.StatusStarting, "")

	log.Printf("Pulling image %s for server %s...", cfg.Image, srv.Name)
	if err := m.docker.EnsureImage(ctx, cfg.Image); err != nil {
		m.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
		return fmt.Errorf("pulling image: %w", err)
	}

	containerID, err := m.docker.StartContainer(ctx, dockermgr.ContainerConfig{
		Name:  srv.Name,
		Image: cfg.Image,
		Env:   cfg.Env,
		Port:  cfg.Port,
	})
	if err != nil {
		m.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
		return fmt.Errorf("starting container: %w", err)
	}

	// Get the mapped host port
	hostPort, err := m.docker.GetHostPort(ctx, containerID, cfg.Port)
	if err != nil {
		m.docker.StopContainer(ctx, containerID)
		m.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
		return fmt.Errorf("getting host port: %w", err)
	}

	targetURL := fmt.Sprintf("http://127.0.0.1:%s", hostPort)
	m.backends[srv.ID] = NewHTTPProxy(targetURL)
	m.containers[srv.ID] = containerID
	m.servers.UpdateStatus(srv.ID, store.StatusRunning, "")

	log.Printf("Started HTTP server %s (container %s, port %s)", srv.Name, containerID[:12], hostPort)
	m.enumerateAsync(srv.ID, srv.Name)
	return nil
}

func (m *Manager) startRemote(ctx context.Context, srv *store.Server) error {
	var cfg store.RemoteConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return fmt.Errorf("parsing remote config: %w", err)
	}

	// For OAuth servers, check that tokens exist before starting
	if cfg.Auth.Type == "oauth" && cfg.Auth.AccessToken == "" {
		if m.OAuthManager == nil || !m.OAuthManager.HasTokens(srv.ID) {
			m.servers.UpdateStatus(srv.ID, store.StatusError, "OAuth not yet authorized — click Authorize on the server detail page")
			return fmt.Errorf("OAuth not yet authorized for server %s", srv.Name)
		}
	}

	m.backends[srv.ID] = NewRemoteProxy(srv.ID, cfg, m.OAuthManager)
	m.servers.UpdateStatus(srv.ID, store.StatusRunning, "")

	log.Printf("Connected to remote server %s at %s", srv.Name, cfg.URL)
	m.enumerateAsync(srv.ID, srv.Name)
	return nil
}

// acquireBuildLock returns a per-server mutex for serializing builds.
func (m *Manager) acquireBuildLock(serverID string) *sync.Mutex {
	v, _ := m.buildLocks.LoadOrStore(serverID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// buildImageIfNeeded generates and builds a Docker image from a StdioBuildConfig.
// If force is false, it skips the build when the image already exists locally.
func (m *Manager) buildImageIfNeeded(ctx context.Context, srv *store.Server, cfg *store.StdioConfig, tag string, force bool) error {
	mu := m.acquireBuildLock(srv.ID)
	mu.Lock()
	defer mu.Unlock()

	if !force && m.docker.ImageExists(ctx, tag) {
		log.Printf("Image %s already exists for server %s, skipping build", tag, srv.Name)
		return nil
	}

	build := cfg.Build

	// Route to git repo build when GitURL is set without a Package or custom Dockerfile
	if build.GitURL != "" && build.Package == "" && build.Dockerfile == "" {
		return m.buildFromGitRepo(ctx, srv, cfg, tag, force)
	}

	dockerfile, err := dockermgr.GenerateDockerfile(build.Runtime, build.Package, build.Version, build.GitURL, build.Dockerfile)
	if err != nil {
		m.servers.UpdateStatus(srv.ID, store.StatusError, "Dockerfile generation failed: "+err.Error())
		return fmt.Errorf("generating Dockerfile: %w", err)
	}

	log.Printf("Building image %s for server %s...", tag, srv.Name)
	if err := m.docker.BuildImage(ctx, dockerfile, tag, force); err != nil {
		m.servers.UpdateStatus(srv.ID, store.StatusError, "Image build failed: "+err.Error())
		return fmt.Errorf("building image: %w", err)
	}

	// Persist the built image tag back to config
	cfg.Image = tag
	updatedConfig, _ := json.Marshal(cfg)
	m.servers.UpdateConfig(srv.ID, updatedConfig)

	log.Printf("Built image %s for server %s", tag, srv.Name)
	return nil
}

// buildFromGitRepo clones a git repository and builds using the repo's own Dockerfile.
// If the repo has no Dockerfile, it falls back to the generated template.
func (m *Manager) buildFromGitRepo(ctx context.Context, srv *store.Server, cfg *store.StdioConfig, tag string, noCache bool) error {
	build := cfg.Build

	// Validate URL scheme — only allow https://
	if err := validateGitURL(build.GitURL); err != nil {
		m.servers.UpdateStatus(srv.ID, store.StatusError, "Invalid git URL: "+err.Error())
		return fmt.Errorf("invalid git URL: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "arc-relay-git-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Build git clone command with security hardening
	args := []string{"clone", "--depth", "1"}
	if build.GitRef != "" {
		args = append(args, "--branch", build.GitRef)
	}
	args = append(args, "--", build.GitURL, tmpDir)

	log.Printf("Cloning %s (ref: %s) for server %s...", build.GitURL, build.GitRef, srv.Name)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		errMsg := fmt.Sprintf("git clone failed: %s", strings.TrimSpace(string(output)))
		m.servers.UpdateStatus(srv.ID, store.StatusError, errMsg)
		return fmt.Errorf("%s: %w", errMsg, err)
	}

	// Check if repo has its own Dockerfile
	dockerfilePath := filepath.Join(tmpDir, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); err == nil {
		// Build using repo's own Dockerfile with full repo as context
		log.Printf("Building image %s from repo Dockerfile for server %s...", tag, srv.Name)
		if err := m.docker.BuildImageFromContext(ctx, tmpDir, "Dockerfile", tag, noCache); err != nil {
			m.servers.UpdateStatus(srv.ID, store.StatusError, "Image build failed: "+err.Error())
			return fmt.Errorf("building image from context: %w", err)
		}
	} else {
		// No Dockerfile in repo — fall back to generated template
		log.Printf("No Dockerfile in repo, using generated template for server %s", srv.Name)
		dockerfile, err := dockermgr.GenerateDockerfile(build.Runtime, "", "", build.GitURL, "")
		if err != nil {
			m.servers.UpdateStatus(srv.ID, store.StatusError, "Dockerfile generation failed: "+err.Error())
			return fmt.Errorf("generating Dockerfile: %w", err)
		}
		if err := m.docker.BuildImage(ctx, dockerfile, tag, noCache); err != nil {
			m.servers.UpdateStatus(srv.ID, store.StatusError, "Image build failed: "+err.Error())
			return fmt.Errorf("building image: %w", err)
		}
	}

	// Persist the built image tag back to config
	cfg.Image = tag
	updatedConfig, _ := json.Marshal(cfg)
	m.servers.UpdateConfig(srv.ID, updatedConfig)

	log.Printf("Built image %s for server %s", tag, srv.Name)
	return nil
}

// validateGitURL ensures only https:// URLs are accepted for git clone.
func validateGitURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("malformed URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("only https:// git URLs are allowed, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("git URL must include a host")
	}
	return nil
}

// RebuildImage force-rebuilds the Docker image for a stdio server with build config.
func (m *Manager) RebuildImage(ctx context.Context, srv *store.Server) error {
	if srv.ServerType != store.ServerTypeStdio {
		return fmt.Errorf("rebuild is only supported for stdio servers")
	}

	var cfg store.StdioConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return fmt.Errorf("parsing stdio config: %w", err)
	}
	if cfg.Build == nil {
		return fmt.Errorf("server has no build config")
	}

	tag := cfg.Build.BuildImageTag()
	return m.buildImageIfNeeded(ctx, srv, &cfg, tag, true)
}

// EnumerateServer discovers tools, resources, and prompts from a running server.
func (m *Manager) EnumerateServer(ctx context.Context, serverID string) (*mcp.ServerEndpoints, error) {
	backend, ok := m.GetBackend(serverID)
	if !ok {
		return nil, fmt.Errorf("server not running")
	}

	endpoints, err := mcp.Enumerate(ctx, backend)
	if err != nil {
		log.Printf("Enumeration failed for server %s: %v", serverID, err)
	}
	m.Endpoints.Set(serverID, endpoints)

	// Sync access tiers after enumeration
	if m.AccessStore != nil && endpoints != nil {
		m.syncAccessTiers(serverID, endpoints)
	}

	return endpoints, err
}

// syncAccessTiers updates the access tier database after endpoint enumeration.
func (m *Manager) syncAccessTiers(serverID string, endpoints *mcp.ServerEndpoints) {
	var infos []store.EndpointInfo
	for _, t := range endpoints.Tools {
		infos = append(infos, store.EndpointInfo{Type: "tool", Name: t.Name, Description: t.Description})
	}
	for _, r := range endpoints.Resources {
		infos = append(infos, store.EndpointInfo{Type: "resource", Name: r.URI, Description: r.Description})
	}
	for _, p := range endpoints.Prompts {
		infos = append(infos, store.EndpointInfo{Type: "prompt", Name: p.Name, Description: p.Description})
	}
	m.AccessStore.SyncAfterEnumerate(serverID, infos, mcp.ClassifyEndpoint)
}

// enumerateAsync runs enumeration in a background goroutine after server start.
func (m *Manager) enumerateAsync(serverID, serverName string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		endpoints, err := m.EnumerateServer(ctx, serverID)
		if err != nil {
			log.Printf("Background enumeration failed for %s: %v", serverName, err)
			return
		}

		toolCount := len(endpoints.Tools)
		resourceCount := len(endpoints.Resources)
		promptCount := len(endpoints.Prompts)
		log.Printf("Enumerated %s: %d tools, %d resources, %d prompts (server: %s v%s)",
			serverName, toolCount, resourceCount, promptCount,
			endpoints.ServerInfo.Name, endpoints.ServerInfo.Version)
	}()
}

// StopServer stops a managed server and removes its backend.
func (m *Manager) StopServer(ctx context.Context, serverID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Close the bridge if it's a stdio bridge
	if backend, ok := m.backends[serverID]; ok {
		if bridge, ok := backend.(*StdioBridge); ok {
			bridge.Close()
		}
	}
	delete(m.backends, serverID)
	m.Endpoints.Remove(serverID)

	// Stop container if managed
	if containerID, ok := m.containers[serverID]; ok {
		if err := m.docker.StopContainer(ctx, containerID); err != nil {
			log.Printf("Error stopping container for server %s: %v", serverID, err)
		}
		delete(m.containers, serverID)
	}

	m.servers.UpdateStatus(serverID, store.StatusStopped, "")
	m.servers.UpdateHealth(serverID, store.HealthUnknown, "")
	return nil
}

// GetBackend returns the backend for a server by name.
func (m *Manager) GetBackend(serverID string) (Backend, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.backends[serverID]
	return b, ok
}

// Docker returns the Docker manager for image/container inspection.
func (m *Manager) Docker() *dockermgr.Manager {
	return m.docker
}

// GetContainerID returns the Docker container ID for a managed server, if any.
func (m *Manager) GetContainerID(serverID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.containers[serverID]
	return id, ok
}

// RebuildAndRestart force-rebuilds the image then restarts the server.
func (m *Manager) RebuildAndRestart(ctx context.Context, srv *store.Server) error {
	if err := m.RebuildImage(ctx, srv); err != nil {
		return fmt.Errorf("rebuild failed: %w", err)
	}
	m.StopServer(ctx, srv.ID)
	// Re-fetch to get updated config (image tag may have changed)
	updated, err := m.servers.Get(srv.ID)
	if err != nil || updated == nil {
		return fmt.Errorf("failed to refresh server after rebuild: %w", err)
	}
	return m.StartServer(ctx, updated)
}

// RecreateContainer stops the current container and starts a fresh one.
// Used for image-based servers where the image was updated externally.
func (m *Manager) RecreateContainer(ctx context.Context, srv *store.Server) error {
	m.StopServer(ctx, srv.ID)
	updated, err := m.servers.Get(srv.ID)
	if err != nil || updated == nil {
		return fmt.Errorf("failed to refresh server: %w", err)
	}
	return m.StartServer(ctx, updated)
}

// PullAndRecreateContainer pulls the latest image from the registry, then
// stops the current container and starts a fresh one from the updated image.
func (m *Manager) PullAndRecreateContainer(ctx context.Context, srv *store.Server) error {
	image := serverImageRef(srv)
	if image == "" {
		return fmt.Errorf("server has no Docker image configured")
	}
	log.Printf("Pulling latest image %s for server %s...", image, srv.Name)
	if err := m.docker.PullImage(ctx, image); err != nil {
		return fmt.Errorf("pulling image: %w", err)
	}
	return m.RecreateContainer(ctx, srv)
}

// RecreateWithProgress performs a recreate (optionally with pull) and reports
// progress via a callback. Each call to progress sends a status message to
// the caller (e.g. for SSE streaming to the browser).
func (m *Manager) RecreateWithProgress(ctx context.Context, srv *store.Server, pull bool, progress func(string)) error {
	if pull {
		image := serverImageRef(srv)
		if image == "" {
			return fmt.Errorf("server has no Docker image configured")
		}
		progress("Pulling latest image: " + image)
		if err := m.docker.PullImage(ctx, image); err != nil {
			return fmt.Errorf("pulling image: %w", err)
		}
		progress("Image pulled successfully")
	}

	progress("Stopping container...")
	m.StopServer(ctx, srv.ID)

	updated, err := m.servers.Get(srv.ID)
	if err != nil || updated == nil {
		return fmt.Errorf("failed to refresh server: %w", err)
	}

	progress("Starting new container...")
	if err := m.StartServer(ctx, updated); err != nil {
		return err
	}

	return nil
}

// serverImageRef extracts the Docker image reference from a server's config.
func serverImageRef(srv *store.Server) string {
	switch srv.ServerType {
	case store.ServerTypeStdio:
		var cfg store.StdioConfig
		if err := json.Unmarshal(srv.Config, &cfg); err == nil {
			return cfg.Image
		}
	case store.ServerTypeHTTP:
		var cfg store.HTTPConfig
		if err := json.Unmarshal(srv.Config, &cfg); err == nil && cfg.URL == "" {
			return cfg.Image
		}
	}
	return ""
}

// StopAll stops all running servers.
func (m *Manager) StopAll(ctx context.Context) {
	m.mu.Lock()
	serverIDs := make([]string, 0, len(m.backends))
	for id := range m.backends {
		serverIDs = append(serverIDs, id)
	}
	m.mu.Unlock()

	for _, id := range serverIDs {
		if err := m.StopServer(ctx, id); err != nil {
			log.Printf("Error stopping server %s: %v", id, err)
		}
	}
}

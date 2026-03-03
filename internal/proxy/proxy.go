package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	dockermgr "github.com/JeremiahChurch/mcp-wrangler/internal/docker"
	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
	"github.com/JeremiahChurch/mcp-wrangler/internal/oauth"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
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
// It removes any stale backend, creates a fresh connection, and pings to verify.
func (m *Manager) RetryServer(ctx context.Context, srv *store.Server) error {
	m.mu.Lock()
	delete(m.backends, srv.ID)
	m.mu.Unlock()

	if err := m.StartServer(ctx, srv); err != nil {
		return err
	}

	// Verify it's actually reachable before declaring recovery
	backend, ok := m.GetBackend(srv.ID)
	if !ok {
		return fmt.Errorf("backend not found after start")
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	id, _ := json.Marshal(999999)
	req := &mcp.Request{JSONRPC: "2.0", ID: id, Method: "ping"}
	if _, err := backend.Send(pingCtx, req); err != nil {
		// Ping failed — clean up the backend we just created
		m.mu.Lock()
		delete(m.backends, srv.ID)
		m.mu.Unlock()
		m.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
		return err
	}

	return nil
}

func (m *Manager) startStdio(ctx context.Context, srv *store.Server) error {
	var cfg store.StdioConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return fmt.Errorf("parsing stdio config: %w", err)
	}

	m.servers.UpdateStatus(srv.ID, store.StatusStarting, "")

	// Pull image
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
		Name:    srv.Name,
		Image:   cfg.Image,
		Env:     cfg.Env,
		Port:    cfg.Port,
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
	return nil
}

// GetBackend returns the backend for a server by name.
func (m *Manager) GetBackend(serverID string) (Backend, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.backends[serverID]
	return b, ok
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

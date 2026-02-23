package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	dockermgr "github.com/JeremiahChurch/mcp-wrangler/internal/docker"
	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
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
}

// NewManager creates a new proxy manager.
func NewManager(servers *store.ServerStore, docker *dockermgr.Manager) *Manager {
	return &Manager{
		backends:   make(map[string]Backend),
		servers:    servers,
		docker:     docker,
		containers: make(map[string]string),
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

func (m *Manager) startStdio(ctx context.Context, srv *store.Server) error {
	var cfg store.StdioConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return fmt.Errorf("parsing stdio config: %w", err)
	}

	m.servers.UpdateStatus(srv.ID, store.StatusStarting, "")

	// Pull image
	log.Printf("Pulling image %s for server %s...", cfg.Image, srv.Name)
	if err := m.docker.PullImage(ctx, cfg.Image); err != nil {
		m.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
		return fmt.Errorf("pulling image: %w", err)
	}

	// Start container
	containerID, err := m.docker.StartContainer(ctx, dockermgr.ContainerConfig{
		Name:    srv.Name,
		Image:   cfg.Image,
		Command: cfg.Command,
		Env:     cfg.Env,
		Port:    0, // stdio, no port
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
		return nil
	}

	// Docker-managed HTTP server
	m.servers.UpdateStatus(srv.ID, store.StatusStarting, "")

	log.Printf("Pulling image %s for server %s...", cfg.Image, srv.Name)
	if err := m.docker.PullImage(ctx, cfg.Image); err != nil {
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
	return nil
}

func (m *Manager) startRemote(ctx context.Context, srv *store.Server) error {
	var cfg store.RemoteConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return fmt.Errorf("parsing remote config: %w", err)
	}

	m.backends[srv.ID] = NewRemoteProxy(cfg)
	m.servers.UpdateStatus(srv.ID, store.StatusRunning, "")

	log.Printf("Connected to remote server %s at %s", srv.Name, cfg.URL)
	return nil
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

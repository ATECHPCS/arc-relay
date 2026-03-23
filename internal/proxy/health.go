package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

const (
	// failThreshold is the number of consecutive probe failures before marking a server as Error.
	failThreshold = 3
	// recoveryCooldown is the minimum time between recovery attempts for a server.
	recoveryCooldown = 5 * time.Minute
)

// HealthMonitor periodically checks running servers and updates their status.
type HealthMonitor struct {
	proxyMgr *Manager
	servers  *store.ServerStore
	interval time.Duration
	cancel   context.CancelFunc

	mu          sync.Mutex
	failCounts  map[string]int       // server ID -> consecutive probe failures
	lastRecover map[string]time.Time // server ID -> last recovery attempt
}

// NewHealthMonitor creates a health monitor that checks servers at the given interval.
func NewHealthMonitor(proxyMgr *Manager, servers *store.ServerStore, interval time.Duration) *HealthMonitor {
	return &HealthMonitor{
		proxyMgr:    proxyMgr,
		servers:     servers,
		interval:    interval,
		failCounts:  make(map[string]int),
		lastRecover: make(map[string]time.Time),
	}
}

// Start begins periodic health checking in a background goroutine.
func (hm *HealthMonitor) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	hm.cancel = cancel

	go func() {
		ticker := time.NewTicker(hm.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				hm.checkAll(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	log.Printf("Health monitor started (interval: %s)", hm.interval)
}

// Stop stops the health monitor.
func (hm *HealthMonitor) Stop() {
	if hm.cancel != nil {
		hm.cancel()
	}
}

// CheckHealth performs an on-demand MCP health check for a single server.
// Returns the health status and any error message.
func (hm *HealthMonitor) CheckHealth(ctx context.Context, serverID string) (store.HealthStatus, string) {
	backend, ok := hm.proxyMgr.GetBackend(serverID)
	if !ok {
		return store.HealthUnknown, "no backend available"
	}

	if err := hm.pingServer(ctx, backend); err != nil {
		hm.servers.UpdateHealth(serverID, store.HealthUnhealthy, err.Error())
		return store.HealthUnhealthy, err.Error()
	}

	hm.servers.UpdateHealth(serverID, store.HealthHealthy, "")
	return store.HealthHealthy, ""
}

func (hm *HealthMonitor) checkAll(ctx context.Context) {
	servers, err := hm.servers.List()
	if err != nil {
		log.Printf("Health monitor: error listing servers: %v", err)
		return
	}

	for _, srv := range servers {
		if srv.Status == store.StatusRunning {
			hm.checkRunning(ctx, srv)
		} else if srv.Status == store.StatusError {
			hm.tryRecover(ctx, srv)
		} else if srv.Status == store.StatusStopped && srv.Health != store.HealthUnknown {
			// Reset health to unknown when stopped
			hm.servers.UpdateHealth(srv.ID, store.HealthUnknown, "")
		}
	}
}

func (hm *HealthMonitor) checkRunning(ctx context.Context, srv *store.Server) {
	backend, ok := hm.proxyMgr.GetBackend(srv.ID)
	if !ok {
		hm.servers.UpdateStatus(srv.ID, store.StatusStopped, "backend not found")
		hm.servers.UpdateHealth(srv.ID, store.HealthUnknown, "")
		hm.resetFailCount(srv.ID)
		log.Printf("Health monitor: server %s has no backend, marking stopped", srv.Name)
		return
	}

	if err := hm.pingServer(ctx, backend); err != nil {
		hm.mu.Lock()
		hm.failCounts[srv.ID]++
		count := hm.failCounts[srv.ID]
		hm.mu.Unlock()

		hm.servers.UpdateHealth(srv.ID, store.HealthUnhealthy, err.Error())

		if count >= failThreshold {
			log.Printf("Health monitor: server %s failed %d consecutive probes, marking error: %v", srv.Name, count, err)
			hm.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
		} else {
			log.Printf("Health monitor: server %s probe failed (%d/%d): %v", srv.Name, count, failThreshold, err)
		}
	} else {
		hm.resetFailCount(srv.ID)
		if srv.Health != store.HealthHealthy {
			log.Printf("Health monitor: server %s is healthy", srv.Name)
		}
		hm.servers.UpdateHealth(srv.ID, store.HealthHealthy, "")
	}
}

// tryRecover attempts to reconnect errored remote and external HTTP servers.
// Docker-managed servers require explicit restart (container lifecycle).
// Applies a cooldown to prevent rapid reconnect storms.
func (hm *HealthMonitor) tryRecover(ctx context.Context, srv *store.Server) {
	if !hm.isStatelessServer(srv) {
		return
	}

	hm.mu.Lock()
	lastAttempt, exists := hm.lastRecover[srv.ID]
	if exists && time.Since(lastAttempt) < recoveryCooldown {
		hm.mu.Unlock()
		return // too soon, skip this cycle
	}
	hm.lastRecover[srv.ID] = time.Now()
	hm.mu.Unlock()

	if err := hm.proxyMgr.RetryServer(ctx, srv); err != nil {
		return // still down, stay in error state silently
	}

	hm.resetFailCount(srv.ID)
	log.Printf("Health monitor: auto-recovered server %s", srv.Name)
}

func (hm *HealthMonitor) resetFailCount(serverID string) {
	hm.mu.Lock()
	delete(hm.failCounts, serverID)
	hm.mu.Unlock()
}

// isStatelessServer returns true for server types that can be reconnected
// without managing external state (containers, processes, etc).
func (hm *HealthMonitor) isStatelessServer(srv *store.Server) bool {
	switch srv.ServerType {
	case store.ServerTypeRemote:
		return true
	case store.ServerTypeHTTP:
		var cfg store.HTTPConfig
		if err := json.Unmarshal(srv.Config, &cfg); err != nil {
			return false
		}
		return cfg.URL != "" // external HTTP only, not Docker-managed
	default:
		return false
	}
}

// pingServer sends an MCP ping request to verify the server is responsive.
// Uses the standard MCP ping method which works on established sessions
// without requiring re-initialization.
func (hm *HealthMonitor) pingServer(ctx context.Context, backend Backend) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	id, _ := json.Marshal(999999)
	req := &mcp.Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "ping",
	}

	resp, err := backend.Send(ctx, req)
	if err != nil {
		return fmt.Errorf("ping failed: %w", err)
	}

	// A valid JSON-RPC response (even an error response) means the MCP layer is alive
	if resp == nil {
		return fmt.Errorf("no response to ping")
	}

	return nil
}

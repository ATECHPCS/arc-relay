package proxy

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

// HealthMonitor periodically checks running servers and updates their status.
type HealthMonitor struct {
	proxyMgr *Manager
	servers  *store.ServerStore
	interval time.Duration
	cancel   context.CancelFunc
}

// NewHealthMonitor creates a health monitor that checks servers at the given interval.
func NewHealthMonitor(proxyMgr *Manager, servers *store.ServerStore, interval time.Duration) *HealthMonitor {
	return &HealthMonitor{
		proxyMgr: proxyMgr,
		servers:  servers,
		interval: interval,
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
		}
	}
}

func (hm *HealthMonitor) checkRunning(ctx context.Context, srv *store.Server) {
	backend, ok := hm.proxyMgr.GetBackend(srv.ID)
	if !ok {
		hm.servers.UpdateStatus(srv.ID, store.StatusStopped, "backend not found")
		log.Printf("Health monitor: server %s has no backend, marking stopped", srv.Name)
		return
	}

	if err := hm.pingServer(ctx, backend, srv.Name); err != nil {
		log.Printf("Health monitor: server %s ping failed: %v", srv.Name, err)
		hm.servers.UpdateStatus(srv.ID, store.StatusError, err.Error())
	}
}

// tryRecover attempts to reconnect errored remote and external HTTP servers.
// Docker-managed servers require explicit restart (container lifecycle).
func (hm *HealthMonitor) tryRecover(ctx context.Context, srv *store.Server) {
	if !hm.isStatelessServer(srv) {
		return
	}

	if err := hm.proxyMgr.RetryServer(ctx, srv); err != nil {
		return // still down, stay in error state silently
	}

	log.Printf("Health monitor: auto-recovered server %s", srv.Name)
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
func (hm *HealthMonitor) pingServer(ctx context.Context, backend Backend, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	id, _ := json.Marshal(999999)
	req := &mcp.Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "ping",
	}

	_, err := backend.Send(ctx, req)
	if err != nil {
		return err
	}

	// Any response (even an error response) means the server is alive
	return nil
}

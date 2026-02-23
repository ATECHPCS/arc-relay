package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/JeremiahChurch/mcp-wrangler/internal/config"
	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
	"github.com/JeremiahChurch/mcp-wrangler/internal/proxy"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

// Server is the main HTTP server for MCP Wrangler.
type Server struct {
	cfg      *config.Config
	servers  *store.ServerStore
	users    *store.UserStore
	proxy    *proxy.Manager
	mux      *http.ServeMux
}

// New creates a new HTTP server.
func New(cfg *config.Config, servers *store.ServerStore, users *store.UserStore, proxyMgr *proxy.Manager) *Server {
	s := &Server{
		cfg:     cfg,
		servers: servers,
		users:   users,
		proxy:   proxyMgr,
		mux:     http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// MCP proxy endpoints (API key auth)
	s.mux.Handle("/mcp/", APIKeyAuth(s.users)(http.HandlerFunc(s.handleMCPProxy)))

	// REST API for server management
	s.mux.HandleFunc("/api/servers", s.handleServers)
	s.mux.HandleFunc("/api/servers/", s.handleServerByID)

	// Health check
	s.mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Web UI (placeholder for now)
	s.mux.HandleFunc("/", s.handleDashboard)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Request logging
	log.Printf("%s %s", r.Method, r.URL.Path)
	s.mux.ServeHTTP(w, r)
}

func (s *Server) ListenAndServe() error {
	addr := s.cfg.Addr()
	log.Printf("MCP Wrangler listening on %s", addr)
	return http.ListenAndServe(addr, s)
}

// handleMCPProxy is the core proxy handler. Routes /mcp/{server-name} to the right backend.
func (s *Server) handleMCPProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed, use POST"}`, http.StatusMethodNotAllowed)
		return
	}

	// Extract server name from path: /mcp/{server-name}
	path := strings.TrimPrefix(r.URL.Path, "/mcp/")
	serverName := strings.Split(path, "/")[0]
	if serverName == "" {
		http.Error(w, `{"error":"missing server name in path"}`, http.StatusBadRequest)
		return
	}

	// Look up server
	srv, err := s.servers.GetByName(serverName)
	if err != nil {
		log.Printf("Error looking up server %s: %v", serverName, err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if srv == nil {
		http.Error(w, fmt.Sprintf(`{"error":"server %q not found"}`, serverName), http.StatusNotFound)
		return
	}

	// Get the backend
	backend, ok := s.proxy.GetBackend(srv.ID)
	if !ok {
		http.Error(w, fmt.Sprintf(`{"error":"server %q is not running"}`, serverName), http.StatusServiceUnavailable)
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}

	var mcpReq mcp.Request
	if err := json.Unmarshal(body, &mcpReq); err != nil {
		http.Error(w, `{"error":"invalid JSON-RPC request"}`, http.StatusBadRequest)
		return
	}

	// Forward to backend
	resp, err := backend.Send(r.Context(), &mcpReq)
	if err != nil {
		log.Printf("Error proxying to server %s: %v", serverName, err)
		errResp := mcp.NewErrorResponse(mcpReq.ID, mcp.ErrCodeInternal, "proxy error: "+err.Error())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(errResp)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// REST API handlers

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listServers(w, r)
	case http.MethodPost:
		s.createServer(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (s *Server) listServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.servers.List()
	if err != nil {
		http.Error(w, `{"error":"failed to list servers"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(servers)
}

func (s *Server) createServer(w http.ResponseWriter, r *http.Request) {
	var srv store.Server
	if err := json.NewDecoder(r.Body).Decode(&srv); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if srv.Name == "" || srv.DisplayName == "" || srv.ServerType == "" {
		http.Error(w, `{"error":"name, display_name, and server_type are required"}`, http.StatusBadRequest)
		return
	}

	if err := s.servers.Create(&srv); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to create server: %s"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(srv)
}

func (s *Server) handleServerByID(w http.ResponseWriter, r *http.Request) {
	// Extract ID from /api/servers/{id} or /api/servers/{id}/start etc.
	path := strings.TrimPrefix(r.URL.Path, "/api/servers/")
	parts := strings.Split(path, "/")
	id := parts[0]

	if id == "" {
		http.Error(w, `{"error":"missing server id"}`, http.StatusBadRequest)
		return
	}

	// Handle actions: /api/servers/{id}/start, /api/servers/{id}/stop
	if len(parts) > 1 {
		switch parts[1] {
		case "start":
			s.startServer(w, r, id)
		case "stop":
			s.stopServer(w, r, id)
		default:
			http.Error(w, `{"error":"unknown action"}`, http.StatusNotFound)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getServer(w, r, id)
	case http.MethodPut:
		s.updateServer(w, r, id)
	case http.MethodDelete:
		s.deleteServer(w, r, id)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (s *Server) getServer(w http.ResponseWriter, r *http.Request, id string) {
	srv, err := s.servers.Get(id)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if srv == nil {
		http.Error(w, `{"error":"server not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(srv)
}

func (s *Server) updateServer(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := s.servers.Get(id)
	if err != nil || existing == nil {
		http.Error(w, `{"error":"server not found"}`, http.StatusNotFound)
		return
	}

	var srv store.Server
	if err := json.NewDecoder(r.Body).Decode(&srv); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	srv.ID = id

	if err := s.servers.Update(&srv); err != nil {
		http.Error(w, `{"error":"failed to update server"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(srv)
}

func (s *Server) deleteServer(w http.ResponseWriter, r *http.Request, id string) {
	// Stop the server first if running
	s.proxy.StopServer(r.Context(), id)

	if err := s.servers.Delete(id); err != nil {
		http.Error(w, `{"error":"failed to delete server"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startServer(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	srv, err := s.servers.Get(id)
	if err != nil || srv == nil {
		http.Error(w, `{"error":"server not found"}`, http.StatusNotFound)
		return
	}

	if err := s.proxy.StartServer(r.Context(), srv); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to start server: %s"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

func (s *Server) stopServer(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if err := s.proxy.StopServer(r.Context(), id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to stop server: %s"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
}

// Web UI handler (placeholder)
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	servers, _ := s.servers.List()
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>MCP Wrangler</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 900px; margin: 40px auto; padding: 0 20px; }
  h1 { color: #333; }
  table { border-collapse: collapse; width: 100%%; }
  th, td { border: 1px solid #ddd; padding: 8px 12px; text-align: left; }
  th { background: #f5f5f5; }
  .status-running { color: green; font-weight: bold; }
  .status-stopped { color: #999; }
  .status-error { color: red; }
</style>
</head>
<body>
<h1>MCP Wrangler</h1>
<h2>Managed Servers</h2>
<table>
<tr><th>Name</th><th>Type</th><th>Status</th><th>Proxy URL</th></tr>`)

	for _, srv := range servers {
		statusClass := "status-" + string(srv.Status)
		proxyURL := fmt.Sprintf("/mcp/%s", srv.Name)
		fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td class="%s">%s</td><td><code>%s</code></td></tr>`,
			srv.DisplayName, srv.ServerType, statusClass, srv.Status, proxyURL)
	}

	fmt.Fprintf(w, `</table>
<p><small>API: POST /api/servers, GET /api/servers, POST /api/servers/{id}/start</small></p>
</body></html>`)
}

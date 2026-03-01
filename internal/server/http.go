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
	"github.com/JeremiahChurch/mcp-wrangler/internal/oauth"
	"github.com/JeremiahChurch/mcp-wrangler/internal/proxy"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
	"github.com/JeremiahChurch/mcp-wrangler/internal/web"
)

// Server is the main HTTP server for MCP Wrangler.
type Server struct {
	cfg         *config.Config
	servers     *store.ServerStore
	users       *store.UserStore
	proxy       *proxy.Manager
	oauthMgr    *oauth.Manager
	accessStore *store.AccessStore
	mux         *http.ServeMux
}

// New creates a new HTTP server.
func New(cfg *config.Config, servers *store.ServerStore, users *store.UserStore, proxyMgr *proxy.Manager, oauthMgr *oauth.Manager, accessStore *store.AccessStore) *Server {
	s := &Server{
		cfg:         cfg,
		servers:     servers,
		users:       users,
		proxy:       proxyMgr,
		oauthMgr:    oauthMgr,
		accessStore: accessStore,
		mux:         http.NewServeMux(),
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

	// Web UI
	webHandlers := web.NewHandlers(s.cfg, s.servers, s.users, s.proxy, s.oauthMgr, s.accessStore)
	webHandlers.RegisterRoutes(s.mux)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL.Path)
	s.mux.ServeHTTP(w, r)
}

func (s *Server) ListenAndServe() error {
	addr := s.cfg.Addr()
	log.Printf("MCP Wrangler listening on %s", addr)
	return http.ListenAndServe(addr, s)
}

// handleMCPProxy is the core proxy handler. Routes /mcp/{server-name} to the right backend.
// Implements Streamable HTTP transport: handles both requests (with id) and notifications (without id).
func (s *Server) handleMCPProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed, use POST"}`, http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/mcp/")
	serverName := strings.Split(path, "/")[0]
	if serverName == "" {
		http.Error(w, `{"error":"missing server name in path"}`, http.StatusBadRequest)
		return
	}

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

	backend, ok := s.proxy.GetBackend(srv.ID)
	if !ok {
		http.Error(w, fmt.Sprintf(`{"error":"server %q is not running"}`, serverName), http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}

	// Parse as generic JSON to detect if it's a notification (no "id" field) or a request
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	method := ""
	if m, ok := raw["method"]; ok {
		json.Unmarshal(m, &method)
	}

	// Check if this is a notification (no "id" field)
	_, hasID := raw["id"]
	if !hasID {
		log.Printf("Proxy %s: notification %s", serverName, method)
		// Forward notification to backend if it supports it, then return 202
		if notifier, ok := backend.(interface {
			SendNotification(n *mcp.Notification) error
		}); ok {
			var notif mcp.Notification
			json.Unmarshal(body, &notif)
			notifier.SendNotification(&notif)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// It's a request — forward and wait for response
	var mcpReq mcp.Request
	if err := json.Unmarshal(body, &mcpReq); err != nil {
		http.Error(w, `{"error":"invalid JSON-RPC request"}`, http.StatusBadRequest)
		return
	}

	log.Printf("Proxy %s: request %s (id=%s)", serverName, mcpReq.Method, string(mcpReq.ID))

	// Access control enforcement
	if s.accessStore != nil {
		if denied := s.checkEndpointAccess(r, srv.ID, &mcpReq); denied != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(denied)
			return
		}
	}

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

// checkEndpointAccess verifies the user has sufficient access level for the requested endpoint.
// Returns an error response if denied, nil if allowed.
func (s *Server) checkEndpointAccess(r *http.Request, serverID string, req *mcp.Request) *mcp.Response {
	user := UserFromContext(r.Context())
	if user == nil {
		return nil // no user context = no enforcement (shouldn't happen behind auth middleware)
	}

	var endpointType, endpointName string

	switch req.Method {
	case "tools/call":
		endpointType = "tool"
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params, &params); err == nil && params.Name != "" {
			endpointName = params.Name
		}
	case "resources/read":
		endpointType = "resource"
		var params struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &params); err == nil && params.URI != "" {
			endpointName = params.URI
		}
	case "prompts/get":
		endpointType = "prompt"
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params, &params); err == nil && params.Name != "" {
			endpointName = params.Name
		}
	default:
		// Pass through list/initialize/ping/etc
		return nil
	}

	if endpointName == "" {
		return nil
	}

	tier := s.accessStore.GetTier(serverID, endpointType, endpointName)
	if !s.accessStore.CheckAccess(user.AccessLevel, tier) {
		log.Printf("Access denied: user %s (level=%s) tried %s %s (tier=%s)",
			user.Username, user.AccessLevel, endpointType, endpointName, tier)
		return mcp.NewErrorResponse(req.ID, mcp.ErrCodeInternal,
			fmt.Sprintf("access denied: requires %s level", tier))
	}

	return nil
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
	path := strings.TrimPrefix(r.URL.Path, "/api/servers/")
	parts := strings.Split(path, "/")
	id := parts[0]

	if id == "" {
		http.Error(w, `{"error":"missing server id"}`, http.StatusBadRequest)
		return
	}

	if len(parts) > 1 {
		switch parts[1] {
		case "start":
			s.startServer(w, r, id)
		case "stop":
			s.stopServer(w, r, id)
		case "enumerate":
			s.enumerateServer(w, r, id)
		case "endpoints":
			s.getEndpoints(w, r, id)
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

func (s *Server) enumerateServer(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	endpoints, err := s.proxy.EnumerateServer(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"enumeration failed: %s"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(endpoints)
}

func (s *Server) getEndpoints(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	endpoints := s.proxy.Endpoints.Get(id)
	if endpoints == nil {
		http.Error(w, `{"error":"no endpoints cached for this server"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(endpoints)
}

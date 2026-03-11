package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahChurch/mcp-wrangler/internal/catalog"
	"github.com/JeremiahChurch/mcp-wrangler/internal/config"
	"github.com/JeremiahChurch/mcp-wrangler/internal/middleware"
	"github.com/JeremiahChurch/mcp-wrangler/internal/oauth"
	"github.com/JeremiahChurch/mcp-wrangler/internal/proxy"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Flash represents a one-time notification message.
type Flash struct {
	Type    string // "success", "danger", "info"
	Message string
}

// ConfigDisplay is a view-friendly representation of server config.
type ConfigDisplay struct {
	Image            string
	Command          string
	Port             int
	URL              string
	HealthCheck      string
	AuthType         string
	EnvKeys          []string
	EnvVars          map[string]string
	OAuthAuthorized  bool
	OAuthScopes      string
	OAuthTokenExpiry string
	// Build fields
	HasBuild       bool
	BuildRuntime   string
	BuildPackage   string
	BuildVersion   string
	BuildGitURL    string
	BuildCustom    bool
	// Image staleness fields
	ImageID          string // sha256 ID of the current image tag
	ImageCreated     string // human-readable image creation time
	ImageAge         string // human-readable age (e.g., "3 days ago")
	ContainerImageID string // sha256 ID used when container was created
	ImageStale       bool   // true if container is running an older image
	IsDocker         bool   // true if this server uses Docker (has an image)
}

// loginRateLimiter tracks failed login attempts per IP.
type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newLoginRateLimiter() *loginRateLimiter {
	rl := &loginRateLimiter{attempts: make(map[string][]time.Time)}
	go rl.cleanup()
	return rl
}

// allow returns true if the IP is allowed to attempt login.
// Limit: 5 attempts per 15 minutes.
func (rl *loginRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-15 * time.Minute)
	recent := rl.attempts[ip]
	filtered := recent[:0]
	for _, t := range recent {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	rl.attempts[ip] = filtered
	return len(filtered) < 5
}

func (rl *loginRateLimiter) record(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.attempts[ip] = append(rl.attempts[ip], time.Now())
}

func (rl *loginRateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-15 * time.Minute)
		for ip, attempts := range rl.attempts {
			filtered := attempts[:0]
			for _, t := range attempts {
				if t.After(cutoff) {
					filtered = append(filtered, t)
				}
			}
			if len(filtered) == 0 {
				delete(rl.attempts, ip)
			} else {
				rl.attempts[ip] = filtered
			}
		}
		rl.mu.Unlock()
	}
}

// Handlers holds dependencies for web UI handlers.
type Handlers struct {
	cfg             *config.Config
	servers         *store.ServerStore
	users           *store.UserStore
	proxy           *proxy.Manager
	oauth           *oauth.Manager
	accessStore     *store.AccessStore
	requestLogs     *store.RequestLogStore
	sessionStore    *store.SessionStore
	middlewareStore  *store.MiddlewareStore
	mwRegistry      *middleware.Registry
	healthMon       *proxy.HealthMonitor
	catalogClient   *catalog.Client
	tmpls           map[string]*template.Template
	csrfSecret      []byte
	loginLimiter    *loginRateLimiter
}

func NewHandlers(cfg *config.Config, servers *store.ServerStore, users *store.UserStore, proxyMgr *proxy.Manager, oauthMgr *oauth.Manager, accessStore *store.AccessStore, requestLogs *store.RequestLogStore, sessionStore *store.SessionStore, middlewareStore *store.MiddlewareStore, mwRegistry *middleware.Registry, healthMon *proxy.HealthMonitor) *Handlers {
	// Generate a per-process CSRF secret. Use session_secret from config if set.
	csrfSecret := []byte(cfg.Auth.SessionSecret)
	if len(csrfSecret) == 0 {
		csrfSecret = make([]byte, 32)
		if _, err := rand.Read(csrfSecret); err != nil {
			panic("failed to generate CSRF secret: " + err.Error())
		}
	}
	h := &Handlers{
		cfg:            cfg,
		servers:        servers,
		users:          users,
		proxy:          proxyMgr,
		oauth:          oauthMgr,
		accessStore:    accessStore,
		requestLogs:    requestLogs,
		sessionStore:   sessionStore,
		middlewareStore: middlewareStore,
		mwRegistry:     mwRegistry,
		healthMon:      healthMon,
		catalogClient:  catalog.NewClient(),
		tmpls:          make(map[string]*template.Template),
		csrfSecret:     csrfSecret,
		loginLimiter:   newLoginRateLimiter(),
	}

	// Parse each page template together with the layout
	pages := []string{"dashboard.html", "server_form.html", "server_detail.html", "users.html", "api_keys.html", "logs.html"}
	for _, page := range pages {
		t := template.Must(template.ParseFS(templateFS, "templates/layout.html", "templates/"+page))
		h.tmpls[page] = t
	}
	// Login is standalone (no layout)
	h.tmpls["login.html"] = template.Must(template.ParseFS(templateFS, "templates/login.html"))

	return h
}

// StartSessionCleanup runs a background goroutine that purges expired sessions.
func (h *Handlers) StartSessionCleanup(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			h.sessionStore.Cleanup()
		}
	}()
}

// RegisterRoutes adds web UI routes to the given mux.
func (h *Handlers) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/login", h.handleLogin)
	mux.HandleFunc("/logout", h.handleLogout)
	mux.HandleFunc("/", h.requireAuth(h.handleDashboard))
	mux.HandleFunc("/servers", h.requireAuth(h.handleServersList))
	mux.HandleFunc("/servers/new", h.requireAuth(h.handleServerNew))
	mux.HandleFunc("/servers/", h.requireAuth(h.handleServerRoutes))
	mux.HandleFunc("/logs", h.requireAuth(h.handleLogs))
	mux.HandleFunc("/users", h.requireAuth(h.handleUsers))
	mux.HandleFunc("/users/", h.requireAuth(h.handleUserRoutes))
	mux.HandleFunc("/api-keys", h.requireAuth(h.handleAPIKeys))
	mux.HandleFunc("/api-keys/", h.requireAuth(h.handleAPIKeyRoutes))
	mux.HandleFunc("/api/catalog/search", h.requireAuth(h.handleCatalogSearch))
	mux.HandleFunc("/api/catalog/discover-oauth", h.requireAuth(h.handleCatalogDiscoverOAuth))
	mux.HandleFunc("/oauth/start/", h.requireAuth(h.handleOAuthStart))
	mux.HandleFunc("/oauth/callback", h.handleOAuthCallback) // No session auth — browser redirect from provider
}

// --- CSRF ---

// csrfToken computes an HMAC-based CSRF token from the session ID.
func (h *Handlers) csrfToken(sessionID string) string {
	mac := hmac.New(sha256.New, h.csrfSecret)
	mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

// validateCSRF checks the CSRF token from form data or X-CSRF-Token header.
func (h *Handlers) validateCSRF(r *http.Request, sessionID string) bool {
	token := r.FormValue("csrf_token")
	if token == "" {
		token = r.Header.Get("X-CSRF-Token")
	}
	if token == "" {
		return false
	}
	expected := h.csrfToken(sessionID)
	return hmac.Equal([]byte(token), []byte(expected))
}

// clientIP extracts the client IP from the request for rate limiting.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.SplitN(fwd, ",", 2)[0]
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- Auth ---

func (h *Handlers) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		user, _, ok := h.sessionStore.Get(cookie.Value)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		// Validate CSRF for state-changing requests
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
			if !h.validateCSRF(r, cookie.Value) {
				http.Error(w, "Invalid or missing CSRF token", http.StatusForbidden)
				return
			}
		}
		ctx := setUser(r.Context(), user)
		ctx = setSessionID(ctx, cookie.Value)
		r = r.WithContext(ctx)
		next(w, r)
	}
}

func (h *Handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.renderLogin(w, "")
		return
	}

	ip := clientIP(r)
	if !h.loginLimiter.allow(ip) {
		h.renderLogin(w, "Too many login attempts. Please try again later.")
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := h.users.Authenticate(username, password)
	if err != nil || user == nil {
		h.loginLimiter.record(ip)
		h.renderLogin(w, "Invalid username or password")
		return
	}

	sessionID, err := generateID()
	if err != nil {
		log.Printf("Failed to generate session ID: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	if err := h.sessionStore.Create(sessionID, user.ID, expiresAt); err != nil {
		log.Printf("Failed to create session: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    sessionID,
		Path:     "/",
		MaxAge:   30 * 24 * 60 * 60, // 30 days
		HttpOnly: true,
		Secure:   strings.HasPrefix(h.cfg.PublicBaseURL(), "https"),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil {
		h.sessionStore.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// --- Dashboard ---

func (h *Handlers) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	servers, _ := h.servers.List()
	users, _ := h.users.List()
	runningCount := 0
	endpointCounts := make(map[string]int) // server ID -> tool count
	for _, s := range servers {
		if s.Status == store.StatusRunning {
			runningCount++
		}
		if ep := h.proxy.Endpoints.Get(s.ID); ep != nil {
			endpointCounts[s.ID] = len(ep.Tools) + len(ep.Resources) + len(ep.Prompts)
		}
	}

	var stats *store.LogStats
	var recentLogs []*store.RequestLog
	var serverCallCounts map[string]int
	if h.requestLogs != nil {
		stats, _ = h.requestLogs.Stats()
		recentLogs, _ = h.requestLogs.Recent(10)
		serverCallCounts, _ = h.requestLogs.ServerTotalCounts()
	}

	h.render(w, r, "dashboard.html", map[string]any{
		"Nav":              "dashboard",
		"User":             getUser(r),
		"Servers":          servers,
		"RunningCount":     runningCount,
		"UserCount":        len(users),
		"EndpointCounts":   endpointCounts,
		"Stats":            stats,
		"RecentLogs":       recentLogs,
		"ServerCallCounts": serverCallCounts,
	})
}

// --- Logs ---

func (h *Handlers) handleLogs(w http.ResponseWriter, r *http.Request) {
	servers, _ := h.servers.List()

	serverFilter := r.URL.Query().Get("server")
	var logs []*store.RequestLog
	if h.requestLogs != nil {
		if serverFilter != "" {
			logs, _ = h.requestLogs.ByServer(serverFilter, 100)
		} else {
			logs, _ = h.requestLogs.Recent(100)
		}
	}

	h.render(w, r, "logs.html", map[string]any{
		"Nav":          "logs",
		"User":         getUser(r),
		"Logs":         logs,
		"Servers":      servers,
		"ServerFilter": serverFilter,
	})
}

// --- Servers ---

func (h *Handlers) handleServersList(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handlers) handleServerNew(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.render(w, r, "server_form.html", map[string]any{
			"Nav":             "servers",
			"User":            getUser(r),
			"IsEdit":          false,
			"Server":          &store.Server{},
			"ServerType":      "stdio",
			"RemoteAuthType":  "none",
			"StdioMode":       "image",
			"BuildRuntime":    "",
			"BuildPackage":    "",
			"BuildVersion":    "",
			"BuildGitURL":     "",
			"BuildDockerfile": "",
		})
		return
	}

	srv, err := h.parseServerForm(r)
	if err != nil {
		h.render(w, r, "server_form.html", h.serverFormData(r, nil, err.Error()))
		return
	}

	if err := h.servers.Create(srv); err != nil {
		h.render(w, r, "server_form.html", h.serverFormData(r, srv, fmt.Sprintf("Failed to create server: %s", err)))
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/servers/%s", srv.ID), http.StatusFound)
}

func (h *Handlers) handleServerRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/servers/")
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]
	if id == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		h.handleServerDetail(w, r, id)
	case "edit":
		h.handleServerEdit(w, r, id)
	case "start":
		h.handleServerStart(w, r, id)
	case "stop":
		h.handleServerStop(w, r, id)
	case "delete":
		h.handleServerDelete(w, r, id)
	case "enumerate":
		h.handleServerEnumerate(w, r, id)
	case "rebuild":
		h.handleServerRebuild(w, r, id)
	case "rebuild-restart":
		h.handleServerRebuildRestart(w, r, id)
	case "recreate":
		h.handleServerRecreate(w, r, id)
	case "access-tier":
		h.handleAccessTier(w, r, id)
	case "middleware":
		h.handleServerMiddleware(w, r, id)
	case "health-check":
		h.handleServerHealthCheck(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handlers) handleServerDetail(w http.ResponseWriter, r *http.Request, id string) {
	srv, err := h.servers.Get(id)
	if err != nil || srv == nil {
		http.NotFound(w, r)
		return
	}

	// Build access tier lookup map: "type:name" -> tier
	tierMap := make(map[string]string)
	if tiers, err := h.accessStore.GetAllTiers(srv.ID); err == nil {
		for _, t := range tiers {
			tierMap[t.EndpointType+":"+t.EndpointName] = t.AccessTier
		}
	}

	// Endpoint usage counts and recent logs
	endpointUsage := make(map[string]store.EndpointCallCount)
	var serverLogs []*store.RequestLog
	if h.requestLogs != nil {
		if counts, err := h.requestLogs.EndpointCounts(srv.ID); err == nil {
			for _, ec := range counts {
				endpointUsage[ec.EndpointName] = ec
			}
		}
		serverLogs, _ = h.requestLogs.ByServer(srv.ID, 20)
	}

	// Middleware configs and events
	var mwConfigs []*store.MiddlewareConfig
	var mwEvents []*store.MiddlewareEvent
	if h.middlewareStore != nil {
		mwConfigs, _ = h.middlewareStore.GetForServer(srv.ID)
		mwEvents, _ = h.middlewareStore.RecentEvents(srv.ID, 20)
	}

	cd := buildConfigDisplay(srv)

	// Populate image staleness info for Docker-managed servers
	if cd.Image != "" && h.proxy.Docker() != nil {
		ctx := r.Context()
		if imgInfo, err := h.proxy.Docker().InspectImage(ctx, cd.Image); err == nil {
			cd.ImageID = imgInfo.ID
			if !imgInfo.Created.IsZero() {
				cd.ImageCreated = imgInfo.Created.Format("2006-01-02 15:04:05")
				cd.ImageAge = humanizeAge(imgInfo.Created)
			}
		}
		if containerID, ok := h.proxy.GetContainerID(srv.ID); ok {
			if cImgID, err := h.proxy.Docker().GetContainerImageID(ctx, containerID); err == nil {
				cd.ContainerImageID = cImgID
				cd.ImageStale = cd.ImageID != "" && cd.ImageID != cImgID
			}
		}
	}

	h.render(w, r, "server_detail.html", map[string]any{
		"Nav":              "servers",
		"User":             getUser(r),
		"Server":           srv,
		"ConfigDisplay":    cd,
		"BaseURL":          h.cfg.PublicBaseURL(),
		"Endpoints":        h.proxy.Endpoints.Get(srv.ID),
		"TierMap":          tierMap,
		"EndpointUsage":    endpointUsage,
		"RecentLogs":       serverLogs,
		"MiddlewareConfigs": mwConfigs,
		"MiddlewareEvents":  mwEvents,
	})
}

func (h *Handlers) handleServerEdit(w http.ResponseWriter, r *http.Request, id string) {
	srv, err := h.servers.Get(id)
	if err != nil || srv == nil {
		http.NotFound(w, r)
		return
	}

	if r.Method == http.MethodGet {
		formData := serverToFormData(srv)
		formData["Nav"] = "servers"
		formData["User"] = getUser(r)
		formData["IsEdit"] = true
		formData["Server"] = srv
		h.render(w, r, "server_form.html", formData)
		return
	}

	updated, err := h.parseServerForm(r)
	if err != nil {
		formData := serverToFormData(srv)
		formData["Nav"] = "servers"
		formData["User"] = getUser(r)
		formData["IsEdit"] = true
		formData["Server"] = srv
		formData["Error"] = err.Error()
		h.render(w, r, "server_form.html", formData)
		return
	}

	// Preserve fields not in the form
	updated.ID = id
	updated.Status = srv.Status
	updated.ErrorMsg = srv.ErrorMsg
	updated.CreatedAt = srv.CreatedAt

	// For build-mode stdio servers, preserve the built image tag if package hasn't changed
	if updated.ServerType == store.ServerTypeStdio {
		var oldCfg, newCfg store.StdioConfig
		if json.Unmarshal(srv.Config, &oldCfg) == nil && json.Unmarshal(updated.Config, &newCfg) == nil {
			if newCfg.Build != nil && oldCfg.Build != nil && oldCfg.Image != "" {
				if newCfg.Build.Package == oldCfg.Build.Package &&
					newCfg.Build.Version == oldCfg.Build.Version &&
					newCfg.Build.Runtime == oldCfg.Build.Runtime {
					newCfg.Image = oldCfg.Image
					updated.Config, _ = json.Marshal(newCfg)
				}
			}
		}
	}

	// For OAuth servers, preserve tokens from the existing config
	if updated.ServerType == store.ServerTypeRemote {
		var oldCfg, newCfg store.RemoteConfig
		if json.Unmarshal(srv.Config, &oldCfg) == nil && json.Unmarshal(updated.Config, &newCfg) == nil {
			if newCfg.Auth.AccessToken == "" && oldCfg.Auth.AccessToken != "" {
				newCfg.Auth.AccessToken = oldCfg.Auth.AccessToken
				newCfg.Auth.RefreshToken = oldCfg.Auth.RefreshToken
				newCfg.Auth.TokenExpiry = oldCfg.Auth.TokenExpiry
			}
			// Preserve registration tracking fields
			if newCfg.Auth.RegisteredRedirectURI == "" {
				newCfg.Auth.RegisteredRedirectURI = oldCfg.Auth.RegisteredRedirectURI
			}
			if newCfg.Auth.RegistrationEndpoint == "" {
				newCfg.Auth.RegistrationEndpoint = oldCfg.Auth.RegistrationEndpoint
			}
			updated.Config, _ = json.Marshal(newCfg)
		}
	}

	if err := h.servers.Update(updated); err != nil {
		log.Printf("Error updating server %s: %v", id, err)
		http.Error(w, "Failed to update server", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/servers/%s", id), http.StatusFound)
}

func (h *Handlers) handleServerStart(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	srv, _ := h.servers.Get(id)
	if srv == nil {
		http.NotFound(w, r)
		return
	}
	// Clean up stale backend/container if server is in error state
	if srv.Status == store.StatusError {
		h.proxy.StopServer(r.Context(), id)
	}
	if err := h.proxy.StartServer(r.Context(), srv); err != nil {
		log.Printf("Error starting server %s: %v", srv.Name, err)
	}
	redirectBack(w, r, fmt.Sprintf("/servers/%s", id))
}

func (h *Handlers) handleServerStop(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.proxy.StopServer(r.Context(), id)
	redirectBack(w, r, fmt.Sprintf("/servers/%s", id))
}

func (h *Handlers) handleServerDelete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.proxy.StopServer(r.Context(), id)
	h.servers.Delete(id)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handlers) handleServerEnumerate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_, err := h.proxy.EnumerateServer(r.Context(), id)
	if err != nil {
		log.Printf("Error enumerating server %s: %v", id, err)
	}
	http.Redirect(w, r, fmt.Sprintf("/servers/%s", id), http.StatusFound)
}

func (h *Handlers) handleServerHealthCheck(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.healthMon != nil {
		health, healthErr := h.healthMon.CheckHealth(r.Context(), id)
		log.Printf("On-demand health check for %s: %s %s", id, health, healthErr)
	}
	http.Redirect(w, r, fmt.Sprintf("/servers/%s", id), http.StatusFound)
}

func (h *Handlers) handleServerRebuild(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	srv, _ := h.servers.Get(id)
	if srv == nil {
		http.NotFound(w, r)
		return
	}
	if err := h.proxy.RebuildImage(r.Context(), srv); err != nil {
		log.Printf("Error rebuilding image for server %s: %v", srv.Name, err)
	}
	redirectBack(w, r, fmt.Sprintf("/servers/%s", id))
}

func (h *Handlers) handleServerRebuildRestart(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	srv, _ := h.servers.Get(id)
	if srv == nil {
		http.NotFound(w, r)
		return
	}
	if err := h.proxy.RebuildAndRestart(r.Context(), srv); err != nil {
		log.Printf("Error rebuild+restart for server %s: %v", srv.Name, err)
	}
	redirectBack(w, r, fmt.Sprintf("/servers/%s", id))
}

func (h *Handlers) handleServerRecreate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	srv, _ := h.servers.Get(id)
	if srv == nil {
		http.NotFound(w, r)
		return
	}
	if err := h.proxy.RecreateContainer(r.Context(), srv); err != nil {
		log.Printf("Error recreating container for server %s: %v", srv.Name, err)
	}
	redirectBack(w, r, fmt.Sprintf("/servers/%s", id))
}

// --- Access Tiers ---

func (h *Handlers) handleAccessTier(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := getUser(r)
	if user == nil || user.Role != "admin" {
		http.Error(w, "Admin access required", http.StatusForbidden)
		return
	}

	var body struct {
		EndpointType string `json:"endpoint_type"`
		EndpointName string `json:"endpoint_name"`
		AccessTier   string `json:"access_tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.AccessTier != "read" && body.AccessTier != "write" && body.AccessTier != "admin" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid access tier"})
		return
	}

	if err := h.accessStore.SetTier(id, body.EndpointType, body.EndpointName, body.AccessTier); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to set tier"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Middleware ---

func (h *Handlers) handleServerMiddleware(w http.ResponseWriter, r *http.Request, serverID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := getUser(r)
	if user == nil || user.Role != "admin" {
		http.Error(w, "Admin access required", http.StatusForbidden)
		return
	}

	var body struct {
		Middleware string          `json:"middleware"`
		Enabled    *bool           `json:"enabled"`
		Config     json.RawMessage `json:"config"`
		Priority   int             `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.Middleware == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "middleware name required"})
		return
	}

	// Validate middleware name
	validNames := map[string]bool{"sanitizer": true, "sizer": true, "alerter": true}
	if !validNames[body.Middleware] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown middleware: " + body.Middleware})
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	cfg := json.RawMessage("{}")
	if body.Config != nil {
		cfg = body.Config
	}

	priority := body.Priority
	if priority == 0 {
		// Default priorities: sanitizer=10, sizer=20, alerter=30
		switch body.Middleware {
		case "sanitizer":
			priority = 10
		case "sizer":
			priority = 20
		case "alerter":
			priority = 30
		default:
			priority = 100
		}
	}

	mc := &store.MiddlewareConfig{
		ServerID:   &serverID,
		Middleware: body.Middleware,
		Enabled:    enabled,
		Config:     cfg,
		Priority:   priority,
	}

	if err := h.middlewareStore.Upsert(mc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Users ---

func (h *Handlers) handleUsers(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	if user == nil || user.Role != "admin" {
		http.Error(w, "Admin access required", http.StatusForbidden)
		return
	}
	users, _ := h.users.List()
	h.render(w, r, "users.html", map[string]any{
		"Nav":   "users",
		"User":  user,
		"Users": users,
	})
}

func (h *Handlers) handleUserRoutes(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	if user == nil || user.Role != "admin" {
		http.Error(w, "Admin access required", http.StatusForbidden)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/users/")
	parts := strings.SplitN(path, "/", 2)

	if parts[0] == "new" && r.Method == http.MethodPost {
		h.handleUserCreate(w, r)
		return
	}
	if len(parts) > 1 && parts[1] == "delete" && r.Method == http.MethodPost {
		h.users.Delete(parts[0])
		http.Redirect(w, r, "/users", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

func (h *Handlers) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	role := r.FormValue("role")
	if role != "admin" {
		role = "user"
	}
	accessLevel := r.FormValue("access_level")
	if accessLevel != "read" && accessLevel != "write" && accessLevel != "admin" {
		accessLevel = "write"
	}

	if username == "" || password == "" {
		users, _ := h.users.List()
		h.render(w, r, "users.html", map[string]any{
			"Nav": "users", "User": getUser(r), "Users": users,
			"Error": "Username and password are required",
		})
		return
	}

	if _, err := h.users.CreateWithAccessLevel(username, password, role, accessLevel); err != nil {
		users, _ := h.users.List()
		h.render(w, r, "users.html", map[string]any{
			"Nav": "users", "User": getUser(r), "Users": users,
			"Error": fmt.Sprintf("Failed to create user: %s", err),
		})
		return
	}
	http.Redirect(w, r, "/users", http.StatusFound)
}

// --- API Keys ---

func (h *Handlers) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	keys, _ := h.users.ListAPIKeys(user.ID)
	h.render(w, r, "api_keys.html", map[string]any{
		"Nav": "apikeys", "User": user, "Keys": keys,
	})
}

func (h *Handlers) handleAPIKeyRoutes(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	path := strings.TrimPrefix(r.URL.Path, "/api-keys/")
	parts := strings.SplitN(path, "/", 2)

	if parts[0] == "new" && r.Method == http.MethodPost {
		name := strings.TrimSpace(r.FormValue("key_name"))
		if name == "" {
			name = "unnamed"
		}
		rawKey, _, err := h.users.CreateAPIKey(user.ID, name)
		if err != nil {
			log.Printf("Error creating API key: %v", err)
			http.Redirect(w, r, "/api-keys", http.StatusFound)
			return
		}
		keys, _ := h.users.ListAPIKeys(user.ID)
		h.render(w, r, "api_keys.html", map[string]any{
			"Nav": "apikeys", "User": user, "Keys": keys, "NewKey": rawKey,
		})
		return
	}

	if len(parts) > 1 && parts[1] == "revoke" && r.Method == http.MethodPost {
		h.users.RevokeAPIKey(parts[0])
		http.Redirect(w, r, "/api-keys", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

// --- OAuth ---

func (h *Handlers) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	serverID := strings.TrimPrefix(r.URL.Path, "/oauth/start/")
	if serverID == "" {
		http.NotFound(w, r)
		return
	}

	srv, err := h.servers.Get(serverID)
	if err != nil || srv == nil {
		http.NotFound(w, r)
		return
	}

	if srv.ServerType != store.ServerTypeRemote {
		http.Error(w, "OAuth is only supported for remote servers", http.StatusBadRequest)
		return
	}

	var cfg store.RemoteConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		http.Error(w, "Invalid server config", http.StatusInternalServerError)
		return
	}

	if cfg.Auth.Type != "oauth" {
		http.Error(w, "Server is not configured for OAuth", http.StatusBadRequest)
		return
	}

	// Auto-re-register if redirect URI has changed (e.g. base URL update)
	if reregistered, err := h.oauth.ReRegisterIfNeeded(r.Context(), serverID, srv, &cfg); err != nil {
		log.Printf("OAuth re-registration failed for %s: %v", srv.Name, err)
		http.Error(w, fmt.Sprintf("OAuth re-registration failed: %s", err), http.StatusInternalServerError)
		return
	} else if reregistered {
		log.Printf("OAuth re-registered for %s with updated redirect URI", srv.Name)
	}

	authURL, err := h.oauth.StartAuthFlow(serverID, cfg.Auth)
	if err != nil {
		log.Printf("Error starting OAuth flow for %s: %v", srv.Name, err)
		http.Error(w, "Failed to start OAuth flow", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, authURL, http.StatusFound)
}

func (h *Handlers) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	if code == "" || state == "" {
		errMsg := r.URL.Query().Get("error")
		errDesc := r.URL.Query().Get("error_description")
		if errMsg != "" {
			http.Error(w, fmt.Sprintf("OAuth error: %s — %s", errMsg, errDesc), http.StatusBadRequest)
			return
		}
		http.Error(w, "Missing code or state parameter", http.StatusBadRequest)
		return
	}

	serverID, err := h.oauth.HandleCallback(r.Context(), state, code)
	if err != nil {
		// On duplicate callbacks (browser double-request), the state is already
		// consumed but tokens were acquired. Redirect to dashboard gracefully.
		if strings.Contains(err.Error(), "unknown or expired OAuth state") {
			log.Printf("OAuth callback: duplicate or expired state (likely double-request), redirecting to dashboard")
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		log.Printf("OAuth callback error: %v", err)
		http.Error(w, fmt.Sprintf("OAuth callback failed: %s", err), http.StatusBadRequest)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/servers/%s", serverID), http.StatusFound)
}

// --- Catalog API ---

func (h *Handlers) handleCatalogSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []catalog.ResolvedServer{})
		return
	}

	results, err := h.catalogClient.Search(r.Context(), q, 20)
	if err != nil {
		log.Printf("Catalog search error: %v", err)
		writeJSON(w, http.StatusOK, []catalog.ResolvedServer{}) // graceful degradation
		return
	}
	if results == nil {
		results = []catalog.ResolvedServer{}
	}

	writeJSON(w, http.StatusOK, results)
}

func (h *Handlers) handleCatalogDiscoverOAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		RemoteURL string `json:"remote_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RemoteURL == "" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	discovery, err := oauth.DiscoverOAuth(r.Context(), body.RemoteURL)
	if err != nil || discovery == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	// If a registration endpoint is available, try dynamic client registration
	if discovery.RegistrationEndpoint != "" {
		reg, err := oauth.RegisterClient(r.Context(), discovery.RegistrationEndpoint, h.oauth.CallbackURL())
		if err != nil {
			log.Printf("Dynamic client registration failed: %v", err)
		} else if reg != nil {
			discovery.ClientID = reg.ClientID
			discovery.ClientSecret = reg.ClientSecret
			discovery.RegisteredRedirectURI = h.oauth.CallbackURL()
		}
	}

	writeJSON(w, http.StatusOK, discovery)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// --- Helpers ---

func (h *Handlers) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	t, ok := h.tmpls[name]
	if !ok {
		log.Printf("Template %s not found", name)
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	// Auto-inject CSRF token from session context
	if sessionID := getSessionID(r.Context()); sessionID != "" {
		data["CSRFToken"] = h.csrfToken(sessionID)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Login page has no layout wrapper
	if name == "login.html" {
		if err := t.ExecuteTemplate(w, "content", data); err != nil {
			log.Printf("Template error: %v", err)
		}
		return
	}

	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("Template error rendering %s: %v", name, err)
	}
}

func (h *Handlers) renderLogin(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>Login - MCP Wrangler</title>
<style>body{font-family:system-ui,sans-serif;background:#f8f9fa;color:#212529;}
.card{background:#fff;border:1px solid #dee2e6;border-radius:8px;padding:1.25rem;max-width:400px;margin:4rem auto;}
.card h2{font-size:1.1rem;margin-bottom:1rem;}
.form-group{margin-bottom:1rem;}
.form-group label{display:block;font-weight:500;margin-bottom:.3rem;font-size:.9rem;}
.form-group input{width:100%%;padding:.45rem .7rem;border:1px solid #dee2e6;border-radius:6px;font-size:.9rem;box-sizing:border-box;}
.btn{display:block;width:100%%;padding:.5rem;background:#0d6efd;color:#fff;border:none;border-radius:6px;cursor:pointer;font-size:.9rem;}
.btn:hover{background:#0b5ed7;}
.alert{background:#f8d7da;color:#842029;padding:.75rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem;}
</style></head><body>
<div class="card"><h2>Log in to MCP Wrangler</h2>`)
	if errMsg != "" {
		fmt.Fprintf(w, `<div class="alert">%s</div>`, errMsg)
	}
	fmt.Fprint(w, `<form method="POST" action="/login">
<div class="form-group"><label for="username">Username</label><input type="text" id="username" name="username" required autofocus></div>
<div class="form-group"><label for="password">Password</label><input type="password" id="password" name="password" required></div>
<button type="submit" class="btn">Log In</button>
</form></div></body></html>`)
}

func (h *Handlers) serverFormData(r *http.Request, srv *store.Server, errMsg string) map[string]any {
	if srv == nil {
		srv = &store.Server{Name: r.FormValue("name"), DisplayName: r.FormValue("display_name")}
	}
	return map[string]any{
		"Nav":            "servers",
		"User":           getUser(r),
		"IsEdit":         false,
		"Server":         srv,
		"ServerType":     r.FormValue("server_type"),
		"RemoteAuthType": r.FormValue("remote_auth_type"),
		"StdioImage":      r.FormValue("stdio_image"),
		"StdioEntrypoint": r.FormValue("stdio_entrypoint"),
		"StdioCommand":    r.FormValue("stdio_command"),
		"StdioEnv":        r.FormValue("stdio_env"),
		"StdioMode":       r.FormValue("stdio_mode"),
		"BuildRuntime":    r.FormValue("build_runtime"),
		"BuildPackage":    r.FormValue("build_package"),
		"BuildVersion":    r.FormValue("build_version"),
		"BuildGitURL":     r.FormValue("build_git_url"),
		"BuildDockerfile": r.FormValue("build_dockerfile"),
		"HTTPImage":      r.FormValue("http_image"),
		"HTTPPort":       r.FormValue("http_port"),
		"HTTPURL":        r.FormValue("http_url"),
		"HTTPHealth":     r.FormValue("http_health"),
		"HTTPEnv":        r.FormValue("http_env"),
		"RemoteURL":          r.FormValue("remote_url"),
		"RemoteToken":        r.FormValue("remote_token"),
		"RemoteHeaderName":   r.FormValue("remote_header_name"),
		"OAuthClientID":      r.FormValue("oauth_client_id"),
		"OAuthClientSecret":  r.FormValue("oauth_client_secret"),
		"OAuthAuthURL":       r.FormValue("oauth_auth_url"),
		"OAuthTokenURL":      r.FormValue("oauth_token_url"),
		"OAuthScopes":        r.FormValue("oauth_scopes"),
		"Error":              errMsg,
	}
}

func (h *Handlers) parseServerForm(r *http.Request) (*store.Server, error) {
	name := strings.TrimSpace(r.FormValue("name"))
	displayName := strings.TrimSpace(r.FormValue("display_name"))
	serverType := r.FormValue("server_type")

	if name == "" || displayName == "" {
		return nil, fmt.Errorf("name and display name are required")
	}

	var configJSON []byte
	var err error

	switch store.ServerType(serverType) {
	case store.ServerTypeStdio:
		img := strings.TrimSpace(r.FormValue("stdio_image"))
		stdioMode := r.FormValue("stdio_mode") // "image" or "build"
		cfg := store.StdioConfig{
			Image:      img,
			Entrypoint: parseCommand(r.FormValue("stdio_entrypoint")),
			Command:    parseCommand(r.FormValue("stdio_command")),
			Env:        parseEnvVars(r.FormValue("stdio_env")),
		}

		if stdioMode == "build" {
			runtime := r.FormValue("build_runtime")
			pkg := strings.TrimSpace(r.FormValue("build_package"))
			version := strings.TrimSpace(r.FormValue("build_version"))
			gitURL := strings.TrimSpace(r.FormValue("build_git_url"))
			customDockerfile := strings.TrimSpace(r.FormValue("build_dockerfile"))

			if pkg == "" && gitURL == "" && customDockerfile == "" {
				return nil, fmt.Errorf("package name, git URL, or custom Dockerfile is required for build mode")
			}
			if runtime != "python" && runtime != "node" {
				return nil, fmt.Errorf("runtime must be python or node")
			}

			cfg.Build = &store.StdioBuildConfig{
				Runtime:    runtime,
				Package:    pkg,
				Version:    version,
				GitURL:     gitURL,
				Dockerfile: customDockerfile,
			}
			cfg.Image = "" // will be set after build
		} else if img == "" {
			return nil, fmt.Errorf("docker image is required for stdio servers")
		}
		configJSON, err = json.Marshal(cfg)

	case store.ServerTypeHTTP:
		img := strings.TrimSpace(r.FormValue("http_image"))
		url := strings.TrimSpace(r.FormValue("http_url"))
		if img == "" && url == "" {
			return nil, fmt.Errorf("docker image or external URL is required for HTTP servers")
		}
		if url != "" {
			if err := validateServerURL(url); err != nil {
				return nil, err
			}
		}
		cfg := store.HTTPConfig{
			Image:       img,
			Port:        parseInt(r.FormValue("http_port")),
			URL:         url,
			HealthCheck: strings.TrimSpace(r.FormValue("http_health")),
			Env:         parseEnvVars(r.FormValue("http_env")),
		}
		configJSON, err = json.Marshal(cfg)

	case store.ServerTypeRemote:
		url := strings.TrimSpace(r.FormValue("remote_url"))
		if url == "" {
			return nil, fmt.Errorf("url is required for remote servers")
		}
		if err := validateServerURL(url); err != nil {
			return nil, err
		}
		auth := store.RemoteAuth{
			Type:       r.FormValue("remote_auth_type"),
			Token:      r.FormValue("remote_token"),
			HeaderName: r.FormValue("remote_header_name"),
		}
		if auth.Type == "oauth" {
			auth.ClientID = strings.TrimSpace(r.FormValue("oauth_client_id"))
			auth.ClientSecret = strings.TrimSpace(r.FormValue("oauth_client_secret"))
			auth.AuthURL = strings.TrimSpace(r.FormValue("oauth_auth_url"))
			auth.TokenURL = strings.TrimSpace(r.FormValue("oauth_token_url"))
			auth.Scopes = strings.TrimSpace(r.FormValue("oauth_scopes"))

			// Auto-discover OAuth endpoints + dynamic client registration when fields are missing
			if auth.ClientID == "" || auth.AuthURL == "" || auth.TokenURL == "" {
				disc, _ := oauth.DiscoverOAuth(r.Context(), url)
				if disc != nil {
					if auth.AuthURL == "" {
						auth.AuthURL = disc.AuthURL
					}
					if auth.TokenURL == "" {
						auth.TokenURL = disc.TokenURL
					}
					if auth.Scopes == "" && len(disc.ScopesSupported) > 0 {
						auth.Scopes = strings.Join(disc.ScopesSupported, " ")
					}
					if disc.RegistrationEndpoint != "" {
						auth.RegistrationEndpoint = disc.RegistrationEndpoint
					}
					if auth.ClientID == "" && disc.RegistrationEndpoint != "" {
						reg, _ := oauth.RegisterClient(r.Context(), disc.RegistrationEndpoint, h.oauth.CallbackURL())
						if reg != nil {
							auth.ClientID = reg.ClientID
							auth.ClientSecret = reg.ClientSecret
							auth.RegisteredRedirectURI = h.oauth.CallbackURL()
						}
					}
				}
			}

			if auth.ClientID == "" || auth.AuthURL == "" || auth.TokenURL == "" {
				return nil, fmt.Errorf("OAuth auto-discovery failed for this server — provide client ID, authorization URL, and token URL manually")
			}
		}
		cfg := store.RemoteConfig{
			URL:  url,
			Auth: auth,
		}
		configJSON, err = json.Marshal(cfg)

	default:
		return nil, fmt.Errorf("invalid server type: %s", serverType)
	}

	if err != nil {
		return nil, fmt.Errorf("encoding config: %w", err)
	}

	return &store.Server{
		Name:        name,
		DisplayName: displayName,
		ServerType:  store.ServerType(serverType),
		Config:      configJSON,
	}, nil
}

func buildConfigDisplay(srv *store.Server) *ConfigDisplay {
	cd := &ConfigDisplay{}
	switch srv.ServerType {
	case store.ServerTypeStdio:
		var cfg store.StdioConfig
		json.Unmarshal(srv.Config, &cfg)
		cd.Image = cfg.Image
		cd.IsDocker = true
		cd.Command = strings.Join(cfg.Command, " ")
		cd.EnvKeys = envKeys(cfg.Env)
		cd.EnvVars = cfg.Env
		if cfg.Build != nil {
			cd.HasBuild = true
			cd.BuildRuntime = cfg.Build.Runtime
			cd.BuildPackage = cfg.Build.Package
			cd.BuildVersion = cfg.Build.Version
			cd.BuildGitURL = cfg.Build.GitURL
			cd.BuildCustom = cfg.Build.Dockerfile != ""
		}
	case store.ServerTypeHTTP:
		var cfg store.HTTPConfig
		json.Unmarshal(srv.Config, &cfg)
		cd.Image = cfg.Image
		cd.IsDocker = cfg.Image != "" // Docker-managed if image set (vs external URL)
		cd.Port = cfg.Port
		cd.URL = cfg.URL
		cd.HealthCheck = cfg.HealthCheck
		cd.EnvKeys = envKeys(cfg.Env)
		cd.EnvVars = cfg.Env
	case store.ServerTypeRemote:
		var cfg store.RemoteConfig
		json.Unmarshal(srv.Config, &cfg)
		cd.URL = cfg.URL
		cd.AuthType = cfg.Auth.Type
		if cfg.Auth.Type == "oauth" {
			cd.OAuthAuthorized = cfg.Auth.AccessToken != ""
			cd.OAuthScopes = cfg.Auth.Scopes
			cd.OAuthTokenExpiry = cfg.Auth.TokenExpiry
		}
	}
	return cd
}

func serverToFormData(srv *store.Server) map[string]any {
	data := map[string]any{
		"ServerType":     string(srv.ServerType),
		"RemoteAuthType": "none",
	}
	switch srv.ServerType {
	case store.ServerTypeStdio:
		var cfg store.StdioConfig
		json.Unmarshal(srv.Config, &cfg)
		data["StdioImage"] = cfg.Image
		data["StdioEntrypoint"] = strings.Join(cfg.Entrypoint, " ")
		data["StdioCommand"] = joinQuoted(cfg.Command)
		data["StdioEnv"] = envToText(cfg.Env)
		if cfg.Build != nil {
			data["StdioMode"] = "build"
			data["BuildRuntime"] = cfg.Build.Runtime
			data["BuildPackage"] = cfg.Build.Package
			data["BuildVersion"] = cfg.Build.Version
			data["BuildGitURL"] = cfg.Build.GitURL
			data["BuildDockerfile"] = cfg.Build.Dockerfile
		} else {
			data["StdioMode"] = "image"
			data["BuildRuntime"] = ""
			data["BuildPackage"] = ""
			data["BuildVersion"] = ""
			data["BuildGitURL"] = ""
			data["BuildDockerfile"] = ""
		}
	case store.ServerTypeHTTP:
		var cfg store.HTTPConfig
		json.Unmarshal(srv.Config, &cfg)
		data["HTTPImage"] = cfg.Image
		data["HTTPPort"] = cfg.Port
		data["HTTPURL"] = cfg.URL
		data["HTTPHealth"] = cfg.HealthCheck
		data["HTTPEnv"] = envToText(cfg.Env)
	case store.ServerTypeRemote:
		var cfg store.RemoteConfig
		json.Unmarshal(srv.Config, &cfg)
		data["RemoteURL"] = cfg.URL
		data["RemoteAuthType"] = cfg.Auth.Type
		data["RemoteToken"] = cfg.Auth.Token
		data["RemoteHeaderName"] = cfg.Auth.HeaderName
		data["OAuthClientID"] = cfg.Auth.ClientID
		data["OAuthClientSecret"] = cfg.Auth.ClientSecret
		data["OAuthAuthURL"] = cfg.Auth.AuthURL
		data["OAuthTokenURL"] = cfg.Auth.TokenURL
		data["OAuthScopes"] = cfg.Auth.Scopes
	}
	return data
}

func humanizeAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

func parseEnvVars(text string) map[string]string {
	env := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			env[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return env
}

// parseCommand splits a command string into arguments, respecting quoted strings.
// e.g. `-c "from foo import bar; bar.run()"` → ["-c", "from foo import bar; bar.run()"]
func parseCommand(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var args []string
	var current strings.Builder
	inQuote := byte(0)

	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			} else {
				current.WriteByte(c)
			}
		case c == '"' || c == '\'':
			inQuote = c
		case c == ' ' || c == '\t':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(c)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

func parseInt(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

func envKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// joinQuoted joins command args, quoting any that contain spaces.
func joinQuoted(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t") {
			parts[i] = `"` + a + `"`
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

func envToText(env map[string]string) string {
	keys := envKeys(env)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+env[k])
	}
	return strings.Join(lines, "\n")
}

// redirectBack sends the user back to the Referer if present, otherwise to fallback.
func redirectBack(w http.ResponseWriter, r *http.Request, fallback string) {
	if ref := r.Header.Get("Referer"); ref != "" {
		http.Redirect(w, r, ref, http.StatusFound)
		return
	}
	http.Redirect(w, r, fallback, http.StatusFound)
}

// validateServerURL checks that a URL is a valid http or https URL.
func validateServerURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must use http or https scheme, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL must include a host")
	}
	return nil
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

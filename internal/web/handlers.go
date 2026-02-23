package web

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/JeremiahChurch/mcp-wrangler/internal/config"
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
	Image       string
	Command     string
	Port        int
	URL         string
	HealthCheck string
	AuthType    string
	EnvKeys     []string
}

// Handlers holds dependencies for web UI handlers.
type Handlers struct {
	cfg     *config.Config
	servers *store.ServerStore
	users   *store.UserStore
	proxy   *proxy.Manager
	tmpls   map[string]*template.Template

	// Simple in-memory session store (POC quality)
	sessions map[string]*store.User
}

func NewHandlers(cfg *config.Config, servers *store.ServerStore, users *store.UserStore, proxyMgr *proxy.Manager) *Handlers {
	h := &Handlers{
		cfg:      cfg,
		servers:  servers,
		users:    users,
		proxy:    proxyMgr,
		tmpls:    make(map[string]*template.Template),
		sessions: make(map[string]*store.User),
	}

	// Parse each page template together with the layout
	pages := []string{"dashboard.html", "server_form.html", "server_detail.html", "users.html", "api_keys.html"}
	for _, page := range pages {
		t := template.Must(template.ParseFS(templateFS, "templates/layout.html", "templates/"+page))
		h.tmpls[page] = t
	}
	// Login is standalone (no layout)
	h.tmpls["login.html"] = template.Must(template.ParseFS(templateFS, "templates/login.html"))

	return h
}

// RegisterRoutes adds web UI routes to the given mux.
func (h *Handlers) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/login", h.handleLogin)
	mux.HandleFunc("/logout", h.handleLogout)
	mux.HandleFunc("/", h.requireAuth(h.handleDashboard))
	mux.HandleFunc("/servers", h.requireAuth(h.handleServersList))
	mux.HandleFunc("/servers/new", h.requireAuth(h.handleServerNew))
	mux.HandleFunc("/servers/", h.requireAuth(h.handleServerRoutes))
	mux.HandleFunc("/users", h.requireAuth(h.handleUsers))
	mux.HandleFunc("/users/", h.requireAuth(h.handleUserRoutes))
	mux.HandleFunc("/api-keys", h.requireAuth(h.handleAPIKeys))
	mux.HandleFunc("/api-keys/", h.requireAuth(h.handleAPIKeyRoutes))
}

// --- Auth ---

func (h *Handlers) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		user, ok := h.sessions[cookie.Value]
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		r = r.WithContext(setUser(r.Context(), user))
		next(w, r)
	}
}

func (h *Handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.renderLogin(w, "")
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := h.users.Authenticate(username, password)
	if err != nil || user == nil {
		h.renderLogin(w, "Invalid username or password")
		return
	}

	sessionID := generateID()
	h.sessions[sessionID] = user
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil {
		delete(h.sessions, cookie.Value)
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
	for _, s := range servers {
		if s.Status == store.StatusRunning {
			runningCount++
		}
	}

	h.render(w, "dashboard.html", map[string]any{
		"Nav":          "dashboard",
		"User":         getUser(r),
		"Servers":      servers,
		"RunningCount": runningCount,
		"UserCount":    len(users),
	})
}

// --- Servers ---

func (h *Handlers) handleServersList(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handlers) handleServerNew(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.render(w, "server_form.html", map[string]any{
			"Nav":            "servers",
			"User":           getUser(r),
			"IsEdit":         false,
			"Server":         &store.Server{},
			"ServerType":     "stdio",
			"RemoteAuthType": "none",
		})
		return
	}

	srv, err := h.parseServerForm(r)
	if err != nil {
		h.render(w, "server_form.html", h.serverFormData(r, nil, err.Error()))
		return
	}

	if err := h.servers.Create(srv); err != nil {
		h.render(w, "server_form.html", h.serverFormData(r, srv, fmt.Sprintf("Failed to create server: %s", err)))
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

	h.render(w, "server_detail.html", map[string]any{
		"Nav":           "servers",
		"User":          getUser(r),
		"Server":        srv,
		"ConfigDisplay": buildConfigDisplay(srv),
		"Host":          r.Host,
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
		h.render(w, "server_form.html", formData)
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
		h.render(w, "server_form.html", formData)
		return
	}

	updated.ID = id
	if err := h.servers.Update(updated); err != nil {
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
	if err := h.proxy.StartServer(r.Context(), srv); err != nil {
		log.Printf("Error starting server %s: %v", srv.Name, err)
	}
	http.Redirect(w, r, fmt.Sprintf("/servers/%s", id), http.StatusFound)
}

func (h *Handlers) handleServerStop(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.proxy.StopServer(r.Context(), id)
	http.Redirect(w, r, fmt.Sprintf("/servers/%s", id), http.StatusFound)
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

// --- Users ---

func (h *Handlers) handleUsers(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	if user == nil || user.Role != "admin" {
		http.Error(w, "Admin access required", http.StatusForbidden)
		return
	}
	users, _ := h.users.List()
	h.render(w, "users.html", map[string]any{
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

	if username == "" || password == "" {
		users, _ := h.users.List()
		h.render(w, "users.html", map[string]any{
			"Nav": "users", "User": getUser(r), "Users": users,
			"Error": "Username and password are required",
		})
		return
	}

	if _, err := h.users.Create(username, password, role); err != nil {
		users, _ := h.users.List()
		h.render(w, "users.html", map[string]any{
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
	h.render(w, "api_keys.html", map[string]any{
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
		h.render(w, "api_keys.html", map[string]any{
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

// --- Helpers ---

func (h *Handlers) render(w http.ResponseWriter, name string, data map[string]any) {
	t, ok := h.tmpls[name]
	if !ok {
		log.Printf("Template %s not found", name)
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
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
		"StdioImage":     r.FormValue("stdio_image"),
		"StdioCommand":   r.FormValue("stdio_command"),
		"StdioEnv":       r.FormValue("stdio_env"),
		"HTTPImage":      r.FormValue("http_image"),
		"HTTPPort":       r.FormValue("http_port"),
		"HTTPURL":        r.FormValue("http_url"),
		"HTTPHealth":     r.FormValue("http_health"),
		"HTTPEnv":        r.FormValue("http_env"),
		"RemoteURL":      r.FormValue("remote_url"),
		"RemoteToken":    r.FormValue("remote_token"),
		"RemoteHeaderName": r.FormValue("remote_header_name"),
		"Error":          errMsg,
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
		if img == "" {
			return nil, fmt.Errorf("docker image is required for stdio servers")
		}
		cfg := store.StdioConfig{
			Image:   img,
			Command: parseCommand(r.FormValue("stdio_command")),
			Env:     parseEnvVars(r.FormValue("stdio_env")),
		}
		configJSON, err = json.Marshal(cfg)

	case store.ServerTypeHTTP:
		img := strings.TrimSpace(r.FormValue("http_image"))
		url := strings.TrimSpace(r.FormValue("http_url"))
		if img == "" && url == "" {
			return nil, fmt.Errorf("docker image or external URL is required for HTTP servers")
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
			return nil, fmt.Errorf("URL is required for remote servers")
		}
		cfg := store.RemoteConfig{
			URL: url,
			Auth: store.RemoteAuth{
				Type:       r.FormValue("remote_auth_type"),
				Token:      r.FormValue("remote_token"),
				HeaderName: r.FormValue("remote_header_name"),
			},
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
		cd.Command = strings.Join(cfg.Command, " ")
		cd.EnvKeys = envKeys(cfg.Env)
	case store.ServerTypeHTTP:
		var cfg store.HTTPConfig
		json.Unmarshal(srv.Config, &cfg)
		cd.Image = cfg.Image
		cd.Port = cfg.Port
		cd.URL = cfg.URL
		cd.HealthCheck = cfg.HealthCheck
		cd.EnvKeys = envKeys(cfg.Env)
	case store.ServerTypeRemote:
		var cfg store.RemoteConfig
		json.Unmarshal(srv.Config, &cfg)
		cd.URL = cfg.URL
		cd.AuthType = cfg.Auth.Type
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
		data["StdioCommand"] = strings.Join(cfg.Command, " ")
		data["StdioEnv"] = envToText(cfg.Env)
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
	}
	return data
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

func parseCommand(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return strings.Fields(text)
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

func envToText(env map[string]string) string {
	keys := envKeys(env)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+env[k])
	}
	return strings.Join(lines, "\n")
}

func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

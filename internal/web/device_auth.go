package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// deviceAuthRequest represents a pending device authorization request.
type deviceAuthRequest struct {
	DeviceCode string
	UserCode   string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Status     string // "pending", "approved", "denied"
	APIKey     string // set on approval
}

// deviceAuthStore is an in-memory store for pending device auth requests.
type deviceAuthStore struct {
	mu       sync.Mutex
	requests map[string]*deviceAuthRequest // keyed by device_code
	byUser   map[string]string             // user_code -> device_code
}

func newDeviceAuthStore() *deviceAuthStore {
	s := &deviceAuthStore{
		requests: make(map[string]*deviceAuthRequest),
		byUser:   make(map[string]string),
	}
	go s.cleanup()
	return s
}

// create generates a new device auth request and returns it.
func (s *deviceAuthStore) create() *deviceAuthRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	deviceCode := generateDeviceCode()
	userCode := generateUserCode()

	req := &deviceAuthRequest{
		DeviceCode: deviceCode,
		UserCode:   userCode,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(5 * time.Minute),
		Status:     "pending",
	}
	s.requests[deviceCode] = req
	s.byUser[userCode] = deviceCode
	return req
}

// get returns the request for a device code, or nil if not found/expired.
func (s *deviceAuthStore) get(deviceCode string) *deviceAuthRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[deviceCode]
	if !ok || time.Now().After(req.ExpiresAt) {
		return nil
	}
	return req
}

// getByUserCode returns the request for a user code, or nil if not found/expired.
func (s *deviceAuthStore) getByUserCode(userCode string) *deviceAuthRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	deviceCode, ok := s.byUser[userCode]
	if !ok {
		return nil
	}
	req, ok := s.requests[deviceCode]
	if !ok || time.Now().After(req.ExpiresAt) {
		return nil
	}
	return req
}

// approve marks a device code as approved with the given API key.
func (s *deviceAuthStore) approve(deviceCode, apiKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req, ok := s.requests[deviceCode]; ok {
		req.Status = "approved"
		req.APIKey = apiKey
	}
}

// deny marks a device code as denied.
func (s *deviceAuthStore) deny(deviceCode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req, ok := s.requests[deviceCode]; ok {
		req.Status = "denied"
	}
}

// consume retrieves and removes a completed (approved/denied) request.
// Returns nil if still pending or not found.
func (s *deviceAuthStore) consume(deviceCode string) *deviceAuthRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[deviceCode]
	if !ok || time.Now().After(req.ExpiresAt) {
		return nil
	}
	if req.Status == "pending" {
		return req // still pending, don't consume
	}
	// Remove consumed request
	delete(s.requests, deviceCode)
	delete(s.byUser, req.UserCode)
	return req
}

// cleanup removes expired requests periodically.
func (s *deviceAuthStore) cleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for code, req := range s.requests {
			if now.After(req.ExpiresAt) {
				delete(s.byUser, req.UserCode)
				delete(s.requests, code)
			}
		}
		s.mu.Unlock()
	}
}

// generateDeviceCode returns a crypto-random hex string.
func generateDeviceCode() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// generateUserCode returns a short, human-readable code like "ABCD-1234".
func generateUserCode() string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I/O/0/1 to avoid confusion
	code := make([]byte, 8)
	for i := range code {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		code[i] = chars[n.Int64()]
	}
	return string(code[:4]) + "-" + string(code[4:])
}

// --- HTTP Handlers ---

// handleDeviceAuthStart handles POST /api/auth/device — initiates device auth flow.
func (h *Handlers) handleDeviceAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	req := h.deviceAuth.create()

	baseURL := h.cfg.PublicBaseURL()
	verificationURL := fmt.Sprintf("%s/auth/device?code=%s", baseURL, req.UserCode)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"device_code":      req.DeviceCode,
		"user_code":        req.UserCode,
		"verification_url": verificationURL,
		"expires_in":       300,
		"interval":         5,
	})
}

// handleDeviceAuthToken handles POST /api/auth/device/token — polls for token.
func (h *Handlers) handleDeviceAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DeviceCode == "" {
		http.Error(w, `{"error":"invalid request, device_code required"}`, http.StatusBadRequest)
		return
	}

	req := h.deviceAuth.consume(body.DeviceCode)
	if req == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "expired_token"})
		return
	}

	w.Header().Set("Content-Type", "application/json")

	switch req.Status {
	case "approved":
		json.NewEncoder(w).Encode(map[string]string{"api_key": req.APIKey})
	case "denied":
		json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
	default: // pending
		json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
	}
}

// handleDeviceAuthPage handles GET/POST /auth/device — browser approval page.
func (h *Handlers) handleDeviceAuthPage(w http.ResponseWriter, r *http.Request) {
	// Check session — if not logged in, redirect to login with return URL
	cookie, err := r.Cookie("session")
	if err != nil {
		returnURL := r.URL.RequestURI()
		http.Redirect(w, r, "/login?next="+url.QueryEscape(returnURL), http.StatusFound)
		return
	}
	user, _, ok := h.sessionStore.Get(cookie.Value)
	if !ok {
		returnURL := r.URL.RequestURI()
		http.Redirect(w, r, "/login?next="+url.QueryEscape(returnURL), http.StatusFound)
		return
	}

	// Inject context for CSRF and user display
	ctx := setUser(r.Context(), user)
	ctx = setSessionID(ctx, cookie.Value)
	r = r.WithContext(ctx)

	if r.Method == http.MethodGet {
		h.handleDeviceAuthPageGet(w, r)
		return
	}

	if r.Method == http.MethodPost {
		// Validate CSRF
		if !h.validateCSRF(r, cookie.Value) {
			http.Error(w, "Invalid or missing CSRF token", http.StatusForbidden)
			return
		}
		h.handleDeviceAuthPagePost(w, r)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (h *Handlers) handleDeviceAuthPageGet(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	userCode := r.URL.Query().Get("code")

	data := map[string]any{
		"Nav":  "",
		"User": user,
	}

	if userCode == "" {
		data["Error"] = "No device code provided. Please run mcp-sync init to start the authorization flow."
		h.render(w, r, "device_auth.html", data)
		return
	}

	req := h.deviceAuth.getByUserCode(strings.ToUpper(userCode))
	if req == nil {
		data["Error"] = "This device code has expired or is invalid. Please run mcp-sync init again."
		h.render(w, r, "device_auth.html", data)
		return
	}

	if req.Status != "pending" {
		data["Error"] = "This device code has already been used."
		h.render(w, r, "device_auth.html", data)
		return
	}

	data["UserCode"] = req.UserCode
	data["DeviceCode"] = req.DeviceCode
	h.render(w, r, "device_auth.html", data)
}

func (h *Handlers) handleDeviceAuthPagePost(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	if user == nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	deviceCode := r.FormValue("device_code")
	action := r.FormValue("action")

	if deviceCode == "" {
		http.Redirect(w, r, "/auth/device", http.StatusFound)
		return
	}

	req := h.deviceAuth.get(deviceCode)
	if req == nil || req.Status != "pending" {
		h.render(w, r, "device_auth.html", map[string]any{
			"Nav":   "",
			"User":  user,
			"Error": "This device code has expired or is invalid.",
		})
		return
	}

	if action == "deny" {
		h.deviceAuth.deny(deviceCode)
		h.render(w, r, "device_auth.html", map[string]any{
			"Nav":    "",
			"User":   user,
			"Denied": true,
		})
		return
	}

	// Approve: create a new API key for this user, inheriting their default profile
	fullUser, _ := h.users.Get(user.ID)
	var deviceProfileID *string
	if fullUser != nil && fullUser.DefaultProfileID != nil {
		deviceProfileID = fullUser.DefaultProfileID
	}
	rawKey, _, err := h.users.CreateAPIKey(user.ID, "mcp-sync device auth", deviceProfileID)
	if err != nil {
		log.Printf("Device auth: failed to create API key for user %s: %v", user.Username, err)
		h.render(w, r, "device_auth.html", map[string]any{
			"Nav":   "",
			"User":  user,
			"Error": "Failed to create API key. Please try again.",
		})
		return
	}

	h.deviceAuth.approve(deviceCode, rawKey)
	log.Printf("Device auth: approved for user %s (key created)", user.Username)

	h.render(w, r, "device_auth.html", map[string]any{
		"Nav":      "",
		"User":     user,
		"Approved": true,
	})
}

// --- Install script handler ---

// handleInstallScript serves GET /install.sh — a templated shell installer.
func (h *Handlers) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	baseURL := h.cfg.PublicBaseURL()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, `#!/bin/bash
set -e

WRANGLER_URL=%q

# Detect platform
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

BINARY="mcp-sync-${OS}-${ARCH}"
DOWNLOAD_URL="${WRANGLER_URL}/download/${BINARY}"

# Determine install location
if [ "$(id -u)" = "0" ]; then
  INSTALL_DIR="/usr/local/bin"
else
  INSTALL_DIR="${HOME}/.local/bin"
  mkdir -p "$INSTALL_DIR"
fi

echo "Downloading mcp-sync for ${OS}/${ARCH}..."
curl -fsSL "$DOWNLOAD_URL" -o "${INSTALL_DIR}/mcp-sync"
chmod +x "${INSTALL_DIR}/mcp-sync"
echo "Installed to ${INSTALL_DIR}/mcp-sync"

# Ensure install dir is in PATH
if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
  echo ""
  echo "Add to your PATH:  export PATH=\"${INSTALL_DIR}:\$PATH\""
fi

# Pass through --token if provided
INVITE_TOKEN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --token) INVITE_TOKEN="$2"; shift 2 ;;
    *) shift ;;
  esac
done

echo ""
echo "Setting up connection to ${WRANGLER_URL}..."
if [ -n "$INVITE_TOKEN" ]; then
  "${INSTALL_DIR}/mcp-sync" init "${WRANGLER_URL}" --token "$INVITE_TOKEN"
else
  "${INSTALL_DIR}/mcp-sync" init "${WRANGLER_URL}"
fi
`, baseURL)
}

// handleDownload serves GET /download/mcp-sync-{os}-{arch}.
// Serves from local /data/downloads/ directory if the file exists,
// otherwise falls back to GitHub releases redirect.
func (h *Handlers) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	binary := strings.TrimPrefix(r.URL.Path, "/download/")
	if binary == "" {
		http.Error(w, "Missing binary name", http.StatusBadRequest)
		return
	}

	// Validate binary name to prevent path traversal
	validBinaries := map[string]bool{
		"mcp-sync-linux-amd64":       true,
		"mcp-sync-linux-arm64":       true,
		"mcp-sync-darwin-arm64":      true,
		"mcp-sync-darwin-amd64":      true,
		"mcp-sync-windows-amd64.exe": true,
	}
	if !validBinaries[binary] {
		http.Error(w, "Unknown binary", http.StatusNotFound)
		return
	}

	// Serve from local downloads directory if available (co-located with DB)
	dbDir := filepath.Dir(h.cfg.Database.Path)
	localPath := filepath.Join(dbDir, "downloads", binary)
	if info, err := os.Stat(localPath); err == nil && !info.IsDir() {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", binary))
		http.ServeFile(w, r, localPath)
		return
	}

	// Fall back to GitHub releases
	githubURL := fmt.Sprintf("https://github.com/JeremiahChurch/mcp-wrangler/releases/latest/download/%s", binary)
	http.Redirect(w, r, githubURL, http.StatusFound)
}

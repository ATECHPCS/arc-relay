package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

// TokenSet holds OAuth tokens for a server.
type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// IsExpired returns true if the token expires within 60 seconds.
func (t *TokenSet) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(60 * time.Second).After(t.ExpiresAt)
}

// PendingAuth tracks an in-progress OAuth authorization flow.
type PendingAuth struct {
	ServerID     string
	CodeVerifier string
	State        string
	CreatedAt    time.Time
}

// Manager handles OAuth flows, token storage, and refresh.
type Manager struct {
	mu      sync.Mutex
	pending map[string]*PendingAuth // state -> PendingAuth
	tokens  map[string]*TokenSet    // serverID -> TokenSet

	servers *store.ServerStore
	baseURL string
}

const (
	pendingAuthExpiry = 10 * time.Minute
	maxPendingFlows   = 100
)

// NewManager creates a new OAuth manager.
func NewManager(servers *store.ServerStore, baseURL string) *Manager {
	m := &Manager{
		pending: make(map[string]*PendingAuth),
		tokens:  make(map[string]*TokenSet),
		servers: servers,
		baseURL: baseURL,
	}
	go m.cleanupPendingLoop()
	return m
}

// cleanupPendingLoop periodically removes expired pending OAuth flows.
func (m *Manager) cleanupPendingLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		now := time.Now()
		for state, pa := range m.pending {
			if now.Sub(pa.CreatedAt) > pendingAuthExpiry {
				delete(m.pending, state)
			}
		}
		m.mu.Unlock()
	}
}

// CallbackURL returns the full OAuth callback URL.
func (m *Manager) CallbackURL() string {
	return m.baseURL + "/oauth/callback"
}

// ReRegisterIfNeeded checks if the current callback URL matches the registered
// redirect URI. If not, it re-discovers OAuth endpoints and performs DCR to get
// new credentials with the correct redirect URI. Returns updated auth config.
func (m *Manager) ReRegisterIfNeeded(ctx context.Context, serverID string, srv *store.Server, cfg *store.RemoteConfig) (bool, error) {
	callbackURL := m.CallbackURL()
	auth := cfg.Auth

	// If redirect URI matches the current callback, nothing to do
	if auth.RegisteredRedirectURI == callbackURL {
		return false, nil
	}

	// If we've never tracked the redirect URI, try to discover a registration
	// endpoint and re-register proactively. This handles the case where existing
	// servers were registered before tracking was added.
	if auth.RegisteredRedirectURI == "" {
		log.Printf("OAuth redirect URI not tracked for server %s, attempting discovery + re-registration", serverID)
	} else {
		log.Printf("OAuth redirect URI changed for server %s: %q → %q, re-registering", serverID, auth.RegisteredRedirectURI, callbackURL)
	}

	// Try using stored registration endpoint first, fall back to re-discovery
	regEndpoint := auth.RegistrationEndpoint
	if regEndpoint == "" {
		// Try discovering from the server URL first, then from the auth URL origin
		// (the OAuth provider may be on a different host than the MCP server)
		var disc *OAuthDiscovery
		for _, tryURL := range []string{cfg.URL, auth.AuthURL} {
			if tryURL == "" {
				continue
			}
			log.Printf("Discovering OAuth for server %s at %s", serverID, tryURL)
			d, err := DiscoverOAuth(ctx, tryURL)
			if err != nil {
				log.Printf("OAuth discovery error for server %s at %s: %v", serverID, tryURL, err)
				continue
			}
			if d != nil && d.RegistrationEndpoint != "" {
				disc = d
				log.Printf("OAuth discovery found registration endpoint: %s", d.RegistrationEndpoint)
				break
			}
		}
		if disc == nil || disc.RegistrationEndpoint == "" {
			if auth.RegisteredRedirectURI == "" {
				// Never tracked — can't re-register, proceed with existing credentials
				log.Printf("No registration endpoint found for server %s, proceeding with existing credentials", serverID)
				return false, nil
			}
			return false, fmt.Errorf("redirect URI changed but cannot re-register: no registration endpoint found (update client credentials manually)")
		}
		regEndpoint = disc.RegistrationEndpoint
	}

	reg, err := RegisterClient(ctx, regEndpoint, callbackURL)
	if err != nil {
		return false, fmt.Errorf("re-registration failed: %w", err)
	}

	// Update auth config with new credentials
	cfg.Auth.ClientID = reg.ClientID
	cfg.Auth.ClientSecret = reg.ClientSecret
	cfg.Auth.RegisteredRedirectURI = callbackURL
	cfg.Auth.RegistrationEndpoint = regEndpoint
	// Clear old tokens since they belong to the old client
	cfg.Auth.AccessToken = ""
	cfg.Auth.RefreshToken = ""
	cfg.Auth.TokenExpiry = ""

	// Persist to DB
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return false, fmt.Errorf("marshaling updated config: %w", err)
	}
	m.servers.UpdateConfig(serverID, configJSON)

	// Clear cached tokens
	m.mu.Lock()
	delete(m.tokens, serverID)
	m.mu.Unlock()

	log.Printf("OAuth re-registered for server %s: new client_id=%s", serverID, reg.ClientID)
	return true, nil
}

// StartAuthFlow begins an OAuth authorization code flow with PKCE.
// Returns the full authorization URL to redirect the user to.
func (m *Manager) StartAuthFlow(serverID string, auth store.RemoteAuth) (string, error) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		return "", fmt.Errorf("generating code verifier: %w", err)
	}
	challenge := computeCodeChallenge(verifier)

	state, err := generateState()
	if err != nil {
		return "", fmt.Errorf("generating state: %w", err)
	}

	m.mu.Lock()
	if len(m.pending) >= maxPendingFlows {
		m.mu.Unlock()
		return "", fmt.Errorf("too many pending OAuth flows, try again later")
	}
	m.pending[state] = &PendingAuth{
		ServerID:     serverID,
		CodeVerifier: verifier,
		State:        state,
		CreatedAt:    time.Now(),
	}
	m.mu.Unlock()

	callbackURL := m.baseURL + "/oauth/callback"

	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {auth.ClientID},
		"redirect_uri":          {callbackURL},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if auth.Scopes != "" {
		params.Set("scope", auth.Scopes)
	}

	authURL := auth.AuthURL + "?" + params.Encode()
	return authURL, nil
}

// HandleCallback processes the OAuth callback, exchanging the code for tokens.
// Returns the server ID on success.
func (m *Manager) HandleCallback(ctx context.Context, state, code string) (string, error) {
	m.mu.Lock()
	pa, ok := m.pending[state]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("unknown or expired OAuth state")
	}
	delete(m.pending, state)
	m.mu.Unlock()

	// Check expiry (10 minute window)
	if time.Since(pa.CreatedAt) > 10*time.Minute {
		return "", fmt.Errorf("OAuth state expired")
	}

	// Load server to get OAuth config
	srv, err := m.servers.Get(pa.ServerID)
	if err != nil || srv == nil {
		return "", fmt.Errorf("server not found: %s", pa.ServerID)
	}

	var cfg store.RemoteConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return "", fmt.Errorf("parsing server config: %w", err)
	}

	callbackURL := m.baseURL + "/oauth/callback"

	// Exchange code for tokens
	tokenSet, err := m.exchangeCode(ctx, cfg.Auth, code, pa.CodeVerifier, callbackURL)
	if err != nil {
		return "", fmt.Errorf("token exchange failed: %w", err)
	}

	// Save tokens to DB
	if err := m.saveTokens(pa.ServerID, srv, &cfg, tokenSet); err != nil {
		return "", fmt.Errorf("saving tokens: %w", err)
	}

	// Cache tokens in memory
	m.mu.Lock()
	m.tokens[pa.ServerID] = tokenSet
	m.mu.Unlock()

	log.Printf("OAuth tokens acquired for server %s (expires %s)", pa.ServerID, tokenSet.ExpiresAt.Format(time.RFC3339))
	return pa.ServerID, nil
}

// GetAccessToken returns a valid access token for a server, refreshing if needed.
func (m *Manager) GetAccessToken(ctx context.Context, serverID string) (string, error) {
	m.mu.Lock()
	ts, ok := m.tokens[serverID]
	m.mu.Unlock()

	// Try loading from DB if not in memory
	if !ok {
		loaded, err := m.loadTokens(serverID)
		if err != nil {
			return "", fmt.Errorf("loading tokens: %w", err)
		}
		if loaded == nil {
			return "", fmt.Errorf("no OAuth tokens for server %s", serverID)
		}
		m.mu.Lock()
		m.tokens[serverID] = loaded
		ts = loaded
		m.mu.Unlock()
	}

	if ts.IsExpired() {
		if err := m.refreshToken(ctx, serverID); err != nil {
			return "", fmt.Errorf("refreshing token: %w", err)
		}
		m.mu.Lock()
		ts = m.tokens[serverID]
		m.mu.Unlock()
	}

	return ts.AccessToken, nil
}

// ForceRefresh forces a token refresh for the given server.
func (m *Manager) ForceRefresh(ctx context.Context, serverID string) error {
	return m.refreshToken(ctx, serverID)
}

// HasTokens returns true if the server has OAuth tokens (in memory or DB).
func (m *Manager) HasTokens(serverID string) bool {
	m.mu.Lock()
	_, ok := m.tokens[serverID]
	m.mu.Unlock()
	if ok {
		return true
	}
	ts, _ := m.loadTokens(serverID)
	return ts != nil
}

// GetTokenInfo returns token expiry info for display purposes.
func (m *Manager) GetTokenInfo(serverID string) *TokenSet {
	m.mu.Lock()
	ts, ok := m.tokens[serverID]
	m.mu.Unlock()
	if ok {
		return ts
	}
	ts, _ = m.loadTokens(serverID)
	if ts != nil {
		m.mu.Lock()
		m.tokens[serverID] = ts
		m.mu.Unlock()
	}
	return ts
}

func (m *Manager) exchangeCode(ctx context.Context, auth store.RemoteAuth, code, verifier, redirectURI string) (*TokenSet, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {auth.ClientID},
		"code_verifier": {verifier},
	}
	if auth.ClientSecret != "" {
		data.Set("client_secret", auth.ClientSecret)
	}

	return m.tokenRequest(ctx, auth.TokenURL, data)
}

func (m *Manager) refreshToken(ctx context.Context, serverID string) error {
	m.mu.Lock()
	ts, ok := m.tokens[serverID]
	m.mu.Unlock()

	if !ok || ts.RefreshToken == "" {
		return fmt.Errorf("no refresh token available for server %s — reauthorize via the server detail page", serverID)
	}

	srv, err := m.servers.Get(serverID)
	if err != nil || srv == nil {
		return fmt.Errorf("server not found: %s", serverID)
	}

	var cfg store.RemoteConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return fmt.Errorf("parsing server config: %w", err)
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {ts.RefreshToken},
		"client_id":     {cfg.Auth.ClientID},
	}
	if cfg.Auth.ClientSecret != "" {
		data.Set("client_secret", cfg.Auth.ClientSecret)
	}
	// Some providers require redirect_uri on refresh requests
	if cfg.Auth.RegisteredRedirectURI != "" {
		data.Set("redirect_uri", cfg.Auth.RegisteredRedirectURI)
	}

	newTS, err := m.tokenRequest(ctx, cfg.Auth.TokenURL, data)
	if err != nil {
		// On invalid_grant (revoked/expired refresh token), clear stale tokens
		// so the UI shows "Not Authorized" with a reauthorize button
		if strings.Contains(err.Error(), "invalid_grant") {
			log.Printf("OAuth refresh token invalid for server %s, clearing stale tokens", serverID)
			m.clearTokens(serverID, srv, &cfg)
		}
		return fmt.Errorf("token refresh failed (reauthorize via server detail page): %w", err)
	}

	// Some providers don't return a new refresh token; keep the old one
	if newTS.RefreshToken == "" {
		newTS.RefreshToken = ts.RefreshToken
	}

	if err := m.saveTokens(serverID, srv, &cfg, newTS); err != nil {
		return fmt.Errorf("saving refreshed tokens: %w", err)
	}

	m.mu.Lock()
	m.tokens[serverID] = newTS
	m.mu.Unlock()

	log.Printf("OAuth tokens refreshed for server %s (expires %s)", serverID, newTS.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (m *Manager) tokenRequest(ctx context.Context, tokenURL string, data url.Values) (*TokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in response: %s", string(body))
	}

	ts := &TokenSet{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
	}
	if tokenResp.ExpiresIn > 0 {
		ts.ExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}
	return ts, nil
}

// clearTokens removes stale tokens from memory and DB so the UI reflects "Not Authorized".
func (m *Manager) clearTokens(serverID string, srv *store.Server, cfg *store.RemoteConfig) {
	cfg.Auth.AccessToken = ""
	cfg.Auth.RefreshToken = ""
	cfg.Auth.TokenExpiry = ""

	configJSON, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("Failed to marshal config when clearing tokens for %s: %v", serverID, err)
		return
	}
	if err := m.servers.UpdateConfig(serverID, configJSON); err != nil {
		log.Printf("Failed to clear tokens in DB for %s: %v", serverID, err)
	}

	m.mu.Lock()
	delete(m.tokens, serverID)
	m.mu.Unlock()
}

func (m *Manager) saveTokens(serverID string, srv *store.Server, cfg *store.RemoteConfig, ts *TokenSet) error {
	cfg.Auth.AccessToken = ts.AccessToken
	cfg.Auth.RefreshToken = ts.RefreshToken
	if !ts.ExpiresAt.IsZero() {
		cfg.Auth.TokenExpiry = ts.ExpiresAt.Format(time.RFC3339)
	} else {
		cfg.Auth.TokenExpiry = ""
	}

	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return m.servers.UpdateConfig(serverID, configJSON)
}

func (m *Manager) loadTokens(serverID string) (*TokenSet, error) {
	srv, err := m.servers.Get(serverID)
	if err != nil || srv == nil {
		return nil, err
	}

	var cfg store.RemoteConfig
	if err := json.Unmarshal(srv.Config, &cfg); err != nil {
		return nil, err
	}

	if cfg.Auth.AccessToken == "" {
		return nil, nil
	}

	ts := &TokenSet{
		AccessToken:  cfg.Auth.AccessToken,
		RefreshToken: cfg.Auth.RefreshToken,
	}
	if cfg.Auth.TokenExpiry != "" {
		t, err := time.Parse(time.RFC3339, cfg.Auth.TokenExpiry)
		if err == nil {
			ts.ExpiresAt = t
		}
	}
	return ts, nil
}

// generateCodeVerifier creates a random PKCE code verifier (43-128 chars, base64url).
func generateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// computeCodeChallenge computes the S256 PKCE challenge from a verifier.
func computeCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// generateState creates a random state parameter for CSRF protection.
func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

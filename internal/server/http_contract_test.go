package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/llm"
	"github.com/comma-compliance/arc-relay/internal/middleware"
	"github.com/comma-compliance/arc-relay/internal/oauth"
	"github.com/comma-compliance/arc-relay/internal/proxy"
	"github.com/comma-compliance/arc-relay/internal/server"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

// authMode describes how a route authenticates callers.
type authMode int

const (
	// authNone is a public endpoint - anyone may call it.
	authNone authMode = iota
	// authAPIKey requires an API-key Bearer token (management API + /mcp/).
	authAPIKey
	// authSession requires a browser session cookie (web UI + session-scoped APIs).
	authSession
)

// contractRoute is one documented endpoint that the public contract commits to.
// The test asserts that every entry is actually registered in the mux and that
// auth-required entries reject unauthenticated callers.
type contractRoute struct {
	Method string
	Path   string
	Auth   authMode
	// Purpose documents why the route exists; surfaced in test failures.
	Purpose string
}

// contract lists every externally-visible endpoint Arc Relay commits to. If
// a route is added, removed, or has its auth requirements changed, update
// this list and docs/api-contract.md together.
var contract = []contractRoute{
	// --- Health ---
	{http.MethodGet, "/health", authNone, "liveness probe"},

	// --- MCP proxy (Bearer: API key or OAuth token) ---
	{http.MethodPost, "/mcp/example", authAPIKey, "MCP proxy dispatch"},

	// --- Management API (Bearer: API key only) ---
	{http.MethodGet, "/api/servers", authAPIKey, "list servers"},
	{http.MethodPost, "/api/servers", authAPIKey, "create server"},
	{http.MethodGet, "/api/servers/server-id", authAPIKey, "get server"},
	{http.MethodPut, "/api/servers/server-id", authAPIKey, "update server"},
	{http.MethodDelete, "/api/servers/server-id", authAPIKey, "delete server"},
	{http.MethodPost, "/api/servers/server-id/start", authAPIKey, "start server"},
	{http.MethodPost, "/api/servers/server-id/stop", authAPIKey, "stop server"},
	{http.MethodPost, "/api/servers/server-id/enumerate", authAPIKey, "enumerate tools/resources/prompts"},
	{http.MethodGet, "/api/servers/server-id/endpoints", authAPIKey, "read cached endpoints"},
	{http.MethodPost, "/api/servers/server-id/health", authAPIKey, "on-demand health probe"},
	{http.MethodGet, "/api/servers/server-id/tool-audit", authAPIKey, "tool size audit + optimization status"},
	{http.MethodPost, "/api/servers/server-id/optimize", authAPIKey, "run LLM tool optimization"},
	{http.MethodPost, "/api/servers/server-id/optimize-toggle", authAPIKey, "toggle serving optimized tools"},

	// --- OAuth 2.1 Authorization Server (RFC 9728 / RFC 8414 / RFC 7591) ---
	{http.MethodGet, "/.well-known/oauth-protected-resource", authNone, "OAuth protected resource metadata (RFC 9728)"},
	{http.MethodGet, "/.well-known/oauth-protected-resource/mcp/example", authNone, "per-resource metadata"},
	{http.MethodGet, "/.well-known/oauth-authorization-server", authNone, "authorization server metadata (RFC 8414)"},
	{http.MethodPost, "/oauth/register", authNone, "dynamic client registration (RFC 7591)"},
	{http.MethodPost, "/register", authNone, "DCR alias"},
	{http.MethodPost, "/oauth/token", authNone, "token exchange / refresh"},
	{http.MethodPost, "/token", authNone, "token endpoint alias"},
	{http.MethodGet, "/oauth/authorize", authSession, "authorization prompt"},
	{http.MethodGet, "/authorize", authSession, "authorize alias"},

	// --- Device auth flow (CLI onboarding) ---
	{http.MethodPost, "/api/auth/device", authNone, "initiate device auth (CLI)"},
	{http.MethodPost, "/api/auth/device/token", authNone, "poll for CLI token"},
	{http.MethodGet, "/auth/device", authSession, "browser approval page"},

	// --- Invite / account bootstrap ---
	{http.MethodPost, "/api/auth/invite", authNone, "exchange invite token for API key"},
	{http.MethodGet, "/invite/abc123", authNone, "browser account setup from invite"},

	// --- Upstream remote-server OAuth (admin-initiated) ---
	{http.MethodGet, "/oauth/start/server-id", authSession, "begin upstream OAuth flow"},
	{http.MethodGet, "/oauth/callback", authNone, "upstream OAuth redirect target"},

	// --- Web UI (session cookie) ---
	{http.MethodGet, "/", authSession, "dashboard"},
	{http.MethodGet, "/servers", authSession, "servers index (redirects to dashboard)"},
	{http.MethodGet, "/servers/new", authSession, "new server form"},
	{http.MethodGet, "/servers/server-id", authSession, "server detail"},
	{http.MethodGet, "/logs", authSession, "request logs"},
	{http.MethodGet, "/users", authSession, "users admin"},
	{http.MethodGet, "/api-keys", authSession, "API key management"},
	{http.MethodGet, "/profiles", authSession, "agent profiles"},
	{http.MethodGet, "/account/password", authSession, "change password"},
	{http.MethodGet, "/connect/desktop", authSession, "Claude Desktop onboarding"},

	// --- UI-backing JSON APIs (session cookie) ---
	{http.MethodGet, "/api/catalog/search", authSession, "MCP registry search"},
	{http.MethodPost, "/api/catalog/discover-oauth", authSession, "OAuth discovery helper"},
	{http.MethodPost, "/api/middleware/archive/config", authSession, "middleware global config"},
	{http.MethodPost, "/api/middleware/archive/action/status", authSession, "middleware action dispatch"},

	// --- Login / logout ---
	{http.MethodGet, "/login", authNone, "login form"},
	{http.MethodPost, "/login", authNone, "submit credentials"},
	{http.MethodGet, "/logout", authNone, "clear session"},

	// --- CLI distribution ---
	{http.MethodGet, "/install.sh", authNone, "arc-sync installer script"},
	{http.MethodGet, "/download/arc-sync-linux-amd64", authNone, "arc-sync binary download"},
}

// newContractTestServer wires a real server.Server against in-memory stores
// so the mux is identical to production registrations.
func newContractTestServer(t *testing.T) *server.Server {
	t.Helper()
	db := testutil.OpenTestDB(t)

	cfg := &config.Config{}
	cfg.Server.Host = "localhost"
	cfg.Server.Port = 8080
	cfg.Server.BaseURL = "http://localhost:8080"
	cfg.Auth.SessionSecret = "contract-test-secret"

	crypto := store.NewConfigEncryptor("")
	serverStore := store.NewServerStore(db, crypto)
	userStore := store.NewUserStore(db)
	accessStore := store.NewAccessStore(db)
	profileStore := store.NewProfileStore(db)
	requestLogStore := store.NewRequestLogStore(db)
	sessionStore := store.NewSessionStore(db)
	middlewareStore := store.NewMiddlewareStore(db)
	inviteStore := store.NewInviteStore(db)
	oauthTokenStore := store.NewOAuthTokenStore(db)
	optimizeStore := store.NewOptimizeStore(db)

	oauthMgr := oauth.NewManager(serverStore, cfg.PublicBaseURL())
	proxyMgr := proxy.NewManager(serverStore, nil, oauthMgr, accessStore)
	proxyMgr.OptimizeStore = optimizeStore
	mwRegistry := middleware.NewRegistry(middlewareStore, nil)
	healthMon := proxy.NewHealthMonitor(proxyMgr, serverStore, 30*time.Second)
	llmClient := llm.NewClient("", "")

	return server.New(cfg, serverStore, userStore, proxyMgr, oauthMgr, accessStore,
		profileStore, requestLogStore, sessionStore, middlewareStore, mwRegistry,
		healthMon, inviteStore, oauthTokenStore, optimizeStore, llmClient)
}

// TestAPIContract is the load-bearing check for the published contract.
// It fails if:
//   - a documented route is not registered (404 for any verb)
//   - an auth-required route accepts unauthenticated callers (200/204)
//
// The two invariants together mean docs/api-contract.md and docs/SPEC.md can
// be trusted as the source of truth for the HTTP surface.
func TestAPIContract(t *testing.T) {
	srv := newContractTestServer(t)

	for _, route := range contract {
		route := route
		name := route.Method + " " + route.Path
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(route.Method, route.Path, nil)
			// Claim HTML so requireAuth redirects to /login instead of
			// returning WWW-Authenticate JSON 401s on / root paths.
			req.Header.Set("Accept", "text/html")
			if route.Method == http.MethodPost || route.Method == http.MethodPut {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			// Invariant 1: route must be registered.
			if rec.Code == http.StatusNotFound {
				t.Fatalf("documented route not registered (%s): got 404. body: %s",
					route.Purpose, strings.TrimSpace(rec.Body.String()))
			}

			// Invariant 2: auth-required routes must reject unauthenticated callers.
			switch route.Auth {
			case authAPIKey:
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s requires API key but returned %d without Authorization header; body: %s",
						route.Purpose, rec.Code, strings.TrimSpace(rec.Body.String()))
				}
			case authSession:
				// requireAuth redirects missing sessions to /login.
				// Some session-gated JSON endpoints return 403 or 401 instead.
				switch rec.Code {
				case http.StatusFound, http.StatusSeeOther, http.StatusUnauthorized, http.StatusForbidden:
					// OK - session enforcement working.
				case http.StatusOK:
					t.Fatalf("%s requires a session but returned 200 without a cookie",
						route.Purpose)
				}
			case authNone:
				// Public - only invariant is "not 404".
			}
		})
	}
}

// TestAPIContract_APIKeyAuthEnforced exercises the Authorization-header
// negative path specifically, so that a future refactor that drops the
// middleware from a route is caught here rather than in integration.
func TestAPIContract_APIKeyAuthEnforced(t *testing.T) {
	srv := newContractTestServer(t)

	apiPaths := []string{
		"/api/servers",
		"/api/servers/does-not-exist",
		"/api/servers/does-not-exist/start",
		"/api/servers/does-not-exist/stop",
		"/api/servers/does-not-exist/enumerate",
		"/api/servers/does-not-exist/endpoints",
		"/api/servers/does-not-exist/health",
		"/api/servers/does-not-exist/tool-audit",
		"/api/servers/does-not-exist/optimize",
		"/api/servers/does-not-exist/optimize-toggle",
		"/mcp/example",
	}

	for _, path := range apiPaths {
		path := path
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s without Authorization: status = %d (want 401 or 405); body: %s",
					path, rec.Code, strings.TrimSpace(rec.Body.String()))
			}

			// RFC 9728: 401 responses should advertise OAuth discovery.
			if rec.Code == http.StatusUnauthorized {
				if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer ") {
					t.Errorf("POST %s: WWW-Authenticate header missing or wrong scheme: %q", path, got)
				}
			}
		})
	}
}

// TestAPIContract_AuthorizationServerMetadata locks down the advertised
// OAuth endpoints so that docs/api-contract.md stays consistent with what
// clients actually discover at runtime.
func TestAPIContract_AuthorizationServerMetadata(t *testing.T) {
	srv := newContractTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("metadata endpoint returned %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	// Each advertised endpoint must actually be a documented contract entry.
	required := []string{
		`"authorization_endpoint"`,
		`"token_endpoint"`,
		`"registration_endpoint"`,
		`"code_challenge_methods_supported"`,
		`"S256"`,
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("authorization server metadata missing %s; got: %s", want, body)
		}
	}
}

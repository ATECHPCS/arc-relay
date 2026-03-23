package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JeremiahChurch/mcp-wrangler/internal/config"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
	"github.com/JeremiahChurch/mcp-wrangler/internal/testutil"
)

// newTestHandlersWithInvites extends newTestHandlers with an InviteStore.
func newTestHandlersWithInvites(t *testing.T) (*Handlers, *store.SessionStore, *store.User, *store.InviteStore) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)
	sessions := store.NewSessionStore(db)
	invites := store.NewInviteStore(db)

	user, err := users.Create("testuser", "pass", "admin")
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	cfg := &config.Config{}
	cfg.Auth.SessionSecret = "test-secret"

	h := &Handlers{
		cfg:          cfg,
		users:        users,
		sessionStore: sessions,
		inviteStore:  invites,
		csrfSecret:   []byte(cfg.Auth.SessionSecret),
	}
	return h, sessions, user, invites
}

func TestInviteExchange_ValidToken(t *testing.T) {
	h, _, user, invites := newTestHandlersWithInvites(t)

	rawToken, _, err := invites.Create(user.ID, nil, user.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("creating invite token: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"token": rawToken})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/invite", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handleInviteExchange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["api_key"] == "" {
		t.Error("response missing api_key")
	}
}

func TestInviteExchange_InvalidToken(t *testing.T) {
	h, _, _, _ := newTestHandlersWithInvites(t)

	body, _ := json.Marshal(map[string]string{"token": "bogus-invalid-token"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/invite", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handleInviteExchange(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestInviteExchange_EmptyBody(t *testing.T) {
	h, _, _, _ := newTestHandlersWithInvites(t)

	body, _ := json.Marshal(map[string]string{})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/invite", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handleInviteExchange(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestInviteExchange_GETReturns405(t *testing.T) {
	h, _, _, _ := newTestHandlersWithInvites(t)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/invite", nil)
	rec := httptest.NewRecorder()

	h.handleInviteExchange(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d; body: %s", rec.Code, http.StatusMethodNotAllowed, rec.Body.String())
	}
}

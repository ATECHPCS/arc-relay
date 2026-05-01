package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/store"
)

func TestRequireCapabilityAllowsAdminWithoutKey(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/skills/x/versions/1.0.0", nil)
	user := &store.User{Role: "admin"}
	if !requireCapability(w, r, user, nil, "skills:write") {
		t.Fatalf("admin user with no api_key should pass; got %d %s", w.Code, w.Body.String())
	}
	if w.Code != 200 {
		t.Errorf("admin path should not write a status; got %d", w.Code)
	}
}

func TestRequireCapabilityAllowsKeyWithMatchingCap(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", nil)
	user := &store.User{Role: "user"} // not admin
	key := &store.APIKey{Capabilities: []string{"skills:write"}}
	if !requireCapability(w, r, user, key, "skills:write") {
		t.Fatalf("non-admin key with matching cap should pass; got %d %s", w.Code, w.Body.String())
	}
}

func TestRequireCapabilityDeniesKeyWithDifferentCap(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", nil)
	user := &store.User{Role: "user"}
	key := &store.APIKey{Capabilities: []string{"recipes:write"}}
	if requireCapability(w, r, user, key, "skills:write") {
		t.Fatal("non-admin key without skills:write should be denied")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403; got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "skills:write") {
		t.Errorf("error body should name the missing capability; got: %s", body)
	}
}

func TestRequireCapabilityDeniesNoUserNoKey(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", nil)
	if requireCapability(w, r, nil, nil, "skills:write") {
		t.Fatal("nil user + nil key should be denied")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403; got %d", w.Code)
	}
}

func TestRequireCapabilityDeniesNonAdminUserWithoutKey(t *testing.T) {
	// Session-cookie auth path: user present, key nil. Only admin role passes.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", nil)
	user := &store.User{Role: "user"}
	if requireCapability(w, r, user, nil, "skills:write") {
		t.Fatal("non-admin session user with no key should be denied")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403; got %d", w.Code)
	}
}

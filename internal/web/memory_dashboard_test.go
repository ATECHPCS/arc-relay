package web_test

import (
	"testing"
)

// TestHandleMemoryIndex_RendersStats — placeholder.
//
// Constructing a *web.Handlers requires 17 stub dependencies (cfg, servers,
// users, proxy, oauth, etc.) most of which the dashboard handlers never
// touch. Wiring up that fixture is out of scope for this phase; the
// handlers are verified via `make build` (compile clean) and manual browser
// smoke test.
//
// Future plan: "Web handler test fixtures" — a shared test helper that
// stubs the unused dependencies so dashboard, sessions, detail, and search
// handlers can all get thin handler-level tests.
func TestHandleMemoryIndex_RendersStats(t *testing.T) {
	t.Skip("dashboard handler rig not yet wired — manual verify via browser")
}

// Package web — memory dashboard handlers.
//
// All handlers in this file are read-only and require session-cookie auth.
// Per-user scoping comes from getUser(r).ID passed into memory.Service methods.
//
// The "## RESEARCH ONLY ..." banner is emitted by every transcript-rendering
// template directly (not via a Go-side wrapper) so the page contains the
// banner even on render errors.
package web

import (
	"net/http"
)

// HandleMemoryIndex renders /memory — the landing page with stats and a
// teaser of recent sessions.
func (h *Handlers) HandleMemoryIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/memory" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)

	stats, err := h.memSvc.Stats()
	if err != nil {
		http.Error(w, "stats: "+err.Error(), http.StatusInternalServerError)
		return
	}
	recent, err := h.memSvc.Recent(user.ID, 5)
	if err != nil {
		http.Error(w, "recent: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Nav":    "memory",
		"User":   user,
		"Stats":  stats,
		"Recent": recent,
	}
	h.render(w, r, "memory.html", data)
}

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
	"strings"

	"github.com/comma-compliance/arc-relay/internal/store"
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

// HandleMemorySessions renders /memory/sessions — a flat table of the user's
// recent sessions, sorted by last_seen_at DESC. No filters in MVP.
func (h *Handlers) HandleMemorySessions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/memory/sessions" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)

	sessions, err := h.memSvc.Recent(user.ID, 50)
	if err != nil {
		http.Error(w, "recent: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Nav":      "memory",
		"User":     user,
		"Sessions": sessions,
	}
	h.render(w, r, "memory_sessions.html", data)
}

// HandleMemorySessionDetail renders /memory/sessions/{id} — the full transcript
// for one session. Returns 404 for missing or other-user session IDs (no
// existence leak — same contract as the API's GET /api/memory/sessions/{id}).
func (h *Handlers) HandleMemorySessionDetail(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	sessionID := strings.TrimPrefix(r.URL.Path, "/memory/sessions/")
	if sessionID == "" {
		http.Redirect(w, r, "/memory/sessions", http.StatusFound)
		return
	}

	sess, msgs, err := h.memSvc.GetSessionWithMessages(user.ID, sessionID, 0)
	if err != nil {
		if strings.Contains(err.Error(), "session not found") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Nav":      "memory",
		"User":     user,
		"Session":  sess,
		"Messages": msgs,
	}
	h.render(w, r, "memory_session_detail.html", data)
}

// HandleMemorySearch renders /memory/search — a form-submit search page.
// Empty q renders just the form. Non-empty q runs the same FTS5/regex
// fallback escalation as the API/CLI search surfaces, then renders ranked
// hits below the form.
func (h *Handlers) HandleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/memory/search" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)
	q := r.URL.Query().Get("q")

	data := map[string]any{
		"Nav":   "memory",
		"User":  user,
		"Query": q,
		"Hits":  nil,
	}
	if q != "" {
		hits, err := h.memSvc.Search(user.ID, q, store.SearchOpts{Limit: 25})
		if err != nil {
			http.Error(w, "search: "+err.Error(), http.StatusInternalServerError)
			return
		}
		data["Hits"] = hits
	}
	h.render(w, r, "memory_search.html", data)
}

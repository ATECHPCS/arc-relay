// Package web hosts HTTP handlers for the memory feature. Lives separately from
// internal/server so the feature stays self-contained and the 100k-line
// http.go doesn't grow further.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/memory"
)

const memoryBodyLimit = 10 << 20 // 10 MiB

// MemoryHandlers wraps memory.Service for HTTP. The userIDFromCtx closure
// extracts the authenticated user's ID from context without importing
// internal/server (which would create an import cycle: server→web→server).
type MemoryHandlers struct {
	svc           *memory.Service
	userIDFromCtx func(context.Context) string
}

// NewMemoryHandlers creates MemoryHandlers. The userIDFromCtx closure is
// typically: func(ctx context.Context) string {
//   if u := server.UserFromContext(ctx); u != nil { return u.ID }
//   return ""
// }
func NewMemoryHandlers(svc *memory.Service, userIDFromCtx func(context.Context) string) *MemoryHandlers {
	return &MemoryHandlers{svc: svc, userIDFromCtx: userIDFromCtx}
}

// HandleIngest writes a transcript delta. Wired at /api/memory/ingest behind
// APIKeyAuth — the user ID is read from request context, never from the body.
func (h *MemoryHandlers) HandleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := h.userIDFromCtx(r.Context())
	if userID == "" {
		// Should never happen — APIKeyAuth runs first. But fail safely.
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, memoryBodyLimit)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// MaxBytesReader returns *http.MaxBytesError when the cap is hit.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body exceeds 10 MiB limit", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "body unreadable", http.StatusBadRequest)
		return
	}

	var req memory.IngestRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	resp, err := h.svc.Ingest(userID, &req)
	if err != nil {
		slog.Warn("memory ingest", "user", userID, "session", req.SessionID, "err", err)
		// User-input validation errors render as 400; storage errors as 500.
		if isClientError(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeMemoryJSON(w, http.StatusOK, resp)
}

func writeMemoryJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// isClientError detects validation-class errors so they render as 400, not 500.
// Matches the prefixes Service.Ingest produces for unrecoverable client mistakes.
func isClientError(err error) bool {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "session_id is required"):
		return true
	case strings.HasPrefix(msg, "platform is required"):
		return true
	case strings.HasPrefix(msg, "unknown platform"):
		return true
	}
	return false
}

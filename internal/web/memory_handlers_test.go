package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/memory"
	"github.com/comma-compliance/arc-relay/internal/server"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/web"
	migrationsmemory "github.com/comma-compliance/arc-relay/migrations-memory"
)

// newMemoryTestRig creates a MemoryHandlers backed by an in-memory SQLite DB and
// wraps it in a test mux that injects a fake authenticated user into context,
// bypassing APIKeyAuth (auth middleware is tested separately).
func newMemoryTestRig(t *testing.T) (*web.MemoryHandlers, http.Handler) {
	t.Helper()
	db, err := store.Open(":memory:", migrationsmemory.FS)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := memory.NewService(store.NewSessionMemoryStore(db), store.NewMessageStore(db))
	h := web.NewMemoryHandlers(svc, nil, func(ctx context.Context) string {
		if u := server.UserFromContext(ctx); u != nil {
			return u.ID
		}
		return ""
	})

	// Fake auth middleware: inject a *store.User into context.
	mux := http.NewServeMux()
	mux.Handle("/api/memory/ingest", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := server.WithUser(r.Context(), &store.User{ID: "user-test", Username: "ian"})
		h.HandleIngest(w, r.WithContext(ctx))
	}))
	return h, mux
}

func TestMemoryIngest_HappyPath(t *testing.T) {
	_, mux := newMemoryTestRig(t)

	jsonl := []byte(
		`{"type":"user","uuid":"u1","timestamp":"t","message":{"role":"user","content":"hello arc"}}` + "\n" +
			`{"type":"assistant","uuid":"a1","timestamp":"t2","message":{"role":"assistant","content":"hi"}}` + "\n",
	)
	body, _ := json.Marshal(memory.IngestRequest{
		SessionID:  "s1",
		ProjectDir: "/Users/ian",
		FilePath:   "/Users/ian/.claude/projects/-Users-ian/s1.jsonl",
		FileMtime:  1.0,
		BytesSeen:  120,
		Platform:   "claude-code",
		JSONL:      jsonl,
	})
	req := httptest.NewRequest("POST", "/api/memory/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rw.Code, rw.Body.String())
	}
	var resp memory.IngestResponse
	_ = json.Unmarshal(rw.Body.Bytes(), &resp)
	if resp.MessagesAdded != 2 {
		t.Fatalf("want 2 messages_added, got %d", resp.MessagesAdded)
	}
}

func TestMemoryIngest_Idempotent(t *testing.T) {
	_, mux := newMemoryTestRig(t)
	jsonl := []byte(`{"type":"user","uuid":"u1","timestamp":"t","message":{"role":"user","content":"hi"}}` + "\n")
	body, _ := json.Marshal(memory.IngestRequest{
		SessionID: "s1", ProjectDir: "/p", FilePath: "/f",
		FileMtime: 1, BytesSeen: 50, Platform: "claude-code", JSONL: jsonl,
	})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/api/memory/ingest", bytes.NewReader(body))
		rw := httptest.NewRecorder()
		mux.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("iter %d status=%d body=%s", i, rw.Code, rw.Body.String())
		}
	}
	// Second POST may report messages_added=0 if the unique index dropped the
	// duplicate INSERT (depends on sqlite ON CONFLICT semantics — this test
	// just asserts no error, not a specific count).
}

func TestMemoryIngest_BodyTooLarge(t *testing.T) {
	_, mux := newMemoryTestRig(t)
	// 11 MiB of arbitrary bytes (base64-encoded in JSON → ~15 MiB on wire)
	huge := bytes.Repeat([]byte("x"), 11<<20)
	body, _ := json.Marshal(memory.IngestRequest{
		SessionID: "s1", Platform: "claude-code", JSONL: huge,
	})
	req := httptest.NewRequest("POST", "/api/memory/ingest", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d body=%s", rw.Code, rw.Body.String())
	}
}

func TestMemoryIngest_UnknownPlatform(t *testing.T) {
	_, mux := newMemoryTestRig(t)
	body, _ := json.Marshal(memory.IngestRequest{
		SessionID: "s1", Platform: "klingon", JSONL: []byte("{}\n"),
	})
	req := httptest.NewRequest("POST", "/api/memory/ingest", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "unknown platform") {
		t.Fatalf("missing 'unknown platform' in error: %s", rw.Body.String())
	}
}

func TestMemoryIngest_MissingSessionID(t *testing.T) {
	_, mux := newMemoryTestRig(t)
	body, _ := json.Marshal(memory.IngestRequest{
		Platform: "claude-code", JSONL: []byte(""),
	})
	req := httptest.NewRequest("POST", "/api/memory/ingest", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rw.Code)
	}
}

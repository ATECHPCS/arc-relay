package memory

import (
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/store"
	migrationsmemory "github.com/comma-compliance/arc-relay/migrations-memory"
)

func newServiceTestRig(t *testing.T) (*Service, *store.SessionMemoryStore) {
	t.Helper()
	db, err := store.Open(":memory:", migrationsmemory.FS)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sessions := store.NewSessionMemoryStore(db)
	messages := store.NewMessageStore(db)
	return NewService(sessions, messages), sessions
}

func seed(t *testing.T, s *Service, userID, sessionID string, contents ...string) {
	t.Helper()
	jsonl := ""
	for i, c := range contents {
		jsonl += `{"type":"user","uuid":"u` + sessionID + string(rune('0'+i)) + `","timestamp":"t","message":{"role":"user","content":` + jsonString(c) + `}}` + "\n"
	}
	if _, err := s.Ingest(userID, &IngestRequest{
		SessionID: sessionID, ProjectDir: "/p", FilePath: "/f",
		FileMtime: 1, BytesSeen: int64(len(jsonl)), Platform: "claude-code", JSONL: []byte(jsonl),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func TestGetSessionWithMessages_HappyPath(t *testing.T) {
	svc, _ := newServiceTestRig(t)
	seed(t, svc, "user-A", "s1", "hello", "world")
	sess, msgs, err := svc.GetSessionWithMessages("user-A", "s1", 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sess == nil || sess.SessionID != "s1" {
		t.Fatalf("wrong session: %+v", sess)
	}
	if sess.UserID != "user-A" {
		t.Fatalf("user mismatch: %q", sess.UserID)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
}

func TestGetSessionWithMessages_NotFound(t *testing.T) {
	svc, _ := newServiceTestRig(t)
	_, _, err := svc.GetSessionWithMessages("user-A", "does-not-exist", 0)
	if err == nil || !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("want 'session not found', got %v", err)
	}
}

func TestGetSessionWithMessages_OtherUserReturns404(t *testing.T) {
	svc, _ := newServiceTestRig(t)
	seed(t, svc, "user-A", "s1", "secret")
	_, _, err := svc.GetSessionWithMessages("user-B", "s1", 0)
	if err == nil || !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("want 'session not found' for wrong user, got %v", err)
	}
}

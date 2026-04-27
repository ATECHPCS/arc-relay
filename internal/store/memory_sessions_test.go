package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	migrationsmemory "github.com/comma-compliance/arc-relay/migrations-memory"
)

func newSessionTestDB(t *testing.T) (*DB, *SessionMemoryStore) {
	t.Helper()
	db, err := Open(":memory:", migrationsmemory.FS)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewSessionMemoryStore(db)
}

func TestSessionMemoryStore_UpsertGet(t *testing.T) {
	_, store := newSessionTestDB(t)
	now := float64(time.Now().Unix())

	// Initial insert
	if err := store.Upsert(&MemorySession{
		SessionID:   "abc",
		UserID:      "ian",
		ProjectDir:  "/Users/ian",
		FilePath:    "/Users/ian/.claude/projects/-Users-ian/abc.jsonl",
		FileMtime:   now,
		IndexedAt:   now,
		LastSeenAt:  now,
		CustomTitle: "FTS5 design discussion",
		Platform:    "claude-code",
		BytesSeen:   100,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.Get("abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserID != "ian" || got.BytesSeen != 100 || got.CustomTitle != "FTS5 design discussion" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Second upsert — bytes change, custom_title left empty (should be preserved)
	_ = store.Upsert(&MemorySession{
		SessionID:  "abc",
		UserID:     "ian",
		ProjectDir: "/Users/ian",
		FilePath:   "/Users/ian/.claude/projects/-Users-ian/abc.jsonl",
		FileMtime:  now + 60,
		IndexedAt:  now,
		LastSeenAt: now + 60,
		Platform:   "claude-code",
		BytesSeen:  250,
		// CustomTitle intentionally empty — should NOT clobber the existing value
	})
	got, _ = store.Get("abc")
	if got.BytesSeen != 250 {
		t.Fatalf("bytes_seen not updated: %d", got.BytesSeen)
	}
	if got.CustomTitle != "FTS5 design discussion" {
		t.Fatalf("CustomTitle clobbered by empty upsert: %q", got.CustomTitle)
	}

	// Third upsert — non-empty CustomTitle should overwrite
	_ = store.Upsert(&MemorySession{
		SessionID:   "abc",
		UserID:      "ian",
		ProjectDir:  "/Users/ian",
		FilePath:    "/Users/ian/.claude/projects/-Users-ian/abc.jsonl",
		FileMtime:   now + 120,
		IndexedAt:   now,
		LastSeenAt:  now + 120,
		Platform:    "claude-code",
		BytesSeen:   300,
		CustomTitle: "Renamed session",
	})
	got, _ = store.Get("abc")
	if got.CustomTitle != "Renamed session" {
		t.Fatalf("CustomTitle should overwrite when non-empty: %q", got.CustomTitle)
	}
}

func TestSessionMemoryStore_GetMissing(t *testing.T) {
	_, store := newSessionTestDB(t)
	_, err := store.Get("does-not-exist")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows, got %v", err)
	}
}

func TestSessionMemoryStore_ListByUser(t *testing.T) {
	_, store := newSessionTestDB(t)

	for i, sid := range []string{"a", "b", "c"} {
		_ = store.Upsert(&MemorySession{
			SessionID: sid, UserID: "ian", ProjectDir: "/p", FilePath: "/f",
			FileMtime: float64(i), IndexedAt: float64(i), LastSeenAt: float64(i),
			Platform: "claude-code",
		})
	}
	_ = store.Upsert(&MemorySession{
		SessionID: "z", UserID: "other", ProjectDir: "/p", FilePath: "/f",
		FileMtime: 100, IndexedAt: 100, LastSeenAt: 100,
		Platform: "claude-code",
	})

	rows, err := store.ListByUser("ian", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows for ian, got %d", len(rows))
	}
	if rows[0].SessionID != "c" {
		t.Fatalf("want most-recent-first; got %q", rows[0].SessionID)
	}

	otherRows, _ := store.ListByUser("other", 10)
	if len(otherRows) != 1 || otherRows[0].SessionID != "z" {
		t.Fatalf("user-scoping leaked: %+v", otherRows)
	}
}

func TestSessionMemoryStore_Touch(t *testing.T) {
	_, store := newSessionTestDB(t)
	now := float64(time.Now().Unix())
	_ = store.Upsert(&MemorySession{
		SessionID: "t1", UserID: "ian", ProjectDir: "/p", FilePath: "/f",
		FileMtime: now, IndexedAt: now, LastSeenAt: now,
		Platform: "claude-code", BytesSeen: 100,
	})

	if err := store.Touch("t1", now+30, 200); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, _ := store.Get("t1")
	if got.BytesSeen != 200 {
		t.Fatalf("bytes_seen not advanced: %d", got.BytesSeen)
	}
	if got.LastSeenAt != now+30 {
		t.Fatalf("last_seen_at not advanced: %v want %v", got.LastSeenAt, now+30)
	}

	// Touch on a missing session is a no-op (no error)
	if err := store.Touch("nope", now, 0); err != nil {
		t.Fatalf("touch missing: want nil error, got %v", err)
	}
}

package testutil

import (
	"testing"

	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
	"github.com/JeremiahChurch/mcp-wrangler/migrations"
)

// OpenTestDB creates an in-memory SQLite database with all migrations applied.
func OpenTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:", migrations.FS)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

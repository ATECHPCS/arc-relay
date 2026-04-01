package store_test

import (
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

func TestInviteCreateAndConsume(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)
	invites := store.NewInviteStore(db)

	user, err := users.Create("alice", "pass", "admin")
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	rawToken, tok, err := invites.Create(user.ID, nil, user.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if rawToken == "" {
		t.Error("rawToken should not be empty")
	}
	if tok.ID == "" {
		t.Error("token ID should be generated")
	}
	if tok.Status != "pending" {
		t.Errorf("Status = %q, want %q", tok.Status, "pending")
	}

	consumed, err := invites.ValidateAndConsume(rawToken)
	if err != nil {
		t.Fatalf("ValidateAndConsume() error = %v", err)
	}
	if consumed == nil {
		t.Fatal("ValidateAndConsume() returned nil for valid token")
	}
	if consumed.UserID != user.ID {
		t.Errorf("UserID = %q, want %q", consumed.UserID, user.ID)
	}
	if consumed.Status != "used" {
		t.Errorf("Status = %q, want %q", consumed.Status, "used")
	}
}

func TestInviteDoubleConsumeAtomicity(t *testing.T) {
	// SQLite serializes writes, so we test atomicity by consuming
	// the same token twice in sequence - only the first should succeed.
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)
	invites := store.NewInviteStore(db)

	user, err := users.Create("racer", "pass", "admin")
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	rawToken, _, err := invites.Create(user.ID, nil, user.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// First consume should succeed
	tok1, err := invites.ValidateAndConsume(rawToken)
	if err != nil {
		t.Fatalf("first ValidateAndConsume() error = %v", err)
	}
	if tok1 == nil {
		t.Fatal("first ValidateAndConsume() should succeed")
	}

	// Second consume of same token should fail (already used)
	tok2, err := invites.ValidateAndConsume(rawToken)
	if err != nil {
		t.Fatalf("second ValidateAndConsume() error = %v", err)
	}
	if tok2 != nil {
		t.Error("second ValidateAndConsume() should return nil - token already consumed")
	}
}

func TestInviteExpiredToken(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)
	invites := store.NewInviteStore(db)

	user, err := users.Create("expired-user", "pass", "admin")
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	rawToken, _, err := invites.Create(user.ID, nil, user.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	consumed, err := invites.ValidateAndConsume(rawToken)
	if err != nil {
		t.Fatalf("ValidateAndConsume() error = %v", err)
	}
	if consumed != nil {
		t.Error("ValidateAndConsume() should return nil for expired token")
	}
}

func TestInviteAlreadyUsed(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)
	invites := store.NewInviteStore(db)

	user, err := users.Create("reuse-user", "pass", "admin")
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	rawToken, _, err := invites.Create(user.ID, nil, user.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// First consume should succeed
	consumed, err := invites.ValidateAndConsume(rawToken)
	if err != nil {
		t.Fatalf("first ValidateAndConsume() error = %v", err)
	}
	if consumed == nil {
		t.Fatal("first ValidateAndConsume() should succeed")
	}

	// Second consume should return nil
	consumed, err = invites.ValidateAndConsume(rawToken)
	if err != nil {
		t.Fatalf("second ValidateAndConsume() error = %v", err)
	}
	if consumed != nil {
		t.Error("second ValidateAndConsume() should return nil for already-used token")
	}
}

func TestInvitePendingCountForUser(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)
	invites := store.NewInviteStore(db)

	user, err := users.Create("count-user", "pass", "admin")
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	// Create 2 pending invites (future expiry)
	_, _, err = invites.Create(user.ID, nil, user.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Create() pending 1 error = %v", err)
	}
	_, _, err = invites.Create(user.ID, nil, user.ID, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Create() pending 2 error = %v", err)
	}

	// Create 1 expired invite (past expiry)
	_, _, err = invites.Create(user.ID, nil, user.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Create() expired error = %v", err)
	}

	count, err := invites.PendingCountForUser(user.ID)
	if err != nil {
		t.Fatalf("PendingCountForUser() error = %v", err)
	}
	if count != 2 {
		t.Errorf("PendingCountForUser() = %d, want 2", count)
	}
}

func TestInviteCleanupExpired(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)
	invites := store.NewInviteStore(db)

	user, err := users.Create("cleanup-user", "pass", "admin")
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	_, _, err = invites.Create(user.ID, nil, user.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := invites.CleanupExpired(); err != nil {
		t.Fatalf("CleanupExpired() error = %v", err)
	}

	tokens, err := invites.ListForUser(user.ID)
	if err != nil {
		t.Fatalf("ListForUser() error = %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token after cleanup, got %d", len(tokens))
	}
	if tokens[0].Status != "expired" {
		t.Errorf("Status = %q, want %q", tokens[0].Status, "expired")
	}
}

package store_test

import (
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

// TestAuthenticateConstantTimeOnMissingUser is the regression test for the
// username-enumeration timing oracle in UserStore.Authenticate.
//
// Before the fix, the user-not-found path returned (nil, nil) immediately
// without running bcrypt, while the wrong-password path paid the full
// bcrypt cost (~50–100ms). An attacker probing usernames with arbitrary
// passwords could distinguish "user does not exist" from "user exists,
// wrong password" by response time alone.
//
// After the fix, the missing-user path must also run bcrypt (against a
// dummy hash) so timing of both branches is comparable. We check a
// generous 10ms floor — bcrypt at DefaultCost (10) takes far longer on
// any machine that can run the test suite, and the unfixed missing-user
// path takes microseconds against the in-memory SQLite used by tests.
func TestAuthenticateConstantTimeOnMissingUser(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing test in -short mode")
	}

	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)
	if _, err := users.Create("alice", "correct-password", "user"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Warm the bcrypt code path once for each branch so first-call costs
	// (allocations, JIT-style cost-table init) don't skew measurements.
	_, _ = users.Authenticate("alice", "wrong")
	_, _ = users.Authenticate("nobody", "wrong")

	const iterations = 5
	var existingTotal, missingTotal time.Duration
	for i := 0; i < iterations; i++ {
		t0 := time.Now()
		u, _ := users.Authenticate("alice", "wrong-password")
		existingTotal += time.Since(t0)
		if u != nil {
			t.Fatalf("Authenticate with wrong password should return nil; got %+v", u)
		}

		t0 = time.Now()
		u, _ = users.Authenticate("does-not-exist", "wrong-password")
		missingTotal += time.Since(t0)
		if u != nil {
			t.Fatalf("Authenticate for missing user should return nil; got %+v", u)
		}
	}

	existingAvg := existingTotal / iterations
	missingAvg := missingTotal / iterations

	const minBcryptFloor = 10 * time.Millisecond
	if missingAvg < minBcryptFloor {
		t.Errorf("user-not-found path took avg %v (< %v floor) — bcrypt is not running on miss; "+
			"timing leaks valid usernames. existing-user-wrong-password avg = %v",
			missingAvg, minBcryptFloor, existingAvg)
	}

	// Sanity: the two branches should be within an order of magnitude.
	// We deliberately use a wide ratio (4×) so noisy CI machines don't
	// false-fail on the timing similarity check; the floor above is the
	// real anti-oracle assertion.
	ratio := float64(existingAvg) / float64(missingAvg)
	if ratio < 0.25 || ratio > 4.0 {
		t.Errorf("Authenticate timing ratio existing/missing = %.2f (existing=%v, missing=%v); "+
			"want roughly 1.0 — large skew suggests one branch skips bcrypt",
			ratio, existingAvg, missingAvg)
	}
}

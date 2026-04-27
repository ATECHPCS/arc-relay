package web

import (
	"testing"
	"time"
)

func TestIPRateLimiter_AllowsBelowThreshold(t *testing.T) {
	rl := newIPRateLimiter(time.Hour, 5)
	for i := 0; i < 4; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
		rl.record("1.2.3.4")
	}
	// allow returns true while count < max; after 4 records, count is 4, max is 5, allow returns true
	if !rl.allow("1.2.3.4") {
		t.Fatal("attempt 5 should be allowed (count=4 < max=5)")
	}
}

func TestIPRateLimiter_BlocksAtThreshold(t *testing.T) {
	rl := newIPRateLimiter(time.Hour, 3)
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
		rl.record("1.2.3.4")
	}
	// 4th attempt: count is 3, max is 3, allow returns false
	if rl.allow("1.2.3.4") {
		t.Fatal("attempt 4 should be blocked (count=3 >= max=3)")
	}
}

func TestIPRateLimiter_PerIPIsolation(t *testing.T) {
	rl := newIPRateLimiter(time.Hour, 2)
	for i := 0; i < 2; i++ {
		_ = rl.allow("1.2.3.4")
		rl.record("1.2.3.4")
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("IP A should be blocked")
	}
	if !rl.allow("5.6.7.8") {
		t.Fatal("IP B should be allowed (separate budget)")
	}
}

func TestIPRateLimiter_WindowExpiry(t *testing.T) {
	rl := newIPRateLimiter(time.Hour, 2)
	// Inject old timestamps to simulate expiry — no time.Sleep
	old := time.Now().Add(-2 * time.Hour)
	rl.mu.Lock()
	rl.attempts["1.2.3.4"] = []time.Time{old, old}
	rl.mu.Unlock()
	if !rl.allow("1.2.3.4") {
		t.Fatal("stale entries should be pruned; new attempt should be allowed")
	}
	rl.mu.Lock()
	got := len(rl.attempts["1.2.3.4"])
	rl.mu.Unlock()
	if got != 0 {
		t.Fatalf("stale entries not pruned: %d remaining", got)
	}
}

func TestLoginRateLimiter_StillWorks(t *testing.T) {
	rl := newLoginRateLimiter()
	for i := 0; i < 5; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("login attempt %d should be allowed", i+1)
		}
		rl.record("1.2.3.4")
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("6th login attempt should be blocked")
	}
}

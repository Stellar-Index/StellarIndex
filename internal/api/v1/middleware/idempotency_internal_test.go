package middleware

import (
	"net/http"
	"testing"
	"time"
)

// TestIdempotencyStore_PutSweepsExpiredEntries proves an idempotency
// key that's used exactly once (never replayed) does not live in the
// store forever: once its TTL has passed, a LATER put() on a
// different key must evict it, not just leave it resident until the
// process restarts.
func TestIdempotencyStore_PutSweepsExpiredEntries(t *testing.T) {
	store := NewIdempotencyStore(time.Minute)
	now := time.Now()
	store.now = func() time.Time { return now }

	store.put("account-1:key-a", http.StatusCreated, http.Header{}, []byte("a"))
	store.put("account-1:key-b", http.StatusCreated, http.Header{}, []byte("b"))

	if got := len(store.entries); got != 2 {
		t.Fatalf("entries after two puts = %d, want 2", got)
	}

	// Advance well past the TTL. Neither key-a nor key-b is ever
	// looked up again (no replay), which is exactly the
	// create-then-never-replay pattern that leaked unbounded before
	// the fix.
	now = now.Add(2 * time.Minute)

	store.put("account-1:key-c", http.StatusCreated, http.Header{}, []byte("c"))

	if got := len(store.entries); got != 1 {
		t.Fatalf("entries after expiry sweep = %d, want 1 (only the fresh key-c); "+
			"stale keys from a one-shot idempotency key must not survive past their TTL", got)
	}
	if _, ok := store.entries["account-1:key-c"]; !ok {
		t.Fatalf("expected key-c to remain in the store")
	}
}

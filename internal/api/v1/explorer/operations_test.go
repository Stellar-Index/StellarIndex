package explorer

import (
	"testing"
	"time"
)

// TestOpsDirCache covers the /v1/operations directory first-page cache: a miss
// on empty, a hit within TTL, per-limit keying, and expiry after opsDirTTL.
func TestOpsDirCache(t *testing.T) {
	var c opsDirCache

	// Miss on an empty (zero-value) cache — must not panic on the nil map.
	if _, ok := c.get(50); ok {
		t.Fatal("empty cache returned a hit")
	}

	view := OperationsView{NextCursor: "abc", Operations: make([]OpView, 3)}
	c.put(50, view)

	// Hit within TTL.
	got, ok := c.get(50)
	if !ok {
		t.Fatal("expected a hit right after put")
	}
	if got.NextCursor != "abc" || len(got.Operations) != 3 {
		t.Fatalf("cached view mismatch: %+v", got)
	}

	// Keyed by limit — a different limit is a distinct entry (miss).
	if _, ok := c.get(200); ok {
		t.Fatal("limit=200 should miss when only limit=50 was cached")
	}

	// Expiry: backdate the entry past the TTL and confirm it's evicted on read.
	c.mu.Lock()
	e := c.entries[50]
	e.expires = time.Now().Add(-time.Second)
	c.entries[50] = e
	c.mu.Unlock()
	if _, ok := c.get(50); ok {
		t.Fatal("expired entry should miss")
	}
}

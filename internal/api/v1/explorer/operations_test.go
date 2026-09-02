package explorer

import (
	"testing"
	"time"
)

// TestOpsDirCache covers the /v1/operations directory first-page cache: a miss
// on empty, a hit within TTL, per-limit keying, and — since #444 / #332 F2
// (2026-09-02) — that an entry past opsDirTTL is still RETURNED, marked
// fresh=false, instead of reading as a miss. The stale-serve contract is the
// point of the change: at 3s fill-on-miss, nearly every production hit missed
// and paid the lake read inline. Staleness is now the caller's judgment (it
// serves the entry with flags.stale and kicks a detached rebuild), exactly as
// hot_reads.go / contract_detail_cache.go do.
func TestOpsDirCache(t *testing.T) {
	var c opsDirCache

	// Miss on an empty (zero-value) cache — must not panic on the nil map.
	if _, ok, _ := c.get(50); ok {
		t.Fatal("empty cache returned a hit")
	}

	view := OperationsView{NextCursor: "abc", Operations: make([]OpView, 3)}
	c.put(50, view)

	// Hit within TTL, marked fresh.
	e, ok, fresh := c.get(50)
	if !ok {
		t.Fatal("expected a hit right after put")
	}
	if !fresh {
		t.Error("entry right after put reported stale")
	}
	if e.view.NextCursor != "abc" || len(e.view.Operations) != 3 {
		t.Fatalf("cached view mismatch: %+v", e.view)
	}
	if e.cachedAt.IsZero() {
		t.Error("entry carries no fill time — the served as_of would be a lie")
	}

	// Keyed by limit — a different limit is a distinct entry (miss).
	if _, ok, _ := c.get(200); ok {
		t.Fatal("limit=200 should miss when only limit=50 was cached")
	}

	// Past the TTL: still returned, marked stale, with its real fill time.
	c.mu.Lock()
	backdated := time.Now().Add(-2 * opsDirTTL)
	entry := c.entries[50]
	entry.cachedAt = backdated
	c.entries[50] = entry
	c.mu.Unlock()

	e, ok, fresh = c.get(50)
	if !ok {
		t.Fatal("expired entry must still be RETURNED (stale-serve), not dropped")
	}
	if fresh {
		t.Error("entry older than opsDirTTL reported fresh")
	}
	if e.view.NextCursor != "abc" {
		t.Errorf("stale entry lost its payload: %+v", e.view)
	}
	if !e.cachedAt.Equal(backdated) {
		t.Errorf("stale entry cachedAt = %v, want the real fill time %v", e.cachedAt, backdated)
	}
}

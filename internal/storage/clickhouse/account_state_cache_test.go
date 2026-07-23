package clickhouse

import (
	"testing"
	"time"
)

func TestAccountStateCache(t *testing.T) {
	t.Parallel()
	c := newAccountStateCache()

	if _, ok := c.get("G1"); ok {
		t.Fatal("empty cache returned a hit")
	}
	c.put("G1", AccountState{Exists: true, Balance: 42}, time.Now())
	got, ok := c.get("G1")
	if !ok || got.Balance != 42 {
		t.Fatalf("get after put = (%+v, %t), want balance 42 hit", got, ok)
	}

	// Expiry.
	c.put("G2", AccountState{Exists: true}, time.Now().Add(-2*AccountStateCacheTTL))
	if _, ok := c.get("G2"); ok {
		t.Error("expired entry returned as fresh")
	}
}

// TestAccountStateCacheBounded — the cache never exceeds its cap; the
// oldest entry is evicted on overflow.
func TestAccountStateCacheBounded(t *testing.T) {
	t.Parallel()
	c := newAccountStateCache()
	base := time.Now()
	for i := 0; i < accountStateCacheMax+50; i++ {
		// Stagger cachedAt so eviction is deterministic (oldest-first).
		c.put(string(rune(i)), AccountState{Balance: int64(i)}, base.Add(time.Duration(i)*time.Millisecond))
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > accountStateCacheMax {
		t.Errorf("cache holds %d entries, want <= %d", n, accountStateCacheMax)
	}
}

// TestAccountStateCacheNilSafe — a nil cache (zero-value reader in tests)
// is a permanent miss, never a panic.
func TestAccountStateCacheNilSafe(t *testing.T) {
	t.Parallel()
	var c *accountStateCache
	if _, ok := c.get("G"); ok {
		t.Error("nil cache reported a hit")
	}
	c.put("G", AccountState{}, time.Now()) // must not panic
}

func TestPerKeyFlight(t *testing.T) {
	t.Parallel()
	f := newPerKeyFlight()
	ch, owner := f.begin("k")
	if !owner {
		t.Fatal("first begin did not get ownership")
	}
	if _, o2 := f.begin("k"); o2 {
		t.Error("second begin for same key also got ownership")
	}
	// A different key is independent.
	if _, o3 := f.begin("other"); !o3 {
		t.Error("begin for a different key was denied ownership")
	}
	f.end("k", ch)
	if _, o4 := f.begin("k"); !o4 {
		t.Error("begin after end did not get ownership")
	}
}

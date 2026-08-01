package v1

import (
	"fmt"
	"testing"
	"time"
)

// TestAssetDetailResponseCache_BoundedUnderIDChurn is the regression proof
// for W6-perf-1: enumerating many distinct asset_ids must NOT grow the cache
// without limit. A long TTL is used so nothing expires — forcing the SIZE cap
// (not expiry) to do the bounding, which is exactly the crawler/attacker
// scenario (fresh, distinct, never-repeated ids).
func TestAssetDetailResponseCache_BoundedUnderIDChurn(t *testing.T) {
	c := newAssetDetailResponseCache(time.Hour)
	for i := 0; i < assetDetailCacheMaxEntries+500; i++ {
		c.put(fmt.Sprintf("classic:FAKE%d-GABCDEF", i), []byte("{}"))
	}

	c.mu.RLock()
	n := len(c.entries)
	c.mu.RUnlock()
	if n > assetDetailCacheMaxEntries {
		t.Fatalf("cache grew to %d entries under id churn; must be bounded at %d (W6-perf-1)", n, assetDetailCacheMaxEntries)
	}

	// Eviction drops the OLDEST, not the newest — a just-written entry must
	// still be retrievable within TTL.
	last := fmt.Sprintf("classic:FAKE%d-GABCDEF", assetDetailCacheMaxEntries+499)
	if _, ok := c.get(last); !ok {
		t.Fatalf("most-recent entry %q was evicted; eviction should drop the oldest, not the newest", last)
	}
}

// TestAssetDetailResponseCache_EvictionPrefersExpired confirms the cheap path:
// when the cache is at capacity but holds expired entries, eviction reclaims
// them (rather than dropping a fresh one) so the fresh working set survives.
func TestAssetDetailResponseCache_EvictionPrefersExpired(t *testing.T) {
	c := newAssetDetailResponseCache(50 * time.Millisecond)
	// Fill to capacity, then let everything expire.
	for i := 0; i < assetDetailCacheMaxEntries; i++ {
		c.put(fmt.Sprintf("stale%d", i), []byte("{}"))
	}
	time.Sleep(80 * time.Millisecond) // all entries now past TTL

	// One more put at capacity triggers evictLocked; the expired-purge should
	// clear the whole stale set, leaving just the new entry.
	c.put("fresh", []byte("{}"))

	c.mu.RLock()
	n := len(c.entries)
	c.mu.RUnlock()
	if n != 1 {
		t.Fatalf("expired-purge left %d entries, want 1 (the fresh one)", n)
	}
	if _, ok := c.get("fresh"); !ok {
		t.Fatalf("the fresh entry was not retained after evicting expired entries")
	}
}

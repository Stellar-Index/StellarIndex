package explorer

import (
	"strconv"
	"testing"
	"time"
)

// TestContractDetailCache_PutProtectsUnexpiredCodeHistory is the
// CA2-A03-harden-3 regression guard. A "ch:" (code-history) entry has a
// 24h TTL and is only re-stamped once a day, so under global-oldest-cachedAt
// eviction it is ALWAYS the chronologically oldest entry next to the
// 1-5 minute "pos:"/"act:" classes — and gets evicted first however hot it
// is, even though it has not expired and plenty of genuinely stale churn
// entries exist. The fix must prefer an already-TTL-expired entry for
// eviction over an unexpired one, however old the unexpired one is.
func TestContractDetailCache_PutProtectsUnexpiredCodeHistory(t *testing.T) {
	c := &contractDetailCache{entries: make(map[string]contractDetailEntry, contractDetailCacheMax)}
	now := time.Now()

	const chKey = "ch:CONTRACTX"
	// Computed a while ago but well within its 24h TTL — still hot.
	c.entries[chKey] = contractDetailEntry{v: "code-history", cachedAt: now.Add(-20 * time.Hour)}

	// Fill the rest of the cache with "pos:" churn entries whose cachedAt
	// is staggered a few minutes back — chronologically YOUNGER than the
	// code-history entry, but already past their own 1-minute TTL, exactly
	// as ordinary distinct-G-address explorer traffic produces over time.
	for i := 0; i < contractDetailCacheMax-1; i++ {
		key := positionsCacheKey + "G" + strconv.Itoa(i)
		c.entries[key] = contractDetailEntry{
			v:        i,
			cachedAt: now.Add(-time.Duration(2+i%5) * time.Minute),
		}
	}

	// Cache is now at capacity; one more insert forces an eviction.
	c.put(positionsCacheKey+"GNEW", "new")

	if _, ok := c.entries[chKey]; !ok {
		t.Fatalf("code-history entry %s was evicted ahead of an already-expired churn entry", chKey)
	}
}

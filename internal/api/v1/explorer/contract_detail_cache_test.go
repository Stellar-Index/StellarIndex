package explorer

import (
	"strconv"
	"testing"
	"time"
)

// A "ch:" code-history entry has a 24h TTL re-stamped daily, so under global-oldest-cachedAt eviction it is always
// oldest beside the 1-5 minute "pos:"/"act:" classes. Eviction must prefer an expired entry over any unexpired one.
func TestContractDetailCache_PutProtectsUnexpiredCodeHistory(t *testing.T) {
	c := &contractDetailCache{entries: make(map[string]contractDetailEntry, contractDetailCacheMax)}
	now := time.Now()

	const chKey = "ch:CONTRACTX"
	// Computed a while ago but well within its 24h TTL — still hot.
	c.entries[chKey] = contractDetailEntry{v: "code-history", cachedAt: now.Add(-20 * time.Hour)}

	// Fill the rest with "pos:" churn entries younger than the code-history entry but past their own 1-minute TTL, as
	// ordinary distinct-G-address explorer traffic produces.
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

// A stream of distinct account-activity keys (cheap for any minted G-address) must stay inside its own bound and leave a
// full set of unexpired contract entries untouched.
func TestContractDetailCache_ActivityKeysCannotEvictContractEntries(t *testing.T) {
	c := &contractDetailCache{}
	for i := range contractDetailCacheMax {
		c.put("ev:C"+strconv.Itoa(i), i)
	}
	for i := range contractDetailCacheMax + 1 {
		c.put(accountActivityCacheKey+"G"+strconv.Itoa(i), i)
	}
	for i := range contractDetailCacheMax {
		if _, ok, _ := c.get("ev:C" + strconv.Itoa(i)); !ok {
			t.Fatalf("contract entry ev:C%d evicted by account-activity keys", i)
		}
	}
	if got := len(c.activity); got != contractDetailCacheMax {
		t.Fatalf("account-activity map holds %d entries, want its bound %d", got, contractDetailCacheMax)
	}
}

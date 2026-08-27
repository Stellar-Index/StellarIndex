package clickhouse

import (
	"testing"
	"time"
)

// TestWealthCacheServesAnyLimitFromOneEntry is the site-audit S3
// verification regression: the cache must store ONE ranking that every
// request size slices from, not a per-limit entry.
//
// The original per-limit keying meant prewarm warmed limit=100 while
// requests used 5/10/etc — each a distinct key that missed, so every real
// request 503'd and kicked its own 23s scan. This pins that a single put()
// (as prewarm does) satisfies gets at every limit.
func TestWealthCacheServesAnyLimitFromOneEntry(t *testing.T) {
	t.Parallel()
	c := newAccountsWealthCache()

	ranking := make([]AccountWealth, accountsWealthMaxLimit)
	for i := range ranking {
		ranking[i] = AccountWealth{AccountID: "acct", USD: float64(accountsWealthMaxLimit - i)}
	}
	c.put(ranking, WealthBasisUSD, time.Now())

	rows, _, _, ok := c.get()
	if !ok {
		t.Fatal("get after put returned miss")
	}
	if len(rows) != accountsWealthMaxLimit {
		t.Fatalf("cached ranking has %d rows, want %d", len(rows), accountsWealthMaxLimit)
	}

	// Every realistic request size slices cleanly from the single entry.
	for _, limit := range []int{5, 10, 25, 100, 199, 500} {
		got := clampWealth(rows, limit)
		want := limit
		if want > len(rows) {
			want = len(rows)
		}
		if len(got) != want {
			t.Errorf("clampWealth(limit=%d) = %d rows, want %d", limit, len(got), want)
		}
		if len(got) > 0 && got[0].USD < got[len(got)-1].USD {
			t.Errorf("limit=%d: slice not still descending", limit)
		}
	}
}

// TestWealthCacheStaleServing — an EXPIRED entry is still returned, with
// its real timestamp, so callers can serve degraded-but-honest instead of
// 503 (route-sweep 2026-07-29). Staleness is the caller's judgment via the
// returned cachedAt; the cache itself never withholds a filled entry.
func TestWealthCacheStaleServing(t *testing.T) {
	t.Parallel()
	c := newAccountsWealthCache()
	staleAt := time.Now().Add(-2 * AccountsWealthCacheTTL)
	c.put([]AccountWealth{{AccountID: "a", USD: 1}}, WealthBasisUSD, staleAt)
	rows, _, at, ok := c.get()
	if !ok {
		t.Fatal("expired entry withheld — stale-serving regressed to a hard miss")
	}
	if len(rows) != 1 || !at.Equal(staleAt) {
		t.Errorf("stale entry mangled: rows=%d at=%v want 1 row at %v", len(rows), at, staleAt)
	}
	if time.Since(at) <= AccountsWealthCacheTTL {
		t.Error("test setup: entry should read as stale to a TTL-comparing caller")
	}
}

// TestWealthCacheNilSafe — a nil cache (zero-value reader in some tests)
// is a permanent miss, never a panic.
func TestWealthCacheNilSafe(t *testing.T) {
	t.Parallel()
	var c *accountsWealthCache
	if _, _, _, ok := c.get(); ok {
		t.Error("nil cache reported a hit")
	}
	c.put([]AccountWealth{{AccountID: "a", USD: 1}}, WealthBasisUSD, time.Now()) // must not panic
	if _, owner := c.beginFlight(); owner {
		t.Error("nil cache granted flight ownership")
	}
}

// TestWealthCacheSingleFlight — a second beginFlight while one is in flight
// does not get ownership.
func TestWealthCacheSingleFlight(t *testing.T) {
	t.Parallel()
	c := newAccountsWealthCache()
	ch, owner := c.beginFlight()
	if !owner {
		t.Fatal("first beginFlight did not get ownership")
	}
	if _, owner2 := c.beginFlight(); owner2 {
		t.Error("second concurrent beginFlight also got ownership")
	}
	c.endFlight(ch)
	if _, owner3 := c.beginFlight(); !owner3 {
		t.Error("beginFlight after endFlight did not get ownership")
	}
}

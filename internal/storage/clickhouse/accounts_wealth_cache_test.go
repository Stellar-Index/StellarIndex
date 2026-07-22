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
	c.put(ranking, time.Now())

	rows, ok := c.get(AccountsWealthCacheTTL)
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

// TestWealthCacheTTLExpiry — a stale entry is a miss.
func TestWealthCacheTTLExpiry(t *testing.T) {
	t.Parallel()
	c := newAccountsWealthCache()
	c.put([]AccountWealth{{AccountID: "a", USD: 1}}, time.Now().Add(-2*AccountsWealthCacheTTL))
	if _, ok := c.get(AccountsWealthCacheTTL); ok {
		t.Error("expired entry returned as fresh")
	}
}

// TestWealthCacheNilSafe — a nil cache (zero-value reader in some tests)
// is a permanent miss, never a panic.
func TestWealthCacheNilSafe(t *testing.T) {
	t.Parallel()
	var c *accountsWealthCache
	if _, ok := c.get(AccountsWealthCacheTTL); ok {
		t.Error("nil cache reported a hit")
	}
	c.put([]AccountWealth{{AccountID: "a", USD: 1}}, time.Now()) // must not panic
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

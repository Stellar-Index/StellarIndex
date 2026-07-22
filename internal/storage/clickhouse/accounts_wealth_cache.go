package clickhouse

import (
	"context"
	"sync"
	"time"
)

// AccountsWealthCacheTTL is how long a wealth ranking stays servable.
//
// The underlying query is a `FINAL` scan of stellar.ledger_entries_current
// — 43.6M live account/trustline rows, measured 11.1 s on R1 for the row
// COUNT alone, before the per-account price join and sort. It cannot be
// made to fit a request deadline by tuning; it has to be precomputed.
//
// 15 minutes is chosen against what the data actually does: this is a
// leaderboard of the largest balances on the network, which reorders on
// the timescale of large transfers, not seconds. Serving a ranking up to
// 15 minutes old is materially indistinguishable from live, and the
// response carries the lake watermark so callers can see its vintage.
const AccountsWealthCacheTTL = 15 * time.Minute

// AccountsWealthRefreshTimeout bounds a single background refresh. Well
// above the ~11-20 s the query needs, so a loaded box does not abandon a
// refresh that would have succeeded, but bounded so a wedged query cannot
// pin the refresher forever.
const AccountsWealthRefreshTimeout = 3 * time.Minute

// accountsWealthEntry is one cached ranking, keyed by limit.
type accountsWealthEntry struct {
	rows     []AccountWealth
	cachedAt time.Time
}

// accountsWealthCache is a TTL + single-flight cache in front of
// [ExplorerReader.AccountsByWealth].
//
// Why this exists (site-audit S3): /v1/accounts was returning HTTP 500
// after 8.1 s on every single request. The handler wraps the read in an
// 8 s deadline; the query needs 11-20 s; so it timed out, logged
// "context deadline exceeded", and 500'd — 100% of the time, at any load.
// The page showed a permanent "Loading…" and then an error blaming
// "the current-state projection is still backfilling, or pricing is
// offline", neither of which was true.
//
// Serving stale-but-real data beats serving nothing: a request that finds
// a warm entry returns immediately, and a request that finds none is told
// so honestly rather than being hung for 8 s first.
type accountsWealthCache struct {
	mu      sync.Mutex
	entries map[int]accountsWealthEntry
	flight  map[int]chan struct{}
}

func newAccountsWealthCache() *accountsWealthCache {
	return &accountsWealthCache{
		entries: make(map[int]accountsWealthEntry),
		flight:  make(map[int]chan struct{}),
	}
}

// get returns a cached ranking when one is fresh enough.
// A nil cache (a zero-value ExplorerReader, as built in some tests)
// behaves as a permanent miss rather than panicking.
func (c *accountsWealthCache) get(limit int, ttl time.Duration) ([]AccountWealth, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[limit]
	if !ok || time.Since(e.cachedAt) > ttl {
		return nil, false
	}
	return e.rows, true
}

func (c *accountsWealthCache) put(limit int, rows []AccountWealth, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[limit] = accountsWealthEntry{rows: rows, cachedAt: now}
}

// beginFlight returns (wait, false) when a refresh for `limit` is already
// running — the caller should wait on the channel rather than issue a
// second 20-second scan. It returns (done, true) when the caller owns the
// refresh and must close `done` when finished.
func (c *accountsWealthCache) beginFlight(limit int) (chan struct{}, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, running := c.flight[limit]; running {
		return ch, false
	}
	ch := make(chan struct{})
	c.flight[limit] = ch
	return ch, true
}

func (c *accountsWealthCache) endFlight(limit int, ch chan struct{}) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.flight, limit)
	c.mu.Unlock()
	close(ch)
}

// AccountsByWealthCached serves the wealth ranking from cache, refreshing
// in the background when stale.
//
// It NEVER runs the slow scan on the caller's deadline. On a cache miss it
// returns ok=false immediately so the handler can render an honest
// "warming up" state instead of hanging for the request timeout and then
// failing — which is precisely the behaviour site-audit S3 recorded.
//
// A refresh is kicked off on miss (single-flight, detached context), so
// the first caller after a cold start pays nothing and the entry is
// present for subsequent ones. PrewarmAccountsByWealth exists so that in
// practice nobody ever sees the cold state at all.
func (r *ExplorerReader) AccountsByWealthCached(
	ctx context.Context, assets []string, prices []float64, limit int,
) ([]AccountWealth, bool) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if rows, ok := r.wealthCache.get(limit, AccountsWealthCacheTTL); ok {
		return rows, true
	}
	// Miss: start a background refresh and tell the caller we have nothing
	// yet. Deliberately does not block — see the godoc.
	r.refreshAccountsWealth(assets, prices, limit)
	return nil, false
}

// PrewarmAccountsByWealth refreshes the ranking synchronously, for the
// API's prewarm loop. Blocks for as long as the scan takes (bounded by
// AccountsWealthRefreshTimeout), which is exactly what a background
// warmer should do.
func (r *ExplorerReader) PrewarmAccountsByWealth(
	ctx context.Context, assets []string, prices []float64, limit int,
) error {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.AccountsByWealth(ctx, assets, prices, limit)
	if err != nil {
		return err
	}
	r.wealthCache.put(limit, rows, time.Now())
	return nil
}

// refreshAccountsWealth runs one detached refresh, collapsing concurrent
// attempts for the same limit into a single scan.
func (r *ExplorerReader) refreshAccountsWealth(assets []string, prices []float64, limit int) {
	ch, owner := r.wealthCache.beginFlight(limit)
	if !owner {
		return // someone else is already scanning; don't pile on
	}
	// Detached from the request context on purpose: the whole point is to
	// outlive the request that noticed the miss.
	go func() {
		defer r.wealthCache.endFlight(limit, ch)
		ctx, cancel := context.WithTimeout(context.Background(), AccountsWealthRefreshTimeout)
		defer cancel()
		rows, err := r.AccountsByWealth(ctx, assets, prices, limit)
		if err != nil {
			return // next caller retries; nothing cached, nothing corrupted
		}
		r.wealthCache.put(limit, rows, time.Now())
	}()
}

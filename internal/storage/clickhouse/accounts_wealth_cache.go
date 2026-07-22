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

// accountsWealthMaxLimit is the size of the single ranking the cache
// computes and stores. Requests ask for a limit (ParseLimit caps it at
// 500), and the cache serves the first `limit` rows of this one ranking
// rather than caching per-limit.
//
// Per-limit keying was a real bug (site-audit S3 verification): prewarm
// warms one limit (100) while requests use 5/10/etc, so every real request
// missed a different key, kicked its own 23s refresh, and served 503 until
// that particular limit happened to finish. One ranking, sliced, means a
// single warm entry covers every request size.
const accountsWealthMaxLimit = 500

// accountsWealthEntry is the single cached ranking (top
// [accountsWealthMaxLimit]); callers slice it to their requested limit.
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
	mu     sync.Mutex
	entry  accountsWealthEntry
	filled bool
	flight chan struct{}
}

func newAccountsWealthCache() *accountsWealthCache {
	return &accountsWealthCache{}
}

// get returns the cached ranking when it is fresh. A nil cache (a
// zero-value ExplorerReader, as built in some tests) behaves as a
// permanent miss rather than panicking.
func (c *accountsWealthCache) get() ([]AccountWealth, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.filled || time.Since(c.entry.cachedAt) > AccountsWealthCacheTTL {
		return nil, false
	}
	return c.entry.rows, true
}

func (c *accountsWealthCache) put(rows []AccountWealth, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = accountsWealthEntry{rows: rows, cachedAt: now}
	c.filled = true
}

// beginFlight returns (wait, false) when a refresh is already running — the
// caller should wait on the channel rather than issue a second scan. It
// returns (done, true) when the caller owns the refresh and must close
// `done` when finished.
func (c *accountsWealthCache) beginFlight() (chan struct{}, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flight != nil {
		return c.flight, false
	}
	ch := make(chan struct{})
	c.flight = ch
	return ch, true
}

func (c *accountsWealthCache) endFlight(ch chan struct{}) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.flight = nil
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
	if limit <= 0 || limit > accountsWealthMaxLimit {
		limit = 100
	}
	if rows, ok := r.wealthCache.get(); ok {
		return clampWealth(rows, limit), true
	}
	// Miss: start a background refresh and tell the caller we have nothing
	// yet. Deliberately does not block — see the godoc.
	//
	// contextcheck: the refresh must NOT inherit this request's context.
	// Bound to the caller's 8s deadline it would be cancelled before the
	// ~11-20s FINAL scan completed, so the cache would never populate and
	// every request would keep paying the timeout — exactly the failure
	// this cache exists to fix (site-audit S3).
	r.refreshAccountsWealth(assets, prices) //nolint:contextcheck // intentional detach; see above
	return nil, false
}

// clampWealth returns the first `limit` rows of a cached ranking.
func clampWealth(rows []AccountWealth, limit int) []AccountWealth {
	if limit < len(rows) {
		return rows[:limit]
	}
	return rows
}

// PrewarmAccountsByWealth refreshes the ranking synchronously, for the
// API's prewarm loop. Blocks for as long as the scan takes (bounded by
// AccountsWealthRefreshTimeout), which is exactly what a background
// warmer should do.
func (r *ExplorerReader) PrewarmAccountsByWealth(
	ctx context.Context, assets []string, prices []float64,
) error {
	rows, err := r.AccountsByWealth(ctx, assets, prices, accountsWealthMaxLimit)
	if err != nil {
		return err
	}
	r.wealthCache.put(rows, time.Now())
	return nil
}

// refreshAccountsWealth runs one detached refresh, collapsing concurrent
// attempts for the same limit into a single scan.
func (r *ExplorerReader) refreshAccountsWealth(assets []string, prices []float64) {
	ch, owner := r.wealthCache.beginFlight()
	if !owner {
		return // someone else is already scanning; don't pile on
	}
	// Detached from the request context on purpose: the whole point is to
	// outlive the request that noticed the miss.
	go func() {
		defer r.wealthCache.endFlight(ch)
		ctx, cancel := context.WithTimeout(context.Background(), AccountsWealthRefreshTimeout)
		defer cancel()
		rows, err := r.AccountsByWealth(ctx, assets, prices, accountsWealthMaxLimit)
		if err != nil {
			// Log rather than swallow: a persistently-failing refresh keeps
			// /v1/accounts on its 503 warming state indefinitely, and a
			// silent failure here is what made that hard to diagnose the
			// first time (the query was dying at the connection's 30s cap).
			if r.wealthRefreshErr != nil {
				r.wealthRefreshErr(err)
			}
			return // next caller retries; nothing cached, nothing corrupted
		}
		r.wealthCache.put(rows, time.Now())
	}()
}

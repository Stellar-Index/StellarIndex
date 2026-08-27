package clickhouse

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// Wealth-ranking bases. The ranking is by total USD value where the caller
// supplies a USD price map; where it can't (no aggregator / empty catalogue —
// the lean test nets), the caller ranks by native XLM balance instead, and the
// cached entry records which so the API can label the served numbers correctly.
const (
	WealthBasisUSD    = "usd"
	WealthBasisNative = "native_xlm"
)

// wealthBasis infers the ranking basis from the (asset, price) inputs: the
// native-XLM fallback is exactly the single key "native" priced at 1.0.
func wealthBasis(assets []string) string {
	if len(assets) == 1 && assets[0] == "native" {
		return WealthBasisNative
	}
	return WealthBasisUSD
}

// accountsWealthEntry is the single cached ranking (top
// [accountsWealthMaxLimit]); callers slice it to their requested limit.
type accountsWealthEntry struct {
	rows     []AccountWealth
	basis    string
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

// get returns the cached ranking and its fetch time whenever one has EVER
// been stored — including past the TTL. Staleness is the CALLER's decision
// now (2026-07-29): treating an expired entry as a hard miss meant one
// window of failed refreshes blanked the route back to its 503 warming
// state even though a perfectly real ranking sat in memory — serving it
// with an honest as-of + degraded flag beats serving nothing. ok=false only
// when nothing was ever stored. A nil cache (a zero-value ExplorerReader,
// as built in some tests) behaves as a permanent miss rather than
// panicking.
func (c *accountsWealthCache) get() ([]AccountWealth, string, time.Time, bool) {
	if c == nil {
		return nil, "", time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.filled {
		return nil, "", time.Time{}, false
	}
	return c.entry.rows, c.entry.basis, c.entry.cachedAt, true
}

func (c *accountsWealthCache) put(rows []AccountWealth, basis string, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = accountsWealthEntry{rows: rows, basis: basis, cachedAt: now}
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
// It NEVER runs the slow scan on the caller's deadline. Three states:
//
//   - fresh entry (within AccountsWealthCacheTTL): served as-is.
//   - STALE entry (TTL lapsed — e.g. the background refresh has been
//     failing): served anyway, with its real asOf, and a detached
//     single-flight refresh is kicked. The handler compares asOf against
//     AccountsWealthCacheTTL to set the envelope's degraded (`stale`)
//     flag — a real-but-old ranking with an honest timestamp beats a 503
//     (route-sweep 2026-07-29: a window of refresh failures used to blank
//     the route back to "warming up" indefinitely).
//   - nothing ever stored: ok=false immediately so the handler can render
//     an honest warming state instead of hanging for the request timeout
//     and then failing — precisely the behaviour site-audit S3 recorded —
//     and the same detached refresh is kicked.
//
// PrewarmAccountsByWealth exists so that in practice nobody ever sees the
// cold state at all.
func (r *ExplorerReader) AccountsByWealthCached(
	ctx context.Context, assets []string, prices []float64, limit int,
) ([]AccountWealth, string, time.Time, bool) {
	if limit <= 0 || limit > accountsWealthMaxLimit {
		limit = 100
	}
	rows, basis, asOf, ok := r.wealthCache.get()
	if ok && time.Since(asOf) <= AccountsWealthCacheTTL {
		return clampWealth(rows, limit), basis, asOf, true
	}
	// Stale or cold: start a background refresh either way.
	//
	// contextcheck: the refresh must NOT inherit this request's context.
	// Bound to the caller's 8s deadline it would be cancelled before the
	// ~11-20s FINAL scan completed, so the cache would never populate and
	// every request would keep paying the timeout — exactly the failure
	// this cache exists to fix (site-audit S3).
	r.refreshAccountsWealth(assets, prices) //nolint:contextcheck // intentional detach; see above
	if ok {
		// Stale-but-real: serve it with its honest timestamp.
		return clampWealth(rows, limit), basis, asOf, true
	}
	return nil, "", time.Time{}, false
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
	r.wealthCache.put(r.withLocked(ctx, rows), wealthBasis(assets), time.Now())
	return nil
}

// withLocked resolves the locked-burn flag for the ranked accounts and
// stamps it onto the rows, so the request path never runs the
// AccountsUnspendable FINAL scan (site-audit S3). A failure here degrades
// to unbadged rather than failing the whole refresh — the ranking is the
// important part; the badge is advisory.
func (r *ExplorerReader) withLocked(ctx context.Context, rows []AccountWealth) []AccountWealth {
	if len(rows) == 0 {
		return rows
	}
	ids := make([]string, len(rows))
	for i, a := range rows {
		ids[i] = a.AccountID
	}
	locked, err := r.AccountsUnspendable(ctx, ids)
	if err != nil {
		return rows
	}
	for i := range rows {
		rows[i].Locked = locked[rows[i].AccountID]
	}
	return rows
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
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), AccountsWealthRefreshTimeout)
		defer cancel()
		rows, err := r.AccountsByWealth(ctx, assets, prices, accountsWealthMaxLimit)
		if err == nil {
			rows = r.withLocked(ctx, rows)
		}
		obs.ObserveExplorerSWRRefresh("accounts_wealth", start, err)
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
		r.wealthCache.put(rows, wealthBasis(assets), time.Now())
	}()
}

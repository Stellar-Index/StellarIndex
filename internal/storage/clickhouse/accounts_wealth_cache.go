package clickhouse

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// AccountsWealthCacheTTL is how long a wealth ranking stays servable.
//
// The query is a FINAL scan of ledger_entries_current (43.6M rows, ~11 s for the count alone),
// so it must be precomputed. A leaderboard of the largest balances reorders on the timescale
// of large transfers; the response carries the snapshot's own ledger (AsOfLedger).
const AccountsWealthCacheTTL = 15 * time.Minute

// AccountsWealthRefreshTimeout bounds one background refresh: well above the ~11-20 s the
// query needs, but finite so a wedged query cannot pin the refresher.
const AccountsWealthRefreshTimeout = 3 * time.Minute

// accountsWealthMaxLimit is the size of the single ranking the cache stores; requests get the
// first `limit` rows of it. Keying per limit made prewarm (one limit) miss every real request.
const accountsWealthMaxLimit = 500

// Wealth-ranking bases: total USD value when the caller supplies a price map, else native XLM
// balance (no aggregator / empty catalogue); the entry records which so the API can label it.
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

// AccountWealthSnapshot is one computed ranking and its vintage; the cache holds one (top
// [accountsWealthMaxLimit]) and callers get it sliced.
type AccountWealthSnapshot struct {
	Rows  []AccountWealth
	Basis string
	// AsOf is when the ranking was computed. AsOfLedger is the lake watermark read before
	// the scan, so it never names a ledger later than the data read; 0 if unreadable.
	AsOf       time.Time
	AsOfLedger uint32
}

// accountsWealthCache is a TTL + single-flight cache in front of
// [ExplorerReader.AccountsByWealth]. The query needs 11-20 s against the handler's 8 s
// deadline, so it must never run on a request; a warm stale entry beats hanging.
type accountsWealthCache struct {
	mu     sync.Mutex
	entry  AccountWealthSnapshot
	filled bool
	flight chan struct{}
}

func newAccountsWealthCache() *accountsWealthCache {
	return &accountsWealthCache{}
}

// get returns the cached ranking and fetch time whenever one was ever stored, including past
// the TTL: staleness is the caller's decision, and a real old ranking with an honest as-of
// beats blanking the route. ok=false only when nothing was stored. A nil cache is a
// permanent miss.
func (c *accountsWealthCache) get() (AccountWealthSnapshot, bool) {
	if c == nil {
		return AccountWealthSnapshot{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.filled {
		return AccountWealthSnapshot{}, false
	}
	return c.entry, true
}

func (c *accountsWealthCache) put(snap AccountWealthSnapshot) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = snap
	c.filled = true
}

// beginFlight returns (wait, false) when a refresh is running; (done, true) when the caller
// owns it and must close `done`.
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

// AccountsByWealthCached serves the ranking from cache, refreshing in the background when
// stale. It never runs the slow scan on the caller's deadline:
//
//   - fresh: served as-is; handlers stamp the snapshot's AsOf/AsOfLedger, never a serve-time
//     watermark read (a torn read).
//   - stale: served with its real asOf and a detached single-flight refresh is kicked; the
//     handler compares asOf to AccountsWealthCacheTTL to set the `stale` flag.
//   - empty: ok=false immediately (honest warming state) and the refresh is kicked.
//
// PrewarmAccountsByWealth keeps the cold state out of sight in practice.
func (r *ExplorerReader) AccountsByWealthCached(
	ctx context.Context, assets, prices []string, limit int,
) (AccountWealthSnapshot, bool) {
	if limit <= 0 || limit > accountsWealthMaxLimit {
		limit = 100
	}
	snap, ok := r.wealthCache.get()
	snap.Rows = clampWealth(snap.Rows, limit)
	if ok && time.Since(snap.AsOf) <= AccountsWealthCacheTTL {
		return snap, true
	}
	// Stale or cold: refresh in the background.
	//
	// contextcheck: the refresh must not inherit this request's context; bound to the 8s
	// deadline it would be cancelled before the ~11-20s scan finished and never populate.
	r.refreshAccountsWealth(assets, prices) //nolint:contextcheck // intentional detach; see above
	if ok {
		// Stale-but-real: serve it with its honest timestamp.
		return snap, true
	}
	return AccountWealthSnapshot{}, false
}

// clampWealth returns the first `limit` rows of a cached ranking.
func clampWealth(rows []AccountWealth, limit int) []AccountWealth {
	if limit < len(rows) {
		return rows[:limit]
	}
	return rows
}

// PrewarmAccountsByWealth refreshes the ranking synchronously for the prewarm loop, blocking
// up to AccountsWealthRefreshTimeout.
func (r *ExplorerReader) PrewarmAccountsByWealth(
	ctx context.Context, assets, prices []string,
) error {
	snap, err := r.computeAccountsWealth(ctx, assets, prices)
	if err != nil {
		return err
	}
	r.wealthCache.put(snap)
	return nil
}

// computeAccountsWealth runs one ranking and stamps its vintage. The watermark is read before
// the scan so AsOfLedger never exceeds the data seen; unreadable leaves it 0, not a failure.
func (r *ExplorerReader) computeAccountsWealth(
	ctx context.Context, assets, prices []string,
) (AccountWealthSnapshot, error) {
	var ledger uint32
	if wm, _, err := r.LakeWatermark(ctx); err == nil {
		ledger = wm
	}
	rows, err := r.AccountsByWealth(ctx, assets, prices, accountsWealthMaxLimit)
	if err != nil {
		return AccountWealthSnapshot{}, err
	}
	return AccountWealthSnapshot{
		Rows: r.withLocked(ctx, rows), Basis: wealthBasis(assets),
		AsOf: time.Now(), AsOfLedger: ledger,
	}, nil
}

// withLocked stamps the locked-burn flag onto the ranked rows so requests never run the
// AccountsUnspendable FINAL scan. A failure degrades to unbadged; the badge is advisory.
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
func (r *ExplorerReader) refreshAccountsWealth(assets, prices []string) {
	ch, owner := r.wealthCache.beginFlight()
	if !owner {
		return // someone else is already scanning; don't pile on
	}
	// Detached from the request context on purpose: it must outlive the request.
	go func() {
		defer r.wealthCache.endFlight(ch)
		// An unrecovered panic in any goroutine kills the API. Registered last so it unwinds
		// first and endFlight still runs, so a contained panic cannot wedge the flight.
		defer worker.Recover(nil, "explorer-accounts-wealth-refresh")
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), AccountsWealthRefreshTimeout)
		defer cancel()
		snap, err := r.computeAccountsWealth(ctx, assets, prices)
		obs.ObserveExplorerSWRRefresh("accounts_wealth", start, err)
		if err != nil {
			// Log rather than swallow: a persistently-failing refresh pins /v1/accounts on its
			// 503 warming state.
			if r.wealthRefreshErr != nil {
				r.wealthRefreshErr(err)
			}
			return // next caller retries; nothing cached, nothing corrupted
		}
		r.wealthCache.put(snap)
	}()
}

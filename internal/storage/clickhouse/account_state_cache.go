package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// AccountStateCacheTTL bounds how long a cached account state is served; short on purpose, since the cache breaks contention on the FINAL scan rather than providing freshness.
const AccountStateCacheTTL = 30 * time.Second

// accountStateCacheMax bounds resident entries: a few hot accounts plus one-off churn. On
// overflow the oldest entry is evicted.
const accountStateCacheMax = 4096

type accountStateEntry struct {
	state    AccountState
	cachedAt time.Time
}

type accountStateCache struct {
	mu      sync.Mutex
	entries map[string]accountStateEntry
}

func newAccountStateCache() *accountStateCache {
	return &accountStateCache{entries: make(map[string]accountStateEntry)}
}

// get returns the cached state whenever one exists, past the TTL too (fresh=false); staleness is the caller's call, since a hard miss on
// expiry left whale accounts (scan outruns the budget) 503ing. ok=false only when never computed. Nil-safe: a zero-value reader is a
// permanent miss.
func (c *accountStateCache) get(account string) (st AccountState, ok, fresh bool) {
	if c == nil {
		return AccountState{}, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, present := c.entries[account]
	if !present {
		return AccountState{}, false, false
	}
	return e.state, true, time.Since(e.cachedAt) <= AccountStateCacheTTL
}

func (c *accountStateCache) put(account string, st AccountState, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= accountStateCacheMax {
		// Evict the oldest entry (approximate LRU: one pass, only at capacity).
		var oldestKey string
		var oldestAt time.Time
		for k, e := range c.entries {
			if oldestKey == "" || e.cachedAt.Before(oldestAt) {
				oldestKey, oldestAt = k, e.cachedAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[account] = accountStateEntry{state: st, cachedAt: now}
}

// accountStateRefreshTimeout bounds one detached scan: the 3-minute ceiling of the other
// detached refreshes; a whale account's UNION arms run well past the 8s request budget.
const accountStateRefreshTimeout = 3 * time.Minute

// errAccountStateRefreshFailed is returned to waiters when the detached scan produced no
// cacheable state (the error went through the wealth-refresh error hook, the only log seam).
var errAccountStateRefreshFailed = errors.New(
	"clickhouse: detached account-state refresh produced no entry")

// AccountStateCached serves account state from the TTL cache; a miss kicks one detached scan per account (so a whale scan survives the request) and waits only up to the caller's deadline.
func (r *ExplorerReader) AccountStateCached(ctx context.Context, account string) (AccountState, bool, error) {
	if st, ok, fresh := r.stateCache.get(account); ok {
		if !fresh {
			// Serve the stale entry now while one detached refresh runs: old-but-real beats a 503. The
			// stale bool lets the handler set flags.stale, as for the wealth ranking.
			r.refreshAccountState(account) //nolint:contextcheck // intentional detach — see refreshAccountState
		}
		return st, !fresh, nil
	}
	// Not single-flighted across accounts: distinct accounts need distinct scans; only the
	// same-account burst is collapsed, by the per-account flight.
	fl, saturated := r.refreshAccountState(account) //nolint:contextcheck // intentional detach — the fill must outlive a caller that times out (see doc above)
	select {
	case <-fl.done:
		if st, ok, _ := r.stateCache.get(account); ok {
			return st, false, nil
		}
		if saturated || fl.saturated {
			// A full gate means no scan ran: transient backpressure, not a failed scan. Return the
			// retryable sentinel (503) rather than 500; fl.saturated covers a non-owner that joined a
			// flight the owner then skipped.
			return AccountState{}, false, ErrRefreshSaturated
		}
		return AccountState{}, false, errAccountStateRefreshFailed
	case <-ctx.Done():
		return AccountState{}, false, ctx.Err()
	}
}

// refreshAccountState kicks one detached scan for account (returning the existing flight's channel), outliving the request. saturated is
// true only when this call owned the flight and the shared refresh gate was full; non-owners read the entry's flag, set before end()
// closes done.
func (r *ExplorerReader) refreshAccountState(account string) (fl *stateFlightEntry, saturated bool) {
	fl, owner := r.stateFlight.begin(account)
	if !owner {
		return fl, false
	}
	// Global bound across keys: the per-account flight collapses same-account bursts, but the
	// account space is attacker-chosen, so key churn would queue unbounded scans on the shared
	// pool. On saturation skip, never queue (see RefreshGate).
	if !r.refreshGate.TryAcquireClass("account_state") {
		fl.saturated = true // published to waiters by end()'s close
		r.stateFlight.end(account, fl)
		return fl, true
	}
	go func() {
		// end() runs last: a waiter woken by done that re-kicks must find the slot free.
		defer r.stateFlight.end(account, fl)
		defer r.refreshGate.ReleaseClass("account_state")
		// An unrecovered panic in any goroutine kills the API, and the account is attacker-chosen.
		// Registered last so it unwinds first and the releases above run on a non-panicking stack.
		defer worker.Recover(nil, "explorer-account-state-refresh")
		rctx, cancel := context.WithTimeout(context.Background(), accountStateRefreshTimeout)
		defer cancel()
		// Watermark read before the scan (as computeAccountsWealth): AsOfLedger must never exceed
		// the state it stamps. Unreadable leaves it 0 rather than failing a completed scan.
		var ledger uint32
		if wm, _, err := r.LakeWatermark(rctx); err == nil {
			ledger = wm
		}
		st, err := r.AccountState(rctx, account)
		if err != nil {
			if r.wealthRefreshErr != nil {
				r.wealthRefreshErr(fmt.Errorf("detached account-state refresh (%s): %w", account, err))
			}
			return
		}
		st.AsOfLedger = ledger
		r.stateCache.put(account, st, time.Now())
	}()
	return fl, false
}

// stateFlightEntry is one in-flight refresh; saturated is written only by the owner before end() closes done, so waiters read it after
// <-done without locking.
type stateFlightEntry struct {
	done      chan struct{}
	saturated bool
}

// perKeyFlight collapses concurrent work for the same key. Used for the
// account-state single-flight above.
type perKeyFlight struct {
	mu      sync.Mutex
	inGoing map[string]*stateFlightEntry
}

func newPerKeyFlight() *perKeyFlight {
	return &perKeyFlight{inGoing: make(map[string]*stateFlightEntry)}
}

func (f *perKeyFlight) begin(key string) (*stateFlightEntry, bool) {
	if f == nil {
		// Test-only readers have no flight; a nil done channel blocks until the caller's deadline.
		return &stateFlightEntry{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl, ok := f.inGoing[key]; ok {
		return fl, false
	}
	fl := &stateFlightEntry{done: make(chan struct{})}
	f.inGoing[key] = fl
	return fl, true
}

func (f *perKeyFlight) end(key string, fl *stateFlightEntry) {
	if f == nil {
		return
	}
	f.mu.Lock()
	delete(f.inGoing, key)
	f.mu.Unlock()
	close(fl.done)
}

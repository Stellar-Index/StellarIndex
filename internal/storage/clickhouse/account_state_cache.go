package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// AccountStateCacheTTL bounds how long a cached account state is served.
//
// Account balances change, but a detail view tolerates seconds of
// staleness, and this is deliberately short. The point of the cache is not
// long-lived freshness — it is to break the contention spiral (site-audit
// follow-up): AccountState reads stellar.ledger_entries_current (4.2B rows)
// with a FINAL scan that, though it uses the account_id bloom index and is
// ~0.4s in isolation, balloons to the 8s handler ceiling when many detail
// requests (a crawler, a static-export build, a burst of users) run
// concurrently under the bounded 2-thread api_serving profile. Serving
// repeat views from cache both returns them instantly AND cuts the number
// of concurrent scans, so the scans that DO run stay fast.
const AccountStateCacheTTL = 30 * time.Second

// accountStateCacheMax bounds resident entries. Account detail is
// long-tail — a handful of hot accounts (large issuers, the burn address)
// plus a churn of one-off lookups — so a modest cap holds the hot set while
// capping memory. On overflow the oldest entry is evicted.
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

// get returns the cached state whenever one exists — INCLUDING past the
// TTL (fresh=false). Staleness is the CALLER's judgment (route-sweep
// 2026-07-30): treating an expired entry as a hard miss meant a whale
// account whose scan outruns the request budget was warm for only the
// 30s after each detached fill and 503'd the rest of the time — the
// same failure shape the wealth cache fixed on 2026-07-29. ok=false only
// when the account was never computed. Nil-safe (a zero-value reader in
// tests behaves as a permanent miss).
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
		// Evict the oldest entry (approximate LRU — good enough for a
		// bound, and cheap: one pass only when at capacity).
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

// accountStateRefreshTimeout bounds one detached account-state scan. The
// same 3-minute ceiling as the other detached explorer refreshes; a whale
// account's UNION arms measured well past the 8s request budget, which is
// exactly why the scan cannot be request-scoped (see below).
const accountStateRefreshTimeout = 3 * time.Minute

// errAccountStateRefreshFailed is returned to waiters when the detached
// scan finished without producing a cacheable state (the underlying error
// was reported through the wealth-refresh error hook, the reader's only
// logging seam).
var errAccountStateRefreshFailed = errors.New(
	"clickhouse: detached account-state refresh produced no entry")

// AccountStateCached serves account state from the TTL cache; on a miss it
// kicks ONE DETACHED scan per account and waits bounded by the CALLER's
// deadline only. This is the method the API detail handlers should call —
// see accountStateCache's godoc for why (site-audit follow-up:
// /v1/accounts/{g} and /v1/issuers/{g} were 6-8s under concurrent load).
//
// Detached (route-sweep 2026-07-30): the scan used to run on the request
// context, so a whale account whose UNION arms exceed the 8s budget died
// WITH the request, the cache never filled, and every retry paid the
// timeout again — a permanent 503 for exactly the accounts people look up.
// Now the scan runs on its own bounded budget and outlives any caller that
// gives up; the timed-out request 503s honestly and the retry lands warm.
func (r *ExplorerReader) AccountStateCached(ctx context.Context, account string) (AccountState, bool, error) {
	if st, ok, fresh := r.stateCache.get(account); ok {
		if !fresh {
			// Serve the STALE entry immediately while ONE detached
			// refresh runs — old-but-real beats a 503, and for a whale
			// account whose scan outruns the request budget this is the
			// only way the route answers at all outside the short
			// post-fill window (route-sweep 2026-07-30). The stale bool
			// lets the handler pair the serve with flags.stale, matching
			// the wealth ranking's honesty contract.
			r.refreshAccountState(account) //nolint:contextcheck // intentional detach — see refreshAccountState
		}
		return st, !fresh, nil
	}
	// Not single-flighted across accounts on purpose — distinct accounts
	// genuinely need distinct scans. Only the exact-same-account burst is
	// worth collapsing, which the per-account flight handles.
	ch := r.refreshAccountState(account) //nolint:contextcheck // intentional detach — the fill must outlive a caller that times out (see doc above)
	select {
	case <-ch:
		if st, ok, _ := r.stateCache.get(account); ok {
			return st, false, nil
		}
		return AccountState{}, false, errAccountStateRefreshFailed
	case <-ctx.Done():
		return AccountState{}, false, ctx.Err()
	}
}

// refreshAccountState kicks ONE detached scan for account (returning the
// existing flight's channel while one is up). Detached from any request
// context on purpose — the whole point is to outlive the request that
// noticed the miss (see AccountStateCached).
func (r *ExplorerReader) refreshAccountState(account string) chan struct{} {
	ch, owner := r.stateFlight.begin(account)
	if !owner {
		return ch
	}
	go func() {
		defer r.stateFlight.end(account, ch)
		rctx, cancel := context.WithTimeout(context.Background(), accountStateRefreshTimeout)
		defer cancel()
		st, err := r.AccountState(rctx, account)
		if err != nil {
			if r.wealthRefreshErr != nil {
				r.wealthRefreshErr(fmt.Errorf("detached account-state refresh (%s): %w", account, err))
			}
			return
		}
		r.stateCache.put(account, st, time.Now())
	}()
	return ch
}

// perKeyFlight collapses concurrent work for the same key. Used for the
// account-state single-flight above.
type perKeyFlight struct {
	mu      sync.Mutex
	inGoing map[string]chan struct{}
}

func newPerKeyFlight() *perKeyFlight {
	return &perKeyFlight{inGoing: make(map[string]chan struct{})}
}

func (f *perKeyFlight) begin(key string) (chan struct{}, bool) {
	if f == nil {
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.inGoing[key]; ok {
		return ch, false
	}
	ch := make(chan struct{})
	f.inGoing[key] = ch
	return ch, true
}

func (f *perKeyFlight) end(key string, ch chan struct{}) {
	if f == nil {
		return
	}
	f.mu.Lock()
	delete(f.inGoing, key)
	f.mu.Unlock()
	close(ch)
}

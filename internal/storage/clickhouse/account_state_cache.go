package clickhouse

import (
	"context"
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

// get returns a fresh cached state. Nil-safe (a zero-value reader in tests
// behaves as a permanent miss).
func (c *accountStateCache) get(account string) (AccountState, bool) {
	if c == nil {
		return AccountState{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[account]
	if !ok || time.Since(e.cachedAt) > AccountStateCacheTTL {
		return AccountState{}, false
	}
	return e.state, true
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

// AccountStateCached serves account state from the TTL cache, falling
// through to a live read (and populating the cache) on a miss. This is the
// method the API detail handlers should call — see accountStateCache's
// godoc for why (site-audit follow-up: /v1/accounts/{g} and /v1/issuers/{g}
// were 6-8s under concurrent load).
//
// A single-flight collapse per account keeps a burst of concurrent misses
// for the SAME hot account from each launching its own scan.
func (r *ExplorerReader) AccountStateCached(ctx context.Context, account string) (AccountState, error) {
	if st, ok := r.stateCache.get(account); ok {
		return st, nil
	}
	// Not single-flighted across accounts on purpose — distinct accounts
	// genuinely need distinct scans. Only the exact-same-account burst is
	// worth collapsing, which the per-account flight below handles.
	ch, owner := r.stateFlight.begin(account)
	if !owner {
		// Someone else is scanning this account; wait briefly for their
		// result rather than launch a duplicate scan.
		select {
		case <-ch:
			if st, ok := r.stateCache.get(account); ok {
				return st, nil
			}
		case <-ctx.Done():
			return AccountState{}, ctx.Err()
		}
		// Fall through to a live read if the waited-for refresh produced
		// nothing cacheable.
	} else {
		defer r.stateFlight.end(account, ch)
	}
	st, err := r.AccountState(ctx, account)
	if err != nil {
		return AccountState{}, err
	}
	r.stateCache.put(account, st, time.Now())
	return st, nil
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

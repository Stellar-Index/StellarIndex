package clickhouse

import "errors"

// ErrRefreshSaturated is returned by a cache-fill method (AccountStateCached)
// to a COLD-path waiter when the shared detached-refresh gate was saturated
// and the refresh was SKIPPED rather than queued (TryAcquire returned false).
// It is a TRANSIENT capacity/backpressure condition — the caller, and any HTTP
// handler above it, must treat it as retryable (503 + retry), NOT as an
// internal error (500). It is deliberately DISTINCT from
// errAccountStateRefreshFailed, which signals a genuine refresh failure (the
// scan ran and errored) and stays on the 500 path so an alert fires.
var ErrRefreshSaturated = errors.New("clickhouse: detached refresh capacity saturated; retry shortly")

// RefreshGate is a small non-blocking semaphore bounding how many DETACHED
// cache refreshes may run concurrently against the explorer's lake pool
// (audit 2026-07-31).
//
// Why it exists: the explorer's stale-while-revalidate caches (account
// state here; asset holders / contracts directory / contract detail in
// internal/api/v1/explorer) each single-flight PER KEY — but the key space
// is attacker-chosen on unauthenticated routes. Churning fabricated
// G-addresses (every one shape-valid, every one a cache miss) launched one
// detached multi-minute lake scan PER KEY with no bound across keys, all
// contending on the 8-connection serving pool — an unauthenticated
// amplification from cheap requests to unbounded expensive scans.
//
// The gate deliberately SKIPS on saturation rather than queueing: a
// refresh that can't start simply doesn't (the caller serves whatever is
// cached, or misses honestly and the next request retries). Queueing would
// just move the unbounded backlog from the pool into the gate. Legitimate
// traffic is unaffected in steady state — prewarm loops and TTL refreshes
// re-kick on their next pass — while an attacker is capped at `limit`
// concurrent scans instead of thousands.
//
// A nil *RefreshGate admits everything (handy for test stubs).
type RefreshGate struct {
	sem chan struct{}
}

// DefaultDetachedRefreshLimit is the production bound on concurrently
// running detached refreshes. Half the 8-connection explorer pool: worst
// case the detached tier can never consume every connection, so inline
// request-path reads always have headroom.
const DefaultDetachedRefreshLimit = 4

// NewRefreshGate returns a gate admitting at most limit concurrent
// holders.
func NewRefreshGate(limit int) *RefreshGate {
	return &RefreshGate{sem: make(chan struct{}, limit)}
}

// TryAcquire claims a slot without blocking; false means saturated (the
// caller must skip its refresh, not queue).
func (g *RefreshGate) TryAcquire() bool {
	if g == nil {
		return true
	}
	select {
	case g.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns a slot claimed by a successful TryAcquire.
func (g *RefreshGate) Release() {
	if g == nil {
		return
	}
	<-g.sem
}

// DetachedRefreshGate exposes the reader's gate so the API-layer explorer
// caches (internal/api/v1/explorer) share ONE global bound with the
// reader's own account-state refreshes — four independent gates of 4
// would multiply back into pool exhaustion.
func (r *ExplorerReader) DetachedRefreshGate() *RefreshGate {
	return r.refreshGate
}

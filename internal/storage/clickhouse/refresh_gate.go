package clickhouse

import (
	"errors"
	"sync"
)

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
//
// PER-CLASS FAIRNESS (inventory #26 item 5, second half): the single
// global bound stopped the amplification but let one key CLASS starve
// the rest — a crawler churning fabricated contract ids could hold all
// 4 slots with contract-detail refreshes, and every cold account /
// holders / directory page then fast-503d behind it. TryAcquireClass
// additionally caps each class at half the global limit, so a burst in
// one class leaves headroom for the others while the global bound (the
// pool-safety property) is unchanged.
type RefreshGate struct {
	sem chan struct{}

	mu      sync.Mutex
	classes map[string]chan struct{}
}

// DefaultDetachedRefreshLimit is the production bound on concurrently
// running detached refreshes. Half the explorer pool: worst case the
// detached tier can never consume every connection, so inline
// request-path reads always have headroom.
//
// Sized against a PAGE, not a request (2026-08-13). At 4 — half the old
// 8-connection pool — a single cold contract page could not fill
// itself: it fans out to five reads, so even with per-panel classes the
// global bound refused some, and a second visitor had nothing left.
// Explorer traffic is inherently fan-out traffic, and the previous
// figure was below one page's width. The pool moved 8 -> 16 with it, so
// the "detached can never take the whole pool" invariant is unchanged;
// r1 has 20 cores and idles at ~2 concurrent ClickHouse queries, and
// every explorer scan carries max_threads = 4, so the ceiling this
// implies is well within the host.
const DefaultDetachedRefreshLimit = 8

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

// classSem returns (lazily creating) the per-class semaphore, capped at
// half the global limit (min 1).
func (g *RefreshGate) classSem(class string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.classes == nil {
		g.classes = make(map[string]chan struct{})
	}
	sem, ok := g.classes[class]
	if !ok {
		limit := cap(g.sem) / 2
		if limit < 1 {
			limit = 1
		}
		sem = make(chan struct{}, limit)
		g.classes[class] = sem
	}
	return sem
}

// TryAcquireClass claims a slot for a named refresh class without
// blocking: the class must be under its own cap (half the global
// limit) AND the global bound must have room. False means skip the
// refresh — same contract as TryAcquire.
func (g *RefreshGate) TryAcquireClass(class string) bool {
	if g == nil {
		return true
	}
	sem := g.classSem(class)
	select {
	case sem <- struct{}{}:
	default:
		return false
	}
	select {
	case g.sem <- struct{}{}:
		return true
	default:
		<-sem // give the class token back — all-or-nothing
		return false
	}
}

// ReleaseClass returns both tokens claimed by a successful
// TryAcquireClass.
func (g *RefreshGate) ReleaseClass(class string) {
	if g == nil {
		return
	}
	<-g.sem
	<-g.classSem(class)
}

// DetachedRefreshGate exposes the reader's gate so the API-layer explorer
// caches (internal/api/v1/explorer) share ONE global bound with the
// reader's own account-state refreshes — four independent gates of 4
// would multiply back into pool exhaustion.
func (r *ExplorerReader) DetachedRefreshGate() *RefreshGate {
	return r.refreshGate
}

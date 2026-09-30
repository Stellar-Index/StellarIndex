package clickhouse

import (
	"errors"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
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
// therefore caps each client-keyed class at a quarter of the global limit
// (so several driven classes cannot fill the pool between them) and keeps
// one slot that no client-keyed class may take, so the server-keyed
// prewarm refreshes always have room. The global bound (the pool-safety
// property) is unchanged.
type RefreshGate struct {
	sem chan struct{}

	mu      sync.Mutex
	classes map[string]int // slots held per class
	client  int            // slots held by client-keyed classes
}

// serverKeyedClasses are the refresh classes whose keys the server fixes
// (one or a handful, kicked by operator prewarm loops). Every other class
// is keyed on caller-chosen ids, so a client can mint cold keys at will.
var serverKeyedClasses = map[string]bool{
	"contracts_dir":      true,
	"network_throughput": true,
	"ops_directory":      true,
	"protocol_bespoke":   true,
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
		obs.ExplorerRefreshGateSaturatedTotal.WithLabelValues("unclassed", "global").Inc()
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

// TryAcquireClass claims a slot for a named refresh class without
// blocking. A client-keyed class may hold a quarter of the global limit
// and, with the other client-keyed classes, all but one slot; a
// server-keyed class may hold half. False means skip the refresh — same
// contract as TryAcquire. Every refusal is counted on
// obs.ExplorerRefreshGateSaturatedTotal, since the caller only sees a 503.
func (g *RefreshGate) TryAcquireClass(class string) bool {
	if g == nil {
		return true
	}
	limit := cap(g.sem)
	serverKeyed := serverKeyedClasses[class]
	classCap := max(1, limit/4)
	if serverKeyed {
		classCap = max(1, limit/2)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.classes[class] >= classCap {
		obs.ExplorerRefreshGateSaturatedTotal.WithLabelValues(class, "class").Inc()
		return false
	}
	if !serverKeyed && limit > 1 && g.client >= limit-1 {
		obs.ExplorerRefreshGateSaturatedTotal.WithLabelValues(class, "global").Inc()
		return false
	}
	select {
	case g.sem <- struct{}{}:
	default:
		obs.ExplorerRefreshGateSaturatedTotal.WithLabelValues(class, "global").Inc()
		return false
	}
	if g.classes == nil {
		g.classes = make(map[string]int)
	}
	g.classes[class]++
	if !serverKeyed {
		g.client++
	}
	return true
}

// ReleaseClass returns the slot claimed by a successful TryAcquireClass.
func (g *RefreshGate) ReleaseClass(class string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	<-g.sem
	if g.classes[class]--; g.classes[class] <= 0 {
		delete(g.classes, class)
	}
	if !serverKeyedClasses[class] {
		g.client--
	}
}

// DetachedRefreshGate exposes the reader's gate so the API-layer explorer
// caches (internal/api/v1/explorer) share ONE global bound with the
// reader's own account-state refreshes — four independent gates of 4
// would multiply back into pool exhaustion.
func (r *ExplorerReader) DetachedRefreshGate() *RefreshGate {
	return r.refreshGate
}

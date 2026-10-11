package clickhouse

import (
	"errors"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ErrRefreshSaturated is returned to a cold-path waiter when the detached-refresh gate was
// saturated and the refresh was skipped, not queued. It is transient backpressure: handlers
// must answer retryable (503), not 500. It is distinct from errAccountStateRefreshFailed (the
// scan ran and errored), which stays on the 500 path so an alert fires.
var ErrRefreshSaturated = errors.New("clickhouse: detached refresh capacity saturated; retry shortly")

// RefreshGate is a small non-blocking semaphore bounding how many DETACHED
// cache refreshes may run concurrently against the explorer's lake pool.
//
// The stale-while-revalidate caches single-flight PER KEY, but the key space is
// attacker-chosen on unauthenticated routes: fabricated shape-valid addresses would each
// launch a detached multi-minute lake scan with no bound across keys.
//
// The gate SKIPS on saturation rather than queueing: the caller serves what is cached, or
// misses honestly and the next request retries. Queueing would only move the unbounded
// backlog into the gate. A nil *RefreshGate admits everything (test stubs).
//
// PER-CLASS FAIRNESS: a global bound alone lets one key class starve the rest.
// TryAcquireClass caps each client-keyed class at a quarter of the global limit and keeps one
// slot no client-keyed class may take, so server-keyed prewarm refreshes always have room.
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
	"contract_stats":     true,
	"network_throughput": true,
	"ops_directory":      true,
	"protocol_bespoke":   true,
}

// DefaultDetachedRefreshLimit is the production bound on concurrently
// running detached refreshes: half the 16-connection explorer pool, so inline request-path
// reads keep headroom. Sized against a PAGE, not a request: a cold contract page fans out to
// five reads, so a smaller bound would refuse some of its own panels.
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
// blocking. A client-keyed class may hold a quarter of the global limit (all client-keyed
// classes together, all but one slot); a server-keyed class may hold half. False means skip
// the refresh, as TryAcquire. Refusals are counted on
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

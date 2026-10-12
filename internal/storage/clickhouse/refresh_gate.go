package clickhouse

import (
	"errors"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ErrRefreshSaturated is returned to a cold-path waiter when the detached-refresh gate skipped (not queued) the refresh: transient backpressure, handlers must answer retryable 503, not 500.
var ErrRefreshSaturated = errors.New("clickhouse: detached refresh capacity saturated; retry shortly")

// RefreshGate is a non-blocking semaphore bounding concurrent DETACHED cache refreshes against the lake pool (the key space is attacker-chosen on unauthenticated routes); it skips on saturation, per-class capped via TryAcquireClass, and a nil gate admits everything.
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

// DefaultDetachedRefreshLimit is the production bound on concurrent detached refreshes: half the 16-connection explorer pool, sized against a PAGE (a cold contract page fans out to five reads).
const DefaultDetachedRefreshLimit = 8

// NewRefreshGate returns a gate admitting at most limit concurrent holders.
func NewRefreshGate(limit int) *RefreshGate {
	return &RefreshGate{sem: make(chan struct{}, limit)}
}

// TryAcquire claims a slot without blocking; false means saturated and the caller must skip its refresh, not queue.
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

// TryAcquireClass claims a slot for a named refresh class without blocking (client-keyed classes capped at a quarter of the limit, server-keyed at half); false means skip, and refusals are counted on obs.ExplorerRefreshGateSaturatedTotal.
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

// DetachedRefreshGate exposes the reader's gate so the API-layer explorer caches share ONE global bound with the reader's own refreshes.
func (r *ExplorerReader) DetachedRefreshGate() *RefreshGate {
	return r.refreshGate
}

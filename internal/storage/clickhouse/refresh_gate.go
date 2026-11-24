package clickhouse

import (
	"context"
	"errors"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ErrRefreshSaturated is returned to a cold-path waiter when the detached-refresh gate skipped (not queued) the refresh: transient backpressure, handlers must answer retryable 503, not 500.
var ErrRefreshSaturated = errors.New("clickhouse: detached refresh capacity saturated; retry shortly")

// RefreshGate is a non-blocking semaphore bounding concurrent DETACHED cache refreshes against the lake pool (the key space is attacker-chosen on unauthenticated routes); it skips on saturation (AcquireClassWait: a bounded wait), per-class capped via TryAcquireClass, and a nil gate admits everything.
type RefreshGate struct {
	sem chan struct{}

	mu      sync.Mutex
	classes map[string]int // slots held per class
	client  int            // slots held by client-keyed classes
	waiting map[string]int // AcquireClassWait callers parked per class
	freed   chan struct{}  // closed and replaced on every release; wakes waiters
}

// classWaitersPerSlot bounds AcquireClassWait's queue at this many waiters per class slot: a burst of distinct cold keys drains at
// gate speed instead of shedding, while waiters past it (an attacker minting keys) still shed at once.
const classWaitersPerSlot = 4

// serverKeyedClasses are the refresh classes whose keys the server fixes (one or a handful, kicked by operator prewarm loops); every
// other class is keyed on caller-chosen ids, so a client can mint cold keys at will.
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
	g.mu.Lock()
	defer g.mu.Unlock()
	<-g.sem
	g.wakeLocked()
}

// classCap is class's share of the gate: a quarter of the limit for client-keyed classes, half for server-keyed.
func (g *RefreshGate) classCap(class string) int {
	if serverKeyedClasses[class] {
		return max(1, cap(g.sem)/2)
	}
	return max(1, cap(g.sem)/4)
}

// TryAcquireClass claims a slot for a named refresh class without blocking (see classCap); false means skip, and refusals are
// counted on obs.ExplorerRefreshGateSaturatedTotal.
func (g *RefreshGate) TryAcquireClass(class string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	bound := g.tryAcquireClassLocked(class)
	g.mu.Unlock()
	if bound != "" {
		obs.ExplorerRefreshGateSaturatedTotal.WithLabelValues(class, bound).Inc()
		return false
	}
	return true
}

// AcquireClassWait is TryAcquireClass for a COLD read with a caller waiting on it: while ctx lives it waits for a slot, in a queue
// capped at classWaitersPerSlot per class slot. It sheds (false, counted like a TryAcquireClass refusal) only when that queue is
// full or ctx ends first; it never admits past the gate's bounds.
func (g *RefreshGate) AcquireClassWait(ctx context.Context, class string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	bound := g.tryAcquireClassLocked(class)
	if bound == "" {
		g.mu.Unlock()
		return true
	}
	if g.waiting[class] >= classWaitersPerSlot*g.classCap(class) {
		g.mu.Unlock()
		obs.ExplorerRefreshGateSaturatedTotal.WithLabelValues(class, bound).Inc()
		return false
	}
	if g.waiting == nil {
		g.waiting = make(map[string]int)
	}
	g.waiting[class]++
	defer func() {
		if g.waiting[class]--; g.waiting[class] <= 0 {
			delete(g.waiting, class)
		}
		g.mu.Unlock()
		if bound != "" {
			obs.ExplorerRefreshGateSaturatedTotal.WithLabelValues(class, bound).Inc()
		}
	}()
	for bound != "" {
		if g.freed == nil {
			g.freed = make(chan struct{})
		}
		freed := g.freed
		g.mu.Unlock()
		select {
		case <-freed:
		case <-ctx.Done():
			g.mu.Lock()
			return false
		}
		g.mu.Lock()
		bound = g.tryAcquireClassLocked(class)
	}
	return true
}

// tryAcquireClassLocked claims a slot for class, returning "" on success or the refusing bound ("class" | "global"). Caller holds g.mu.
func (g *RefreshGate) tryAcquireClassLocked(class string) string {
	limit := cap(g.sem)
	serverKeyed := serverKeyedClasses[class]
	if g.classes[class] >= g.classCap(class) {
		return "class"
	}
	if !serverKeyed && limit > 1 && g.client >= limit-1 {
		return "global"
	}
	select {
	case g.sem <- struct{}{}:
	default:
		return "global"
	}
	if g.classes == nil {
		g.classes = make(map[string]int)
	}
	g.classes[class]++
	if !serverKeyed {
		g.client++
	}
	return ""
}

// wakeLocked wakes every AcquireClassWait waiter to retry. Caller holds g.mu.
func (g *RefreshGate) wakeLocked() {
	if g.freed != nil {
		close(g.freed)
		g.freed = nil
	}
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
	g.wakeLocked()
}

// DetachedRefreshGate exposes the reader's gate so the API-layer explorer caches share ONE global bound with the reader's own refreshes.
func (r *ExplorerReader) DetachedRefreshGate() *RefreshGate {
	return r.refreshGate
}

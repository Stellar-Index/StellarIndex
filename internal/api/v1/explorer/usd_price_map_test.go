package explorer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func resetUSDPriceMapCache(t *testing.T) {
	t.Helper()
	reset := func() {
		usdPriceMapMu.Lock()
		usdPriceMapCache = usdPriceMapEntry{}
		usdPriceMapMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// Concurrent misses must share one walk, not each run the full sequential
// price lookup.
func TestUSDPriceMap_ConcurrentMissesShareOneWalk(t *testing.T) {
	resetUSDPriceMapCache(t)
	const callers = 8
	var lookups atomic.Int32
	h := &Handler{LookupUSDPrice: func(context.Context, canonical.Asset) (string, bool) {
		// Hold the walk open until every caller could have joined it.
		lookups.Add(1)
		deadline := time.Now().Add(200 * time.Millisecond)
		for lookups.Load() < callers && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return "0.1", true
	}}

	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a, _ := h.usdPriceMap(context.Background()); len(a) != 1 || a[0] != "native" {
				t.Errorf("assets = %v, want [native]", a)
			}
		}()
	}
	wg.Wait()
	if got := lookups.Load(); got != 1 {
		t.Fatalf("LookupUSDPrice ran %d times across %d concurrent callers, want 1", got, callers)
	}
}

// An empty map (pricing down) is cached briefly so each request does not
// re-run the walk while the outage lasts.
func TestUSDPriceMap_EmptyResultIsCached(t *testing.T) {
	resetUSDPriceMapCache(t)
	var lookups atomic.Int32
	h := &Handler{LookupUSDPrice: func(context.Context, canonical.Asset) (string, bool) {
		lookups.Add(1)
		return "", false
	}}
	for range 3 {
		if a, _ := h.usdPriceMap(context.Background()); len(a) != 0 {
			t.Fatalf("assets = %v, want empty", a)
		}
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("LookupUSDPrice ran %d times over 3 calls, want 1", got)
	}

	usdPriceMapMu.Lock()
	usdPriceMapCache.cachedAt = time.Now().Add(-usdPriceMapEmptyTTL)
	usdPriceMapMu.Unlock()
	h.usdPriceMap(context.Background())
	if got := lookups.Load(); got != 2 {
		t.Fatalf("expired empty entry: LookupUSDPrice ran %d times, want 2", got)
	}
}

// One caller's cancellation must not truncate the walk every waiter shares:
// the walk it started still fills the cache with the full result.
func TestUSDPriceMap_CallerCancelDoesNotTruncate(t *testing.T) {
	resetUSDPriceMapCache(t)
	var lookups atomic.Int32
	h := &Handler{LookupUSDPrice: func(ctx context.Context, _ canonical.Asset) (string, bool) {
		lookups.Add(1)
		if ctx.Err() != nil {
			return "", false
		}
		return "0.1", true
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.usdPriceMap(ctx)
	// Joins the detached walk if still in flight, else reads what it cached.
	if a, _ := h.usdPriceMap(context.Background()); len(a) != 1 || a[0] != "native" {
		t.Fatalf("assets = %v, want [native]", a)
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("LookupUSDPrice ran %d times, want 1 (the cancelled caller's walk)", got)
	}
}

// A caller's own deadline must bound its wait on a slow shared walk: it gets
// the last (expired) entry back on time, and the walk still refreshes the cache.
func TestUSDPriceMap_ShortDeadlineReturnsWhileWalkInFlight(t *testing.T) {
	resetUSDPriceMapCache(t)
	usdPriceMapMu.Lock()
	usdPriceMapCache = usdPriceMapEntry{
		assets: []string{"native"}, prices: []string{"0.2"},
		cachedAt: time.Now().Add(-2 * usdPriceMapTTL),
	}
	usdPriceMapMu.Unlock()
	release := make(chan struct{})
	time.AfterFunc(time.Second, func() { close(release) })
	h := &Handler{LookupUSDPrice: func(context.Context, canonical.Asset) (string, bool) {
		<-release
		return "0.1", true
	}}

	const deadline = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	_, p := h.usdPriceMap(ctx)
	if elapsed := time.Since(start); elapsed > deadline+200*time.Millisecond {
		t.Fatalf("usdPriceMap returned after %v with a %v deadline; the caller waited on the walk", elapsed, deadline)
	}
	if len(p) != 1 || p[0] != "0.2" {
		t.Fatalf("prices = %v, want the expired entry [0.2]", p)
	}

	if _, p := h.usdPriceMap(context.Background()); len(p) != 1 || p[0] != "0.1" {
		t.Fatalf("after the walk: prices = %v, want [0.1]", p)
	}
}

// A panic in the shared walk must fail only the waiting callers: they get the
// last entry back, the cache is left as it was, and the next call retries.
func TestUSDPriceMap_FillPanicServesLastEntry(t *testing.T) {
	resetUSDPriceMapCache(t)
	stale := time.Now().Add(-2 * usdPriceMapTTL)
	usdPriceMapMu.Lock()
	usdPriceMapCache = usdPriceMapEntry{assets: []string{"native"}, prices: []string{"0.2"}, cachedAt: stale}
	usdPriceMapMu.Unlock()
	var lookups atomic.Int32
	h := &Handler{LookupUSDPrice: func(context.Context, canonical.Asset) (string, bool) {
		lookups.Add(1)
		panic("boom")
	}}
	if _, p := h.usdPriceMap(context.Background()); len(p) != 1 || p[0] != "0.2" {
		t.Fatalf("prices = %v, want the last entry [0.2]", p)
	}
	if c := loadUSDPriceMapCache(); !c.cachedAt.Equal(stale) {
		t.Fatalf("cache cachedAt = %v, want untouched %v", c.cachedAt, stale)
	}
	h.usdPriceMap(context.Background())
	if got := lookups.Load(); got != 2 {
		t.Fatalf("LookupUSDPrice ran %d times over 2 calls, want 2 (a panicked walk is retried)", got)
	}
}

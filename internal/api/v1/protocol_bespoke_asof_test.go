package v1

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// cachedBespoke is cachedBespokeEntry's block, staleness and ok, the shape the cache tests assert on.
func (s *Server) cachedBespoke(ctx context.Context, source, category string, windowDays int) (*timescale.BespokeBlock, bool, bool) {
	e, stale, ok := s.cachedBespokeEntry(ctx, source, category, windowDays)
	return e.blk, stale, ok
}

// A refresh that lands while the detail view is being built must not lend its build time to the older block being served.
func TestEnrichBespoke_BuiltAtIsTheServedBlocksTime(t *testing.T) {
	stub := &bespokeStub{block: make(chan struct{})}
	srv := New(Options{ProtocolBespoke: stub})
	key := bespokeTestKey()

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var (
		mu    sync.Mutex
		now   = t0
		armed atomic.Bool
	)
	srv.protocolBespokeCache.now = func() time.Time {
		if armed.CompareAndSwap(true, false) {
			// The staleness read inside the served lookup: let the detached refresh put a newer entry before the caller continues.
			mu.Lock()
			now = t0.Add(bespokeStaleAfter + time.Minute)
			mu.Unlock()
			close(stub.block)
			waitBespokeIdle(t, srv, key)
		}
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	old := &timescale.BespokeBlock{Category: "bridge", KPIs: []timescale.BespokeKPI{{Label: "old", Value: "1"}}}
	srv.protocolBespokeCache.put(key, old)
	armed.Store(true)

	var view ProtocolDetailView
	ok, stale, builtAt := srv.enrichBespoke(context.Background(), ProtocolMeta{Name: "cctp", Category: "bridge"}, &view, 90)
	if !ok || view.Bespoke == nil || len(view.Bespoke.KPIs) != 1 || view.Bespoke.KPIs[0].Label != "old" {
		t.Fatalf("served block: ok=%v view=%+v, want the old block", ok, view.Bespoke)
	}
	if !stale {
		t.Errorf("stale=false, want true for a block older than bespokeStaleAfter")
	}
	if !builtAt.Equal(t0) {
		t.Errorf("builtAt=%v, want the served block's build time %v", builtAt, t0)
	}
	if e, _ := srv.protocolBespokeCache.get(key); !e.at.After(t0) {
		t.Fatalf("test setup: refresh never landed a newer entry (at=%v)", e.at)
	}
}

package v1

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// panickyPoolsUpstream panics inside AllPools while armed, as a refresh scan that blows up.
type panickyPoolsUpstream struct {
	*gatedMarketsUpstream
	armed atomic.Bool
}

func (u *panickyPoolsUpstream) AllPools(ctx context.Context, f timescale.PoolsFilter, cursor string, limit int, o timescale.MarketsOrder) ([]Pool, string, error) {
	if u.armed.Load() {
		panic("pools scan blew up")
	}
	return u.gatedMarketsUpstream.AllPools(ctx, f, cursor, limit, o)
}

func newPoolsSWRCase(t *testing.T) (*CachedMarketsReader, *swrTestClock, *panickyPoolsUpstream, func() swrFlagsServe) {
	t.Helper()
	clk := &swrTestClock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	up := &panickyPoolsUpstream{gatedMarketsUpstream: &gatedMarketsUpstream{}}
	cache := NewCachedMarketsReader(up, 30*time.Second)
	cache.now, cache.sparks.now = clk.now, clk.now
	h := New(Options{Markets: cache}).Handler()
	serve := func() swrFlagsServe { return getSWRFlags(t, h, "/v1/pools") }
	if got := serve(); got.stale || got.noStore {
		t.Fatalf("cold fill: stale=%v no-store=%v", got.stale, got.noStore)
	}
	return cache, clk, up, serve
}

// A serve past the TTL whose refresh is SKIPPED because every refresh slot is busy has nothing replacing it, so it is stale;
// no refresh of it failed or overran, so it is not degraded and keeps the route band.
func TestMarketsSWR_SkippedRefreshIsFlagged(t *testing.T) {
	cache, clk, _, serve := newPoolsSWRCase(t)
	clk.add(30*time.Second + time.Second)
	var held []chan struct{}
	for _, head := range []bool{true, false} {
		slots := cache.refreshSlots(head)
		for len(slots) < cap(slots) {
			slots <- struct{}{}
		}
		held = append(held, slots)
	}
	got := serve()
	for _, slots := range held {
		for len(slots) > 0 {
			<-slots
		}
	}
	if !got.stale || got.noStore {
		t.Errorf("refresh skipped, slots full: stale=%v no-store=%v, want stale only", got.stale, got.noStore)
	}
	waitMarketsIdle(t, cache)
}

// A refresh that panics is a failed refresh: the next serve is flagged.
func TestMarketsSWR_PanickedRefreshIsFlagged(t *testing.T) {
	cache, clk, up, serve := newPoolsSWRCase(t)
	clk.add(30*time.Second + time.Second)
	up.armed.Store(true)
	serve() // kicks the refresh that panics
	waitMarketsIdle(t, cache)
	up.armed.Store(false)
	up.hold()
	got := serve()
	up.release(false)
	waitMarketsIdle(t, cache)
	if !got.stale || !got.noStore {
		t.Errorf("after a panicked refresh: stale=%v no-store=%v, want both true", got.stale, got.noStore)
	}
}

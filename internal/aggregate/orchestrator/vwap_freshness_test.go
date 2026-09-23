package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestTick_LongWindowVWAP_StopsServingAfterTheSilenceGrace is F034 at
// the production entry point: the value the windowed /v1/price surface
// reads is written by Tick, and it must not outlive the aggregator.
//
// The scenario the finding describes: the aggregator stops (crash,
// deploy, OOM, failing Redis writes) at T0. Before this bound the
// `vwap:<pair>:86400` key lived for the WINDOW — 24 h — and every
// `GET /v1/price?asset=…&window=86400` in between returned HTTP 200
// with `observed_at` stamped at request time and `flags.stale` unset,
// i.e. a price computed the previous day asserted as current, with no
// field on the wire able to reveal it.
//
// Asserted through the real write path (Tick → refreshPairWindow →
// cachekeys.VWAPTTL) rather than against the TTL function alone, so a
// writer that stops honouring the bound fails here too.
func TestTick_LongWindowVWAP_StopsServingAfterTheSilenceGrace(t *testing.T) {
	pair := xlmUsdtPair(t)
	cache, mr := newTestRedis(t)
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(100_000_000), big.NewInt(21_000_000), time.Now()),
			buildTrade(t, big.NewInt(200_000_000), big.NewInt(42_000_000), time.Now()),
		},
	}

	// The 24 h window /v1/price?window=86400 serves.
	const window = 24 * time.Hour
	o := New(store, cache, Config{
		Pairs:   []canonical.Pair{pair},
		Windows: []time.Duration{window},
	})

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	key := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	if !mr.Exists(key) {
		t.Fatalf("precondition: Tick did not publish %s", key)
	}
	if ttl := mr.TTL(key); ttl > cachekeys.VWAPMaxAge {
		t.Errorf("TTL(%s) = %v, want ≤ the %v silence grace — a 24h window is an "+
			"aggregation span, not a freshness claim; this key is re-written every tick",
			key, ttl, cachekeys.VWAPMaxAge)
	}

	// The aggregator stops here: no further ticks. One grace later the
	// value must be gone, so the windowed handler answers its
	// documented 404 instead of stamping observed_at=now on it.
	mr.FastForward(cachekeys.VWAPMaxAge + time.Minute)
	if mr.Exists(key) {
		v, _ := mr.Get(key)
		t.Errorf("%s still serves %q %v after the last publish — a stopped aggregator's "+
			"VWAP must expire, not be served as a current price",
			key, v, cachekeys.VWAPMaxAge+time.Minute)
	}

	// And a live aggregator keeps it alive: one more tick re-publishes
	// the key, so the bound costs nothing while the writer is running.
	nextBucket(o)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick after grace: %v", err)
	}
	if !mr.Exists(key) {
		t.Errorf("%s missing after a fresh tick — the bound must not stop a running "+
			"aggregator from serving", key)
	}
}

// TestTick_ShortWindowVWAP_KeepsItsWindowTTL — the bound only ever
// TIGHTENS. The 5 m window is already inside the grace and must keep
// its own window as the TTL, unchanged from the pre-fix behaviour.
func TestTick_ShortWindowVWAP_KeepsItsWindowTTL(t *testing.T) {
	pair := xlmUsdtPair(t)
	cache, mr := newTestRedis(t)
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(100_000_000), big.NewInt(21_000_000), time.Now()),
		},
	}

	const window = 5 * time.Minute
	o := New(store, cache, Config{
		Pairs:   []canonical.Pair{pair},
		Windows: []time.Duration{window},
	})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	key := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	if ttl := mr.TTL(key); ttl != window {
		t.Errorf("TTL(%s) = %v, want the untouched %v window TTL", key, ttl, window)
	}
}

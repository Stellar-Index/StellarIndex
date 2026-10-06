package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestTick_AnomalyFreeze_StreamCarriesAMarker: a pair that freezes
// mid-stream must put the refused bucket on the closed-bucket stream as a
// frozen marker, once per bucket, or /v1/price/stream subscribers see only
// keepalives while /v1/price serves flags.frozen — a freeze would read as
// a quiet market.
func TestTick_AnomalyFreeze_StreamCarriesAMarker(t *testing.T) {
	pair := xlmUsdtPair(t)
	window := 5 * time.Minute
	cache, _ := newTestRedis(t)
	stream := &recordingStreamPublisher{}
	o := New(nil, cache, Config{
		Pairs:           []canonical.Pair{pair},
		Windows:         []time.Duration{window},
		Anomaly:         newAnomalyChecker(t, pair),
		FreezeWriter:    &recordingFreezeMarker{},
		StreamPublisher: stream,
	})
	t0 := time.Now().UTC().Truncate(closedBucket).Add(10 * time.Second)
	firstBucket := t0.Truncate(closedBucket)
	o.prevVWAPs[pair.String()+":"+window.String()] = big.NewRat(1, 1)
	o.store = &mockStore{trades: []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), t0.Add(-30*time.Second)),
	}}

	tick := func(at time.Time) {
		t.Helper()
		o.clock = func() time.Time { return at }
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	tick(t0)
	tick(t0.Add(20 * time.Second)) // replays the decided bucket
	tick(t0.Add(closedBucket))     // next bucket, still inside the hold

	if len(stream.calls) != 0 {
		t.Errorf("a refused bucket was published as a price: %+v", stream.calls)
	}
	if len(stream.frozen) != 2 {
		t.Fatalf("frozen markers = %d, want one per refused bucket (2): %+v", len(stream.frozen), stream.frozen)
	}
	for i, want := range []time.Time{firstBucket, firstBucket.Add(closedBucket)} {
		got := stream.frozen[i]
		if got.pair.String() != pair.String() || got.window != window || !got.observedAt.Equal(want) {
			t.Errorf("marker %d = %+v, want %s/%s at %s", i, got, pair, window, want)
		}
		if !got.frozenSince.Equal(firstBucket) {
			t.Errorf("marker %d frozenSince = %s, want the first refused bucket %s", i, got.frozenSince, firstBucket)
		}
	}
}

// TestTriangulate_InheritedFreeze_StreamCarriesAMarker: a triangulated
// target that inherits its leg's freeze serves flags.frozen on /v1/price,
// so its stream must carry the same marker once per bucket, never a price.
func TestTriangulate_InheritedFreeze_StreamCarriesAMarker(t *testing.T) {
	leg1 := xlmUsdtPair(t)
	leg2 := mkPair(t, "crypto", "USDT", "fiat", "EUR")
	target := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute
	cache, _ := newTestRedis(t)
	stream := &recordingStreamPublisher{}
	o := New(nil, cache, Config{
		Pairs:           []canonical.Pair{leg1},
		Windows:         []time.Duration{window},
		Anomaly:         newAnomalyChecker(t, leg1),
		FreezeWriter:    &recordingFreezeMarker{},
		StreamPublisher: stream,
		Triangulations:  []TriangulationChain{{Target: target, Legs: []canonical.Pair{leg1, leg2}}},
	})
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(closedBucket).Add(10 * time.Second)
	firstBucket := t0.Truncate(closedBucket)
	cache.Set(ctx, cachekeys.VWAP(leg1.Base, leg1.Quote, window).String(), "1.000000000000", time.Hour)
	cache.Set(ctx, cachekeys.VWAP(leg2.Base, leg2.Quote, window).String(), "0.900000000000", time.Hour)
	o.prevVWAPs[leg1.String()+":"+window.String()] = big.NewRat(1, 1)
	o.store = &mockStore{trades: []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), t0.Add(-30*time.Second)),
	}}

	for _, at := range []time.Time{t0, t0.Add(20 * time.Second), t0.Add(closedBucket)} {
		o.clock = func() time.Time { return at }
		if err := o.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}

	for _, c := range stream.calls {
		if c.pair.String() == target.String() {
			t.Errorf("the inherited-frozen target was published as a price: %+v", c)
		}
	}
	var got []recordedFrozen
	for _, f := range stream.frozen {
		if f.pair.String() == target.String() {
			got = append(got, f)
		}
	}
	if len(got) != 2 {
		t.Fatalf("target frozen markers = %d, want one per refused bucket (2): %+v", len(got), stream.frozen)
	}
	for i, want := range []time.Time{firstBucket, firstBucket.Add(closedBucket)} {
		if got[i].window != window || !got[i].observedAt.Equal(want) {
			t.Errorf("marker %d = %+v, want %s at %s", i, got[i], window, want)
		}
		if !got[i].frozenSince.Equal(firstBucket) {
			t.Errorf("marker %d frozenSince = %s, want the leg freeze's first refused bucket %s",
				i, got[i].frozenSince, firstBucket)
		}
	}
}

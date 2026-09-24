package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// nextBucket moves o's clock one closed bucket past its current reading,
// so the next Tick decides a new bucket instead of replaying the last one.
func nextBucket(o *Orchestrator) {
	next := o.clock().Add(closedBucket)
	o.clock = func() time.Time { return next }
}

// rangeStore honours TradesInRange's [from, to) bounds, which mockStore
// ignores, and records every requested range.
type rangeStore struct {
	all    []canonical.Trade
	ranges [][2]time.Time
}

func (s *rangeStore) TradesInRange(_ context.Context, _ canonical.Pair, from, to time.Time, _ int) ([]canonical.Trade, error) {
	s.ranges = append(s.ranges, [2]time.Time{from, to})
	var out []canonical.Trade
	for _, tr := range s.all {
		if !tr.Timestamp.Before(from) && tr.Timestamp.Before(to) {
			out = append(out, tr)
		}
	}
	return out, nil
}

// ADR-0015: the served VWAP covers the window ending at the last CLOSED
// minute, never the in-progress one, and every tick inside that minute —
// whichever instance runs it — fetches and serves the same bucket.
func TestRefresh_ServesLastClosedBucketNotInProgress(t *testing.T) {
	pair := xlmUSDPair(t)
	window := 5 * time.Minute
	bucketEnd := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := &rangeStore{all: []canonical.Trade{
		makeXLMUSDTrade(t, "soroswap", lkgBaseAmount, lkgQuoteAmount, bucketEnd.Add(-3*time.Minute)),
		// In the in-progress minute: must not reach the served price.
		makeXLMUSDTrade(t, "soroswap", lkgBaseAmount, 2*lkgQuoteAmount, bucketEnd.Add(20*time.Second)),
	}}
	for _, offset := range []time.Duration{5 * time.Second, 35 * time.Second, 59 * time.Second} {
		store.ranges = nil
		rdb, mr := newTestRedis(t)
		o := New(store, rdb, Config{Pairs: []canonical.Pair{pair}, Windows: []time.Duration{window}, Interval: time.Hour})
		now := bucketEnd.Add(offset)
		o.clock = func() time.Time { return now }
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick at %s: %v", now, err)
		}
		if len(store.ranges) != 1 || !store.ranges[0][0].Equal(bucketEnd.Add(-window)) || !store.ranges[0][1].Equal(bucketEnd) {
			t.Errorf("tick at %s fetched %v, want [%s, %s)", now, store.ranges, bucketEnd.Add(-window), bucketEnd)
		}
		got, err := mr.Get(cachekeys.VWAP(pair.Base, pair.Quote, window).String())
		if err != nil {
			t.Fatalf("tick at %s: VWAP not published: %v", now, err)
		}
		if got != lkgFormatted {
			t.Errorf("tick at %s served %s, want the closed bucket's %s", now, got, lkgFormatted)
		}
	}
}

// A second tick inside an already-decided bucket replays the decision; it
// must not score the same data again. Scoring it earned a free "healthy
// bucket" (z=0 against itself) toward ADR-0019's two-bucket release.
func TestRefresh_SameBucketRetickDoesNotAdvanceReleaseStreak(t *testing.T) {
	f := newFreezeFixture(t)
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1242))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, time.Minute)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, time.Minute) // the jump back to LKG: streak 0
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, time.Minute) // first settled bucket: streak 1
	if got := f.state().UnfreezeStreak; got != 1 {
		t.Fatalf("setup: UnfreezeStreak = %d after one settled bucket, want 1", got)
	}

	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second) // same closed minute
	if got := f.state().UnfreezeStreak; got != 1 {
		t.Errorf("UnfreezeStreak = %d after a re-tick of the same bucket, want 1", got)
	}
	if !f.orch.frozenLeg(f.pair, freezeTestWindow) {
		t.Error("replayed frozen bucket is not refused as a triangulation leg")
	}
}

// A healthy single-venue market trending +1%/hour must not freeze at
// window boundaries. Truncating each window to its own size makes the
// 1h/24h VWAP a step function whose whole-window move lands in one
// scored step against a per-minute baseline.
func TestRefresh_TrendingMarketDoesNotFreezeAtWindowBoundaries(t *testing.T) {
	for _, window := range []time.Duration{time.Hour, 24 * time.Hour} {
		t.Run(window.String(), func(t *testing.T) {
			rdb, _ := newTestRedis(t)
			start := time.Date(2026, 7, 25, 8, 0, 0, 0, time.UTC)
			store := &rangeStore{}
			for i := 0; i < 6*120; i++ {
				q := new(big.Int).Mul(big.NewInt(lkgQuoteAmount), big.NewInt(int64(12000+i)))
				q.Div(q, big.NewInt(12000))
				store.all = append(store.all, makeXLMUSDTrade(t, "soroswap", lkgBaseAmount, q.Int64(),
					start.Add(time.Duration(i)*30*time.Second)))
			}
			writer, err := freeze.NewWriter(rdb, 0)
			if err != nil {
				t.Fatal(err)
			}
			pair := xlmUSDPair(t)
			o := New(store, rdb, Config{
				Pairs:        []canonical.Pair{pair},
				Windows:      []time.Duration{window},
				Interval:     30 * time.Second,
				FreezeWriter: writer,
				Baselines: stubBaselineSource{multi: baseline.MultiBaseline{
					Day30: &baseline.Baseline{Median: 0, MAD: 0.001, N: 43_199},
				}},
			})
			now := start.Add(2*time.Hour + 10*time.Second)
			o.clock = func() time.Time { return now }
			frozen := 0
			for k := 0; k < 240; k++ { // two hours of 30s ticks, across two hour boundaries
				if err := o.Tick(context.Background()); err != nil {
					t.Fatalf("Tick: %v", err)
				}
				if o.freezeStates[pair.String()+":"+window.String()].Active() {
					frozen++
				}
				now = now.Add(30 * time.Second)
			}
			if frozen > 0 {
				t.Errorf("a gently trending healthy market froze on %d of 240 ticks", frozen)
			}
		})
	}
}

// triangulateAll must snap FX at the tick's injected clock, not a second
// wall-clock read: the FX factor and the leg VWAPs it multiplies have to
// describe the same instant.
func TestTick_TriangulationFXSnapUsesInjectedClock(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	if err := mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000"); err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2019, 3, 4, 12, 43, 17, 0, time.UTC)
	fx := &fakeFXStore{
		quote:      big.NewRat(90, 100),
		observedAt: fixedNow.Add(-time.Hour),
		source:     "massive",
	}
	o := New(nil, cache, Config{
		Windows:        []time.Duration{window},
		Triangulations: []TriangulationChain{{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}}},
		FXStore:        fx,
	})
	o.clock = func() time.Time { return fixedNow }

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(fx.calls) != 1 {
		t.Fatalf("FXStore called %d times, want 1", len(fx.calls))
	}
	if want := fixedNow.Truncate(window); !fx.calls[0].cutoff.Equal(want) {
		t.Errorf("FX snap cutoff = %s, want %s from the injected clock", fx.calls[0].cutoff, want)
	}
}

// The publishing FX snap is held to the corroborator's admission rule: a
// quote older than the FX budget, or from a non-FX source, must not price
// a composite.
func TestTick_TriangulationRefusesUnusableFXSnap(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute
	fixedNow := time.Date(2019, 3, 4, 12, 43, 17, 0, time.UTC)

	for _, tc := range []struct {
		name       string
		observedAt time.Time
		source     string
		publishes  bool
	}{
		{"fresh FX quote", fixedNow.Add(-time.Hour), "massive", true},
		{"older than the FX budget", fixedNow.Add(-DefaultCompositeReferenceFXMaxAge - time.Hour), "massive", false},
		{"no observation time", time.Time{}, "massive", false},
		{"non-FX source", fixedNow.Add(-time.Hour), "band", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, mr := newTestRedis(t)
			if err := mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000"); err != nil {
				t.Fatal(err)
			}
			o := New(nil, cache, Config{
				Windows:        []time.Duration{window},
				Triangulations: []TriangulationChain{{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}}},
				FXStore:        &fakeFXStore{quote: big.NewRat(90, 100), observedAt: tc.observedAt, source: tc.source},
			})
			o.clock = func() time.Time { return fixedNow }
			if err := o.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			got, err := mr.Get(cachekeys.VWAP(xlmEUR.Base, xlmEUR.Quote, window).String())
			if tc.publishes && (err != nil || got != "0.072000000000") {
				t.Errorf("composite = %q (err %v), want 0.072000000000 = 0.08 x 0.90", got, err)
			}
			if !tc.publishes && err == nil {
				t.Errorf("composite %q published from an unusable FX snap", got)
			}
		})
	}
}

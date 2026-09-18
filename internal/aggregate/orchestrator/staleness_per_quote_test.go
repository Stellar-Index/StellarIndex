package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// stalenessTestClock is a hand-advanced time source for the
// orchestrator's injectable clock.
type stalenessTestClock struct{ now time.Time }

func (c *stalenessTestClock) Now() time.Time { return c.now }

func mustStalenessPair(t *testing.T, base, quote canonical.Asset) canonical.Pair {
	t.Helper()
	p, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair(%s, %s): %v", base, quote, err)
	}
	return p
}

func mustCrypto(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewCryptoAsset(code)
	if err != nil {
		t.Fatalf("NewCryptoAsset(%s): %v", code, err)
	}
	return a
}

func mustFiat(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewFiatAsset(code)
	if err != nil {
		t.Fatalf("NewFiatAsset(%s): %v", code, err)
	}
	return a
}

// liveTrades returns two exchange-class trades on `pair` stamped at
// `ts` — enough for a VWAP publish through the real Tick path.
func liveTrades(pair canonical.Pair, ts time.Time) []canonical.Trade {
	mk := func(op uint32, base, quote int64) canonical.Trade {
		return canonical.Trade{
			Source:      "binance",
			TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
			OpIndex:     op,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(base)),
			QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
		}
	}
	return []canonical.Trade{mk(0, 100_000_000, 21_000_000), mk(1, 200_000_000, 42_000_000)}
}

// TestTick_DeadQuoteIsNotMaskedByALiveSiblingQuote is F067 at the
// production entry point (Tick → refreshPairWindow → the write-time
// record → emitStalenessGauges).
//
// The scenario the finding describes: one base is configured against
// several quotes; one quote's feed goes dark while another keeps
// publishing. `stellarindex_price_staleness_seconds` is the ONLY input
// to the `stellarindex_api_price_stale` alert, and it used to be keyed
// by base alone — so every publish of the live quote reset the one
// timestamp the dead quote was judged by, and the gauge read 0 for an
// asset whose other quote had served nothing for ten minutes.
//
// BTC is used (not XLM) so the native ↔ crypto:XLM dual-form merge is
// not in play: this pins the quote dimension alone.
func TestTick_DeadQuoteIsNotMaskedByALiveSiblingQuote(t *testing.T) {
	btc := mustCrypto(t, "BTC")
	live := mustStalenessPair(t, btc, mustCrypto(t, "USDT"))
	dead := mustStalenessPair(t, btc, mustFiat(t, "GBP"))

	clk := &stalenessTestClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	cache, _ := newTestRedis(t)
	store := &mockStore{perPair: map[string][]canonical.Trade{
		live.String(): liveTrades(live, clk.now),
		// dead: absent → empty window every tick, never a write.
	}}
	o := New(store, cache, Config{
		Pairs:   []canonical.Pair{live, dead},
		Windows: []time.Duration{5 * time.Minute},
	})
	o.clock = clk.Now

	obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC").Set(-1)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	if got := testutil.ToFloat64(obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC")); got != 0 {
		t.Fatalf("after first Tick: staleness = %v, want 0 (first-sighting seed)", got)
	}
	if o.Stats().VWAPWrites == 0 {
		t.Fatalf("precondition: the live quote %s never published — the test would prove nothing", live)
	}

	// Ten minutes on. The live quote publishes again; the dead quote
	// has produced nothing since it was first seen.
	clk.now = clk.now.Add(10 * time.Minute)
	store.perPair[live.String()] = liveTrades(live, clk.now)
	writesBefore := o.Stats().VWAPWrites
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if o.Stats().VWAPWrites == writesBefore {
		t.Fatalf("precondition: the live quote %s did not re-publish on the second Tick", live)
	}

	got := testutil.ToFloat64(obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC"))
	if got != 600 {
		t.Errorf("staleness{asset=crypto:BTC} = %v, want 600 — %s has served nothing for 10 min; "+
			"a fresh %s must not reset the clock the alert judges it by", got, dead, live)
	}
}

// TestTick_AllQuotesLiveReadsFresh is the other half: keying by pair
// must not make a healthy asset read stale. Same harness, both quotes
// publishing on every Tick.
func TestTick_AllQuotesLiveReadsFresh(t *testing.T) {
	eth := mustCrypto(t, "ETH")
	a := mustStalenessPair(t, eth, mustCrypto(t, "USDT"))
	b := mustStalenessPair(t, eth, mustFiat(t, "GBP"))

	clk := &stalenessTestClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	cache, _ := newTestRedis(t)
	store := &mockStore{perPair: map[string][]canonical.Trade{
		a.String(): liveTrades(a, clk.now),
		b.String(): liveTrades(b, clk.now),
	}}
	o := New(store, cache, Config{
		Pairs:   []canonical.Pair{a, b},
		Windows: []time.Duration{5 * time.Minute},
	})
	o.clock = clk.Now

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	clk.now = clk.now.Add(10 * time.Minute)
	store.perPair[a.String()] = liveTrades(a, clk.now)
	store.perPair[b.String()] = liveTrades(b, clk.now)
	obs.PriceStalenessSeconds.WithLabelValues("crypto:ETH").Set(-1)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got := testutil.ToFloat64(obs.PriceStalenessSeconds.WithLabelValues("crypto:ETH")); got != 0 {
		t.Errorf("staleness{asset=crypto:ETH} = %v, want 0 — both quotes published this Tick", got)
	}
}

// TestTick_XLMDualFormIsMergedPerQuote pins how the native ↔ crypto:XLM
// merge composes with the quote dimension. The two forms are
// interchangeable for ONE quote (the API resolves
// `asset=native&quote=fiat:GBP` through either form's GBP key), so the
// merge is "freshest form" WITHIN a quote and "stalest quote" ACROSS
// them. A fresh native/USD says nothing about anybody's GBP.
//
// Driven through Tick, in both cfg.Pairs orders, because the merge this
// replaces was order-dependent once already.
func TestTick_XLMDualFormIsMergedPerQuote(t *testing.T) {
	xlm, native := mustCrypto(t, "XLM"), canonical.NativeAsset()
	usd, gbp := mustFiat(t, "USD"), mustFiat(t, "GBP")
	tickerUSD, nativeUSD := mustStalenessPair(t, xlm, usd), mustStalenessPair(t, native, usd)
	tickerGBP, nativeGBP := mustStalenessPair(t, xlm, gbp), mustStalenessPair(t, native, gbp)
	forward := []canonical.Pair{tickerUSD, nativeUSD, tickerGBP, nativeGBP}
	reversed := []canonical.Pair{nativeGBP, tickerGBP, nativeUSD, tickerUSD}

	for _, tc := range []struct {
		name  string
		pairs []canonical.Pair
		live  []canonical.Pair // pairs that publish on every Tick
		want  float64
	}{
		{"GBP dead on both forms, USD live on both", forward, []canonical.Pair{tickerUSD, nativeUSD}, 600},
		{"GBP dead on both forms, USD live on both (reversed)", reversed, []canonical.Pair{tickerUSD, nativeUSD}, 600},
		{"GBP live on native only, USD live on ticker only", forward, []canonical.Pair{nativeGBP, tickerUSD}, 0},
		{"GBP live on native only, USD live on ticker only (reversed)", reversed, []canonical.Pair{nativeGBP, tickerUSD}, 0},
		{"every quote live on every form", forward, forward, 0},
		{"nothing publishes", forward, nil, 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &stalenessTestClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
			cache, _ := newTestRedis(t)
			store := &mockStore{perPair: map[string][]canonical.Trade{}}
			o := New(store, cache, Config{Pairs: tc.pairs, Windows: []time.Duration{5 * time.Minute}})
			o.clock = clk.Now

			for tick := 0; tick < 2; tick++ {
				for _, p := range tc.live {
					store.perPair[p.String()] = liveTrades(p, clk.now)
				}
				obs.PriceStalenessSeconds.WithLabelValues("native").Set(-1)
				obs.PriceStalenessSeconds.WithLabelValues("crypto:XLM").Set(-1)
				before := o.Stats().VWAPWrites
				if err := o.Tick(context.Background()); err != nil {
					t.Fatalf("Tick %d: %v", tick, err)
				}
				if wrote := o.Stats().VWAPWrites - before; wrote != int64(len(tc.live)) {
					t.Fatalf("precondition: Tick %d published %d pairs, want %d", tick, wrote, len(tc.live))
				}
				if tick == 0 {
					clk.now = clk.now.Add(10 * time.Minute)
				}
			}

			for _, label := range []string{"native", "crypto:XLM"} {
				if got := testutil.ToFloat64(obs.PriceStalenessSeconds.WithLabelValues(label)); got != tc.want {
					t.Errorf("staleness{asset=%s} = %v, want %v", label, got, tc.want)
				}
			}
		})
	}
}

// compositeStalenessHarness is the composite-served shape: BTC/USD
// publishes from direct trades on every Tick; BTC/EUR is a CONFIGURED
// pair with no direct trades at all, reachable only through the chain
// BTC/USD × USD/EUR. Whether the chain publishes is up to the caller
// (the FX store and the route-confidence floor).
//
// BTC, not XLM, so the native ↔ crypto:XLM merge is not in play.
type compositeStalenessHarness struct {
	o              *Orchestrator
	clk            *stalenessTestClock
	store          *mockStore
	direct, target canonical.Pair
}

func newCompositeStalenessHarness(t *testing.T, fx FXStore, minRouteConfidence float64) *compositeStalenessHarness {
	t.Helper()
	btc, usd, eur := mustCrypto(t, "BTC"), mustFiat(t, "USD"), mustFiat(t, "EUR")
	direct := mustStalenessPair(t, btc, usd)
	target := mustStalenessPair(t, btc, eur)
	fxLeg := mustStalenessPair(t, usd, eur)

	clk := &stalenessTestClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	cache, _ := newTestRedis(t)
	store := &mockStore{perPair: map[string][]canonical.Trade{}}
	o := New(store, cache, Config{
		Pairs:              []canonical.Pair{direct, target},
		Windows:            []time.Duration{5 * time.Minute},
		Triangulations:     []TriangulationChain{{Target: target, Legs: []canonical.Pair{direct, fxLeg}}},
		FXStore:            fx,
		MinRouteConfidence: minRouteConfidence,
	})
	o.clock = clk.Now
	return &compositeStalenessHarness{o: o, clk: clk, store: store, direct: direct, target: target}
}

// tick publishes the direct pair, runs one real Tick, and returns how
// many times the chain ended in `outcome` during it.
func (h *compositeStalenessHarness) tick(t *testing.T, outcome string) float64 {
	t.Helper()
	h.store.perPair[h.direct.String()] = liveTrades(h.direct, h.clk.now)
	before := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcome))
	writes := h.o.Stats().VWAPWrites
	if err := h.o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if h.o.Stats().VWAPWrites == writes {
		t.Fatalf("precondition: the direct leg %s did not publish — the chain has nothing to route through", h.direct)
	}
	return testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcome)) - before
}

// TestTick_CompositeServedPairReadsFresh — the served VWAP key has TWO
// writers, and a pair-level clock stamped from only one of them is
// wrong for every pair the other one serves. BTC/EUR here has no direct
// trades; publishComposite writes its VWAP key on every Tick. With the
// stamp taken from refreshPairWindow alone, BTC/EUR read 600 s stale
// while publishing every tick, and because the gauge is the STALEST
// quote, it dragged crypto:BTC to 600 with it — a permanent false page
// for a healthy asset (F067, second writer).
func TestTick_CompositeServedPairReadsFresh(t *testing.T) {
	fx := &fakeFXStore{quote: new(big.Rat).SetFrac(big.NewInt(90), big.NewInt(100)), source: "exchangeratesapi"}
	h := newCompositeStalenessHarness(t, fx, 0)

	if ok := h.tick(t, "ok"); ok != 1 {
		t.Fatalf("precondition: first Tick published the composite %v times, want 1", ok)
	}
	h.clk.now = h.clk.now.Add(10 * time.Minute)
	obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC").Set(-1)
	if ok := h.tick(t, "ok"); ok != 1 {
		t.Fatalf("precondition: second Tick published the composite %v times, want 1", ok)
	}

	if got := testutil.ToFloat64(obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC")); got != 0 {
		t.Errorf("staleness{asset=crypto:BTC} = %v, want 0 — %s was published through its chain on this "+
			"Tick; a pair served by the composite writer is not a dead feed", got, h.target)
	}
	// The stamp is the Tick's injected clock, not a wall-clock read
	// taken inside the triangulation pass.
	if got := h.o.lastWriteAt[h.target.String()]; !got.Equal(h.clk.now) {
		t.Errorf("lastWriteAt[%s] = %v, want the Tick clock %v", h.target, got, h.clk.now)
	}
}

// TestTick_CompositeThatDoesNotPublishStillClimbs is the converse, and
// the reason the stamp sits AFTER the value write on the "ok" path
// only: a chain that resolves nothing, or resolves a composite it
// refuses to publish, has written no VWAP key, so the gauge must keep
// climbing for a target with no direct trades.
func TestTick_CompositeThatDoesNotPublishStillClimbs(t *testing.T) {
	liveFX := &fakeFXStore{quote: new(big.Rat).SetFrac(big.NewInt(90), big.NewInt(100)), source: "exchangeratesapi"}
	for _, tc := range []struct {
		name    string
		fx      FXStore
		minConf float64
		outcome string
		why     string
	}{
		// No FX row and no cached USD/EUR VWAP: the leg is dry.
		{"dry FX leg", &fakeFXStore{}, 0, "missing_leg", "the chain's FX leg is dry"},
		// A floor no route can clear: the composite resolves but is
		// refused as low-confidence and never written to the VWAP key.
		{"low confidence", liveFX, 1.5, "low_confidence", "the composite was refused as low-confidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCompositeStalenessHarness(t, tc.fx, tc.minConf)

			if n := h.tick(t, tc.outcome); n != 1 {
				t.Fatalf("precondition: first Tick ended in %s %v times, want 1", tc.outcome, n)
			}
			h.clk.now = h.clk.now.Add(10 * time.Minute)
			obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC").Set(-1)
			okBefore := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
			if n := h.tick(t, tc.outcome); n != 1 {
				t.Fatalf("precondition: second Tick ended in %s %v times, want 1", tc.outcome, n)
			}
			if d := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok")) - okBefore; d != 0 {
				t.Fatalf("precondition: the composite published (%v) — this case must not publish", d)
			}

			if got := testutil.ToFloat64(obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC")); got != 600 {
				t.Errorf("staleness{asset=crypto:BTC} = %v, want 600 — %s and %s has no direct trades, "+
					"so nothing wrote its VWAP key for 10 min", got, tc.why, h.target)
			}
		})
	}
}

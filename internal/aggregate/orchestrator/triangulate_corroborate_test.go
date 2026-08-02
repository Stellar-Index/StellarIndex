package orchestrator

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ratOf parses a decimal string into *big.Rat for test fixtures.
func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("parse rat %q", s)
	}
	return r
}

// makeTradeOn is buildTradeFrom for an arbitrary pair — the
// corroboration tests need trades on the TARGET of a chain, not on the
// package's fixed XLM/USDT fixture pair.
func makeTradeOn(t *testing.T, pair canonical.Pair, source string, base, quote int64, ts time.Time) canonical.Trade {
	t.Helper()
	return canonical.Trade{
		Source:      source,
		Ledger:      0,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

// TestTriangulationDivergencePct_UncheckedWithoutAComposite — the
// default state of the entire index: no chain configured, so the
// confidence step must report "unchecked" rather than a divergence of
// zero (which reads as perfect agreement).
func TestTriangulationDivergencePct_UncheckedWithoutAComposite(t *testing.T) {
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	o := New(nil, nil, Config{Windows: []time.Duration{5 * time.Minute}})

	if pct, ok := o.triangulationDivergencePct(xlmEUR, 5*time.Minute, ratOf(t, "0.072")); ok {
		t.Errorf("checked=true with no composite recorded (pct=%v)", pct)
	}
}

// TestTriangulationDivergencePct_MeasuresDirectAgainstComposite pins
// the number the confidence factor consumes: deviation measured
// AGAINST the composite (the route through the deep markets), matching
// divergence.Compare's orientation for the cross-oracle factor.
func TestTriangulationDivergencePct_MeasuresDirectAgainstComposite(t *testing.T) {
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute
	o := New(nil, nil, Config{Windows: []time.Duration{window}})

	o.recordComposite(xlmEUR, window, ratOf(t, "0.0720"), 1, 0.8, false)

	// Direct 0.0756 vs composite 0.0720 → |0.0756-0.0720|/0.0720 = 5%.
	pct, ok := o.triangulationDivergencePct(xlmEUR, window, ratOf(t, "0.0756"))
	if !ok {
		t.Fatal("checked=false with a fresh composite recorded")
	}
	if math.Abs(pct-5.0) > 1e-9 {
		t.Errorf("divergence = %v%%, want 5%% (direct 0.0756 vs composite 0.0720)", pct)
	}

	// Symmetric in direction: a direct price BELOW the composite by the
	// same fraction reports the same magnitude.
	below, ok := o.triangulationDivergencePct(xlmEUR, window, ratOf(t, "0.0684"))
	if !ok || math.Abs(below-5.0) > 1e-9 {
		t.Errorf("divergence below composite = %v%% (checked=%v), want 5%%", below, ok)
	}
}

// TestTriangulationDivergencePct_StaleCompositeIsUnchecked — a chain
// that has STOPPED publishing must degrade to "unchecked", never to
// "disagrees". Otherwise a frozen or broken chain would drag a healthy
// pair's confidence down as the direct price walked away from a
// composite that stopped updating.
func TestTriangulationDivergencePct_StaleCompositeIsUnchecked(t *testing.T) {
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute
	interval := 30 * time.Second
	o := New(nil, nil, Config{Windows: []time.Duration{window}, Interval: interval})

	o.recordComposite(xlmEUR, window, ratOf(t, "0.0720"), 1, 0.8, false)

	// One tick old: still trusted.
	o.lastComposites[compositeKey(xlmEUR, window)] = compositeSample{
		price: ratOf(t, "0.0720"),
		at:    time.Now().UTC().Add(-interval),
	}
	if _, ok := o.triangulationDivergencePct(xlmEUR, window, ratOf(t, "0.0756")); !ok {
		t.Error("a one-tick-old composite was rejected; the chain pass is one tick behind by construction")
	}

	// Past compositeMaxAgeTicks: unchecked.
	o.lastComposites[compositeKey(xlmEUR, window)] = compositeSample{
		price: ratOf(t, "0.0720"),
		at:    time.Now().UTC().Add(-(compositeMaxAgeTicks + 1) * interval),
	}
	if pct, ok := o.triangulationDivergencePct(xlmEUR, window, ratOf(t, "0.0756")); ok {
		t.Errorf("a stale composite was still trusted (pct=%v) — staleness must read as unchecked", pct)
	}
}

// TestTick_CompositeRecordedOnlyOnPublish — the sample that feeds the
// confidence factor is written by the chain pass and ONLY on a
// successful publish. A chain that could not publish (missing leg here;
// a frozen leg takes the same path via outcomeFrozenLeg) must leave no
// sample behind for the confidence step to read as evidence.
func TestTick_CompositeRecordedOnlyOnPublish(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000")
	mr.Set(cachekeys.VWAP(usdEUR.Base, usdEUR.Quote, window).String(), "0.900000000000")

	o := New(nil, cache, Config{
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
		},
	})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	sample, ok := o.lastComposites[compositeKey(xlmEUR, window)]
	if !ok {
		t.Fatal("no composite recorded after a successful chain publish")
	}
	// 0.08 × 0.90 = 0.072 — the same value the chain wrote to cache.
	if sample.price.Cmp(ratOf(t, "0.072")) != 0 {
		t.Errorf("recorded composite = %v, want 0.072", sample.price.FloatString(6))
	}

	// Now break a leg: the next tick must not refresh the sample.
	mr.Del(cachekeys.VWAP(usdEUR.Base, usdEUR.Quote, window).String())
	before := o.lastComposites[compositeKey(xlmEUR, window)].at
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got := o.lastComposites[compositeKey(xlmEUR, window)].at; !got.Equal(before) {
		t.Error("a missing-leg chain refreshed the composite sample — an unpublished " +
			"chain must not present itself as this tick's corroboration")
	}
}

// TestTick_CompositeCorroborationReachesTheCachedConfidence is the
// end-to-end wiring proof: a configured chain publishes a composite,
// and the NEXT tick's confidence score for that pair carries the
// composite comparison — checked, and lower when the two disagree.
//
// It also pins the invariant that makes this safe to ship: the
// source-count factor is identical in both runs. A composite is
// corroboration, never a second source, so it must not move the leg
// ADR-0019's 3-signal freeze AND reads (`source_count <= 1`).
func TestTick_CompositeCorroborationReachesTheCachedConfidence(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := time.Minute
	now := time.Now().UTC()

	// Direct VWAP for XLM/EUR from the trade fixture below: two sources,
	// quote/base = 1.242 and 1.245 → volume-weighted ≈ 1.2435.
	run := func(t *testing.T, legUSDEUR string) confidence.Score {
		t.Helper()
		store := &mockStore{
			trades: []canonical.Trade{
				makeTradeOn(t, xlmEUR, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
				makeTradeOn(t, xlmEUR, "phoenix", 1_000_000, 1_245_000, now.Add(-20*time.Second)),
			},
		}
		cache, mr := newTestRedis(t)
		mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "1.000000000000")
		mr.Set(cachekeys.VWAP(usdEUR.Base, usdEUR.Quote, window).String(), legUSDEUR)

		o := New(store, cache, Config{
			Pairs:    []canonical.Pair{xlmEUR},
			Windows:  []time.Duration{window},
			Interval: time.Hour, // long enough that no sample ages out mid-test
			Triangulations: []TriangulationChain{
				{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
			},
			Baselines: stubBaselineSource{
				multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0.0001, MAD: 0.001, N: 100_000}},
				computedAt: now,
			},
		})
		// Tick 1 warms prevVWAP and publishes the first composite;
		// tick 2 scores the direct price against it.
		for i := 0; i < 2; i++ {
			if err := o.Tick(context.Background()); err != nil {
				t.Fatalf("tick %d: %v", i+1, err)
			}
		}
		body, err := cache.Get(context.Background(),
			cachekeys.Confidence(xlmEUR.Base, xlmEUR.Quote, window).String()).Bytes()
		if err != nil {
			t.Fatalf("confidence key missing: %v", err)
		}
		var score confidence.Score
		if err := json.Unmarshal(body, &score); err != nil {
			t.Fatalf("confidence not valid JSON: %v", err)
		}
		return score
	}

	// Composite = 1.0 × 1.2435 = the direct price → agreement.
	agree := run(t, "1.243500000000")
	// Composite = 1.0 × 1.75 → ~29% below the direct price.
	disagree := run(t, "1.750000000000")

	if !agree.Factors.TriangulationChecked || !disagree.Factors.TriangulationChecked {
		t.Fatalf("TriangulationChecked = (%v, %v), want both true — the chain published "+
			"a composite for this pair on the previous tick",
			agree.Factors.TriangulationChecked, disagree.Factors.TriangulationChecked)
	}
	if agree.Factors.TriangulationAgreement != 1.0 {
		t.Errorf("agreeing composite gave factor %v, want 1.0", agree.Factors.TriangulationAgreement)
	}
	if disagree.Factors.TriangulationAgreement >= 0.2 {
		t.Errorf("~29%% disagreement gave factor %v, want well under 0.2 — divergence "+
			"between a direct print and its composite is a manipulation signal",
			disagree.Factors.TriangulationAgreement)
	}
	if disagree.Confidence >= agree.Confidence {
		t.Errorf("disagreement did not lower confidence: %v (disagree) vs %v (agree)",
			disagree.Confidence, agree.Confidence)
	}
	if agree.Factors.SourceCount != disagree.Factors.SourceCount {
		t.Errorf("the composite moved the source-count factor (%v vs %v) — a derived "+
			"path must never count as a source for the freeze's 3-signal AND",
			agree.Factors.SourceCount, disagree.Factors.SourceCount)
	}
}

package orchestrator

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// setLegVWAP pre-writes a leg's VWAP into the test Redis so the router's
// edge builder resolves it from cache (FXStore nil path). Value is
// exact — big.Rat parses it back losslessly.
func setLegVWAP(t *testing.T, cache Cache, leg canonical.Pair, window time.Duration, decimal string) {
	t.Helper()
	key := cachekeys.VWAP(leg.Base, leg.Quote, window)
	if err := cache.Set(context.Background(), key.String(), decimal, window).Err(); err != nil {
		t.Fatalf("seed leg VWAP %s: %v", key, err)
	}
}

func readCompositeMeta(t *testing.T, cache Cache, target canonical.Pair, window time.Duration) (compositeMeta, bool) {
	t.Helper()
	raw, err := cache.Get(context.Background(),
		cachekeys.VWAPCompositeMeta(target.Base, target.Quote, window).String()).Bytes()
	if err != nil {
		return compositeMeta{}, false
	}
	var m compositeMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("composite_meta not valid JSON: %v", err)
	}
	return m, true
}

// TestRouterFreeze_TwoRoutesSuppressSingleSourceFreeze is regression (a):
// a thin, single-DIRECT-source target that WOULD trip the Phase-2
// 3-signal freeze (confidence < 0.45 AND z > 5 AND source_count <= 1) does
// NOT freeze once the graph router has corroborated it with TWO
// independent, agreeing routes on the prior tick — the pathCount widens
// the freeze's source_count leg past 1. The single-route counterfactual
// (pathCount == 1) still freezes, which is what makes the assertion
// non-vacuous: revert effectiveSourceCount and BOTH runs freeze.
func TestRouterFreeze_TwoRoutesSuppressSingleSourceFreeze(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	now := time.Now().UTC()

	// route1 XLM→USD→GBP = 0.10 × 0.80 = 0.08; route2 XLM→EUR→GBP =
	// 0.10 × 0.80 = 0.08 — the two routes AGREE, so both survive and
	// pathCount == 2.
	chainUSD := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}
	chainEUR := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmEUR, eurGBP}}

	// run drives two ticks and reports whether the freeze engaged on the
	// second (single-source, z≈50) bucket. tick 1 warms prevVWAP + records
	// the composite corroboration; tick 2 is the one that would freeze.
	run := func(t *testing.T, chains []TriangulationChain) (froze bool, pathCount int) {
		t.Helper()
		store := &mockStore{}
		cache, _ := newTestRedis(t)
		setLegVWAP(t, cache, xlmUSD, window, "0.100000000000")
		setLegVWAP(t, cache, usdGBP, window, "0.800000000000")
		setLegVWAP(t, cache, xlmEUR, window, "0.100000000000")
		setLegVWAP(t, cache, eurGBP, window, "0.800000000000")

		marker := &recordingFreezeMarker{}
		o := New(store, cache, Config{
			Pairs:          []canonical.Pair{xlmGBP},
			Windows:        []time.Duration{window},
			Interval:       time.Hour, // no sample ages out mid-test
			Triangulations: chains,
			FreezeWriter:   marker,
			// Anomaly nil isolates the Phase-2 lifecycle; Baselines gives it
			// the z-score it needs to fire.
			Baselines: stubBaselineSource{
				multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 100_000}},
				computedAt: now,
			},
		})

		// Tick 1: direct XLM/GBP = 0.08 (single source), composite recorded.
		store.trades = []canonical.Trade{
			makeTradeOn(t, xlmGBP, "soroswap", 100_000_000, 8_000_000, now.Add(-30*time.Second)),
		}
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("tick 1: %v", err)
		}
		sample := o.lastComposites[compositeKey(xlmGBP, window)]

		// Tick 2: direct jumps to 0.12 — a +50% return, z ≈ 50 against the
		// MAD=0.01 baseline, single source. This is the bucket the freeze
		// evaluates.
		store.trades = []canonical.Trade{
			makeTradeOn(t, xlmGBP, "soroswap", 100_000_000, 12_000_000, now.Add(-10*time.Second)),
		}
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("tick 2: %v", err)
		}
		return len(marker.marks) > 0, sample.pathCount
	}

	// Two agreeing routes → corroborated → freeze suppressed.
	froze2, pc2 := run(t, []TriangulationChain{chainUSD, chainEUR})
	if pc2 != 2 {
		t.Fatalf("two-route target recorded pathCount=%d, want 2 (the router did not "+
			"find both corroborating routes — the rest of the test is meaningless)", pc2)
	}
	if froze2 {
		t.Error("freeze ENGAGED on a target corroborated by 2 agreeing routes — pathCount=2 " +
			"must widen the freeze's source_count leg past 1 and suppress the single-source freeze")
	}

	// Single route → pathCount 1 → still single-source → freeze fires.
	froze1, pc1 := run(t, []TriangulationChain{chainUSD})
	if pc1 != 1 {
		t.Fatalf("single-route target recorded pathCount=%d, want 1", pc1)
	}
	if !froze1 {
		t.Error("single-route (pathCount=1) target did NOT freeze on a z≈50 single-source " +
			"bucket — the counterfactual is broken, so the two-route pass proves nothing")
	}
}

// TestRouterTarget_DustEdgeIsLowConfidenceNotPublished is regression (b):
// a target reachable ONLY through a low-confidence (thin, single-source)
// edge is flagged low_confidence and does NOT overwrite the served price
// with the dust-derived cross. A dust edge can't launder itself into a
// confident valuation through a hub (INV-11).
func TestRouterTarget_DustEdgeIsLowConfidenceNotPublished(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	now := time.Now().UTC()

	// XLM/USD is a priced pair with a SINGLE source, so its edge
	// confidence is SourceCountFactor(1) ≈ 0.119 — below the 0.5 floor.
	store := &mockStore{
		trades: []canonical.Trade{
			makeTradeOn(t, xlmUSD, "soroswap", 100_000_000, 10_000_000, now.Add(-20*time.Second)),
		},
	}
	cache, mr := newTestRedis(t)
	setLegVWAP(t, cache, usdGBP, window, "0.800000000000") // FX leg, authoritative

	o := New(store, cache, Config{
		Pairs:              []canonical.Pair{xlmUSD},
		Windows:            []time.Duration{window},
		Interval:           time.Hour,
		MinRouteConfidence: 0.5, // the dust XLM/USD edge (≈0.119) is below this
		Triangulations: []TriangulationChain{
			{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}},
		},
	})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The composite must NOT have been published over the target's VWAP
	// key — a dust-only cross is not a confident price.
	if mr.Exists(cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String()) {
		v, _ := mr.Get(cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String())
		t.Errorf("dust-only target published a VWAP (%q) — a route whose weakest edge is "+
			"below min_route_confidence must NOT set a confident price", v)
	}

	// The low_confidence flag is carried for Step 3.
	meta, ok := readCompositeMeta(t, cache, xlmGBP, window)
	if !ok {
		t.Fatal("no composite_meta marker written for the low-confidence target")
	}
	if !meta.LowConfidence {
		t.Errorf("composite_meta.low_confidence = false, want true (only dust routes reached the target)")
	}

	// And it is NOT recorded as corroboration — a dust cross must not
	// suppress a downstream freeze.
	if _, corroborated := o.routeCorroborationCount(xlmGBP, window); corroborated {
		t.Error("a low-confidence composite was recorded as corroboration — a dust route " +
			"must never widen the freeze's source_count leg")
	}
}

// TestRouterTarget_SingleRouteUnchanged is regression (c): a target with
// exactly one route behaves like the pre-router static single-chain
// multiply — the served composite is the EXACT rational product, stamped
// triangulated, backed by pathCount == 1.
func TestRouterTarget_SingleRouteUnchanged(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute

	cache, mr := newTestRedis(t)
	setLegVWAP(t, cache, xlmUSD, window, "0.100000000000")
	setLegVWAP(t, cache, usdGBP, window, "0.800000000000")

	o := New(nil, cache, Config{
		// No Pairs — the target is priced solely by the router.
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}},
		},
	})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Exact rational product: 0.10 × 0.80 = 0.08 = 2/25 (ADR-0003).
	got, err := mr.Get(cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String())
	if err != nil {
		t.Fatalf("target VWAP missing: %v", err)
	}
	if got != "0.080000000000" {
		t.Errorf("single-route composite = %q, want 0.080000000000 (exact 0.10 × 0.80)", got)
	}
	if sample := o.lastComposites[compositeKey(xlmGBP, window)]; sample.price == nil ||
		sample.price.Cmp(big.NewRat(2, 25)) != 0 {
		t.Errorf("recorded composite = %v, want exact 2/25", sample.price)
	}

	// Provenance still stamped triangulated.
	prov, err := mr.Get(cachekeys.VWAPProvenance(xlmGBP.Base, xlmGBP.Quote, window).String())
	if err != nil || prov != cachekeys.VWAPProvenanceTriangulated {
		t.Errorf("provenance = %q (err %v), want %q", prov, err, cachekeys.VWAPProvenanceTriangulated)
	}

	// pathCount == 1 → a single route can never widen the freeze's
	// source_count leg (max against the direct count).
	if pc, ok := o.routeCorroborationCount(xlmGBP, window); !ok || pc != 1 {
		t.Errorf("routeCorroborationCount = (%d, %v), want (1, true)", pc, ok)
	}
	meta, ok := readCompositeMeta(t, cache, xlmGBP, window)
	if !ok || meta.LowConfidence || meta.PathCount != 1 {
		t.Errorf("composite_meta = %+v (ok=%v), want {PathCount:1, LowConfidence:false}", meta, ok)
	}
}

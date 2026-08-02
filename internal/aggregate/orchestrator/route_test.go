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
	// 0.10 × 0.80 = 0.08 — the two routes AGREE tightly AND are edge-
	// disjoint (distinct USD vs EUR hubs), so corroborationCount == 2.
	chainUSD := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}
	chainEUR := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmEUR, eurGBP}}

	// run drives two ticks and reports whether the freeze engaged on the
	// second (single-source, z≈50) bucket. tick 1 warms prevVWAP + records
	// the composite corroboration; tick 2 is the one that would freeze.
	run := func(t *testing.T, chains []TriangulationChain) (froze bool, corroboration int) {
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
		return len(marker.marks) > 0, sample.corroborationCount
	}

	// Two agreeing, edge-disjoint routes → corroborated → freeze suppressed.
	froze2, corr2 := run(t, []TriangulationChain{chainUSD, chainEUR})
	if corr2 != 2 {
		t.Fatalf("two-route target recorded corroborationCount=%d, want 2 (the router did not "+
			"find both independent corroborating routes — the rest of the test is meaningless)", corr2)
	}
	if froze2 {
		t.Error("freeze ENGAGED on a target corroborated by 2 agreeing edge-disjoint routes — " +
			"corroborationCount=2 must widen the freeze's source_count leg past 1 and suppress " +
			"the single-source freeze")
	}

	// Single route → corroborationCount 1 → still single-source → freeze fires.
	froze1, corr1 := run(t, []TriangulationChain{chainUSD})
	if corr1 != 1 {
		t.Fatalf("single-route target recorded corroborationCount=%d, want 1", corr1)
	}
	if !froze1 {
		t.Error("single-route (corroborationCount=1) target did NOT freeze on a z≈50 single-source " +
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

// runFreezeWithChains drives the two-tick single-source-z≈50 freeze
// scenario used by the corroboration-integrity tests: tick 1 warms the
// prev VWAP + records the composite corroboration, tick 2 is the
// single-source spike bucket that freezes UNLESS the recorded
// corroboration widened its source_count leg past 1. It returns whether a
// freeze marker was written and the corroboration count the chain pass
// recorded on tick 1. legVWAPs seeds the leg cache values that shape the
// routes.
func runFreezeWithChains(
	t *testing.T,
	target canonical.Pair,
	chains []TriangulationChain,
	legVWAPs map[canonical.Pair]string,
) (froze bool, corroboration int) {
	t.Helper()
	window := time.Minute
	now := time.Now().UTC()

	store := &mockStore{}
	cache, _ := newTestRedis(t)
	for leg, v := range legVWAPs {
		setLegVWAP(t, cache, leg, window, v)
	}

	marker := &recordingFreezeMarker{}
	o := New(store, cache, Config{
		Pairs:          []canonical.Pair{target},
		Windows:        []time.Duration{window},
		Interval:       time.Hour, // no sample ages out mid-test
		Triangulations: chains,
		FreezeWriter:   marker,
		Baselines: stubBaselineSource{
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 100_000}},
			computedAt: now,
		},
	})

	// Tick 1: direct target = 0.08 (single source), composite recorded.
	store.trades = []canonical.Trade{
		makeTradeOn(t, target, "soroswap", 100_000_000, 8_000_000, now.Add(-30*time.Second)),
	}
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	sample := o.lastComposites[compositeKey(target, window)]

	// Tick 2: direct jumps to 0.12 — z ≈ 50 against MAD=0.01, single source.
	store.trades = []canonical.Trade{
		makeTradeOn(t, target, "soroswap", 100_000_000, 12_000_000, now.Add(-10*time.Second)),
	}
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	return len(marker.marks) > 0, sample.corroborationCount
}

// TestRouterFreeze_LooselyAgreeingRoutesDoNotSuppress is R1: two routes
// that survive the LOOSE 40% outlier band but disagree beyond the tight
// corroboration band must NOT suppress the single-source freeze. Before
// the fix the raw survivor pathCount (2) fed the freeze and wrongly
// suppressed it; the corroboration count is now 0, so the freeze fires.
func TestRouterFreeze_LooselyAgreeingRoutesDoNotSuppress(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")

	// route1 XLM→USD→GBP = 0.10 × 0.80 = 0.08; route2 XLM→EUR→GBP =
	// 0.10 × 0.90 = 0.09 — 12.5% apart: inside the 40% loose band (both
	// survive, pathCount=2, not diverged) but WAY outside the ~3% tight
	// corroboration band.
	chains := []TriangulationChain{
		{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}},
		{Target: xlmGBP, Legs: []canonical.Pair{xlmEUR, eurGBP}},
	}
	froze, corroboration := runFreezeWithChains(t, xlmGBP, chains, map[canonical.Pair]string{
		xlmUSD: "0.100000000000",
		usdGBP: "0.800000000000",
		xlmEUR: "0.100000000000",
		eurGBP: "0.900000000000", // makes route2 = 0.09, disagreeing with route1
	})
	if corroboration != 0 {
		t.Errorf("corroborationCount=%d, want 0 — routes 12.5%% apart do not tightly agree, "+
			"so neither corroborates the other", corroboration)
	}
	if !froze {
		t.Error("freeze did NOT engage on a single-source z≈50 bucket whose two routes only " +
			"LOOSELY agree — loosely-agreeing routes must not suppress the anomaly freeze (R1)")
	}
}

// TestRouterFreeze_SharedBottleneckDoesNotSuppress is R2: two agreeing
// routes that both funnel through one shared edge are NOT independent, so
// they must NOT suppress the freeze. Before the fix pathCount=2 fed the
// freeze; the independent corroboration count is now 1.
func TestRouterFreeze_SharedBottleneckDoesNotSuppress(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmBTC := mkPair(t, "crypto", "XLM", "crypto", "BTC")
	btcEUR := mkPair(t, "crypto", "BTC", "fiat", "EUR")
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")

	// Two 3-hop routes to GBP that CONVERGE on EUR→GBP (the bottleneck):
	//   A: XLM→USD→EUR→GBP = 0.10 × 0.90 × 0.80 = 0.072
	//   B: XLM→BTC→EUR→GBP = 1e-5 × 9000 × 0.80 = 0.072
	// They agree exactly, but share EUR→GBP — one independent path, not two.
	chains := []TriangulationChain{
		{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdEUR, eurGBP}},
		{Target: xlmGBP, Legs: []canonical.Pair{xlmBTC, btcEUR, eurGBP}},
	}
	froze, corroboration := runFreezeWithChains(t, xlmGBP, chains, map[canonical.Pair]string{
		xlmUSD: "0.100000000000",
		usdEUR: "0.900000000000",
		xlmBTC: "0.000010000000",
		btcEUR: "9000.000000000000",
		eurGBP: "0.800000000000",
	})
	if corroboration != 1 {
		t.Errorf("corroborationCount=%d, want 1 — both routes share the EUR→GBP edge, so they "+
			"are one independent confirmation", corroboration)
	}
	if !froze {
		t.Error("freeze did NOT engage on a single-source z≈50 bucket whose two routes share a " +
			"bottleneck edge — a shared-leg set must not suppress the anomaly freeze (R2)")
	}
}

// TestRouterTarget_FXDryRerouteGatedAndFlagged is R3: when a configured
// FX leg dries and the router substitutes a crypto cross-pair path, the
// reroute is (a) confidence-gated on a sane floor — a thin substitute does
// NOT publish over the direct price — and (b) flagged so it is observable,
// never silent.
func TestRouterTarget_FXDryRerouteGatedAndFlagged(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")   // the FX leg that dries
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")   // reroute leg
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")   // reroute leg
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP") // target
	window := time.Minute
	now := time.Now().UTC()

	const directPrice = "0.050000000000" // the direct price that must keep serving

	// buildO wires a target xlmGBP whose configured chain [XLM/USD, USD/GBP]
	// has a DRY USD/GBP leg, plus a reroute path XLM→USD→EUR→GBP supplied by
	// a second chain that adds the USD↔EUR and EUR↔GBP edges. When
	// thinXLMUSD is true the XLM/USD edge is a single-source priced pair
	// (confidence ≈ 0.119, below the reroute floor); otherwise it is a
	// cache leg (confidence 1.0, above the floor).
	buildO := func(t *testing.T, thinXLMUSD bool) (*Orchestrator, Cache) {
		t.Helper()
		store := &mockStore{}
		var pairs []canonical.Pair
		cache, _ := newTestRedis(t)
		// Direct price already serving for the target.
		setLegVWAP(t, cache, xlmGBP, window, directPrice)
		// Reroute legs present.
		setLegVWAP(t, cache, usdEUR, window, "0.900000000000")
		setLegVWAP(t, cache, eurGBP, window, "0.800000000000")
		// USD/GBP deliberately NOT seeded → the FX leg is dry.
		if thinXLMUSD {
			pairs = []canonical.Pair{xlmUSD}
			store.trades = []canonical.Trade{
				makeTradeOn(t, xlmUSD, "soroswap", 100_000_000, 10_000_000, now.Add(-20*time.Second)),
			}
		} else {
			setLegVWAP(t, cache, xlmUSD, window, "0.100000000000")
		}
		o := New(store, cache, Config{
			Pairs:   pairs,
			Windows: []time.Duration{window},
			Triangulations: []TriangulationChain{
				{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}},
				{Target: usdGBP, Legs: []canonical.Pair{usdEUR, eurGBP}},
			},
		})
		return o, cache
	}

	// ── below the floor: thin substitute must NOT publish over direct ──
	t.Run("below_floor_direct_serves", func(t *testing.T) {
		o, cache := buildO(t, true)
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		got, err := cache.Get(context.Background(),
			cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String()).Result()
		if err != nil {
			t.Fatalf("target VWAP read: %v", err)
		}
		if got != directPrice {
			t.Errorf("target VWAP = %q, want the untouched direct price %q — a thin FX-dry "+
				"reroute (conf ≈ 0.119 < %.2f) must NOT publish over the direct price (R3a)",
				got, directPrice, rerouteMinConfidence)
		}
		meta, ok := readCompositeMeta(t, cache, xlmGBP, window)
		if !ok {
			t.Fatal("no composite_meta written for the rerouted target")
		}
		if !meta.Rerouted {
			t.Error("composite_meta.rerouted = false, want true — a leg-substitution must be " +
				"flagged, not silent (R3b)")
		}
		if !meta.LowConfidence {
			t.Error("composite_meta.low_confidence = false, want true — a below-floor reroute " +
				"was not published over direct")
		}
		if _, corroborated := o.routeCorroborationCount(xlmGBP, window); corroborated {
			t.Error("a below-floor reroute was recorded as corroboration — it must not widen the freeze")
		}
	})

	// ── above the floor: substitute publishes, still flagged ──
	t.Run("above_floor_publishes_flagged", func(t *testing.T) {
		o, cache := buildO(t, false)
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		got, err := cache.Get(context.Background(),
			cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String()).Result()
		if err != nil {
			t.Fatalf("target VWAP read: %v", err)
		}
		// Reroute composite 0.10 × 0.90 × 0.80 = 0.072 replaced the direct.
		if got != "0.072000000000" {
			t.Errorf("target VWAP = %q, want the reroute composite 0.072000000000 — a "+
				"confident reroute IS the multi-path robustness we keep (R3)", got)
		}
		meta, ok := readCompositeMeta(t, cache, xlmGBP, window)
		if !ok {
			t.Fatal("no composite_meta written for the rerouted target")
		}
		if !meta.Rerouted {
			t.Error("composite_meta.rerouted = false, want true — even a published reroute is " +
				"flagged so the substitution is observable (R3b)")
		}
		if meta.LowConfidence {
			t.Error("composite_meta.low_confidence = true, want false — the reroute cleared the floor")
		}
	})
}

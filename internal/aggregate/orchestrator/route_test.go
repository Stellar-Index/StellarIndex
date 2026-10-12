package orchestrator

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// TestRouterFreeze_RouteCorroborationDoesNotDisarmSingleSourceFreeze pins the ADR-0019 amendment: a composite is corroboration, not a second venue, so a single-direct-source target tripping the 3-signal freeze still freezes even with two agreeing edge-disjoint routes, and the reason reports sources=1. The count is pinned to 2 so trust floors do not matter.
func TestRouterFreeze_RouteCorroborationDoesNotDisarmSingleSourceFreeze(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	now := time.Now().UTC()

	// route1 XLM→USD→GBP = 0.10 × 0.80 = 0.08; route2 XLM→EUR→GBP =
	// 0.10 × 0.80 = 0.08 — two agreeing, edge-disjoint routes.
	chainUSD := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}
	chainEUR := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmEUR, eurGBP}}

	// run drives three ticks: tick 1 warms prevVWAP and records the corroboration, tick 2 is the single-source z~50 bucket that would freeze, tick 3 repeats it (persistent manipulation) to check the hold is kept and the comparator does not ratchet. A positive corroborate overrides tick 1's count.
	type runResult struct {
		froze         bool     // freeze marker written on tick 2
		reason        string   // first freeze marker's reason
		corroboration int      // corroborationCount the tick-2 freeze saw
		heldOnTick3   bool     // lifecycle still Active after tick 3
		prevAfter     *big.Rat // prevVWAPs comparator after tick 3
		servedAfter   string   // VWAP cache key after tick 3
	}
	run := func(t *testing.T, chains []TriangulationChain, corroborate int) runResult {
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
				multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: maxDay30Returns}},
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
		if corroborate > 0 {
			o.recordComposite(xlmGBP, window, big.NewRat(2, 25), corroborate, 0.9, false)
			sample = o.lastComposites[compositeKey(xlmGBP, window)]
		}

		// Tick 2: direct jumps to 0.12 — a +50% return, z ≈ 50 against the
		// MAD=0.01 baseline, single source. This is the bucket the freeze
		// evaluates.
		store.trades = []canonical.Trade{
			makeTradeOn(t, xlmGBP, "soroswap", 100_000_000, 12_000_000, now.Add(-10*time.Second)),
		}
		nextBucket(o)
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("tick 2: %v", err)
		}
		froze := len(marker.marks) > 0
		var reason string
		if froze {
			reason = marker.marks[0].decision.Reason
		}

		// Tick 3: the manipulation PERSISTS at 0.12 (same single venue).
		store.trades = []canonical.Trade{
			makeTradeOn(t, xlmGBP, "soroswap", 100_000_000, 12_000_000, now.Add(-5*time.Second)),
		}
		nextBucket(o)
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("tick 3: %v", err)
		}
		stateKey := xlmGBP.String() + ":" + window.String()
		served, _ := cache.Get(context.Background(),
			cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String()).Result()
		return runResult{
			froze:         froze,
			reason:        reason,
			corroboration: sample.corroborationCount,
			heldOnTick3:   o.freezeStates[stateKey].Active(),
			prevAfter:     o.prevVWAPs[stateKey],
			servedAfter:   served,
		}
	}

	// Two agreeing, edge-disjoint routes → corroborated → freeze STILL fires.
	two := run(t, []TriangulationChain{chainUSD, chainEUR}, 2)
	if two.corroboration != 2 {
		t.Fatalf("tick-2 freeze saw corroborationCount=%d, want 2 — the rest of the test is meaningless",
			two.corroboration)
	}
	if !two.froze {
		t.Error("freeze did NOT engage on a single-venue z≈50 bucket corroborated by 2 router routes — " +
			"route corroboration must never count as a second source (ADR-0019 amendment 2026-07-25 §2)")
	}
	if !strings.Contains(two.reason, " sources=1") {
		t.Errorf("freeze reason = %q; want sources=1 (the venue count, not the route count)", two.reason)
	}
	// The route count is recorded beside the venue count, never in it.
	if !strings.Contains(two.reason, " route_corroboration=2") {
		t.Errorf("freeze reason = %q; want route_corroboration=2 beside sources=1", two.reason)
	}

	// Control: one hub route (XLM-USD-GBP = 0.08) agreeing with the tick-1 print gives corroborationCount 1, still single-source, so the spike freezes. A USD-FX-derived hub route must never count as a second source (design doc 10); this stays red if the freeze leg is widened.
	one := run(t, []TriangulationChain{chainUSD}, 0)
	if one.corroboration != 1 {
		t.Fatalf("single-route target recorded corroborationCount=%d, want 1", one.corroboration)
	}
	if !one.froze {
		t.Fatal("single agreeing-route (corroborationCount=1) target did NOT freeze on a z≈50 " +
			"single-source bucket — the counterfactual is broken, so the two-route pass proves nothing")
	}

	// Persistent manipulation (tick 3 still 0.12): the hold is kept, the comparator does not ratchet onto the anomalous print (that would collapse z to 0), and the served key stays the LKG 0.08.
	if !one.heldOnTick3 {
		t.Error("tick 3 (manipulation persists): freeze lifecycle is NOT active — the hold was dropped")
	}
	if one.prevAfter == nil || one.prevAfter.Cmp(big.NewRat(2, 25)) != 0 {
		t.Errorf("tick 3: prevVWAPs comparator = %v, want 2/25 (0.08) — it ratcheted onto the "+
			"manipulated print", one.prevAfter)
	}
	if one.servedAfter != "0.080000000000" {
		t.Errorf("tick 3: served VWAP = %q, want %q (last-known-good must keep serving through the hold)",
			one.servedAfter, "0.080000000000")
	}
}

// TestRouterTarget_DustEdgeIsLowConfidenceNotPublished: a target reachable only through a thin single-source edge is flagged low_confidence and does not overwrite the served price, so dust cannot launder into a confident valuation.
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

// TestRouterTarget_SingleRouteUnchanged pins that a target with exactly one route behaves like the static
// single-chain multiply: the served composite is the exact rational product, stamped triangulated, with pathCount ==
// 1.
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

// runFreezeWithChains drives the two-tick single-source z~50 scenario: tick 1 warms prev VWAP and records corroboration, tick 2 freezes unless that corroboration widened source_count past 1. Returns whether a marker was written and tick 1's recorded count; legVWAPs seeds the leg cache values that shape the routes.
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
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: maxDay30Returns}},
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
	nextBucket(o)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	return len(marker.marks) > 0, sample.corroborationCount
}

// TestRouterFreeze_LooselyAgreeingRoutesDoNotSuppress: two routes inside the loose 40% outlier band but outside the tight corroboration band must not suppress the single-source freeze; the corroboration count is 0, not the raw survivor pathCount of 2.
func TestRouterFreeze_LooselyAgreeingRoutesDoNotSuppress(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")

	// route1 XLM->USD->GBP = 0.10 x 0.80 = 0.08; route2 XLM->EUR->GBP = 0.10 x 0.90 = 0.09. They are 12.5% apart:
	// inside the 40% loose band (both survive, pathCount=2, not diverged) but far outside the ~3% tight corroboration
	// band.
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

// TestRouterFreeze_SharedBottleneckDoesNotSuppress pins that two agreeing routes funnelling through one shared edge
// are not independent: pathCount=2 must not suppress the freeze, and the independent corroboration count is 1.
func TestRouterFreeze_SharedBottleneckDoesNotSuppress(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmBTC := mkPair(t, "crypto", "XLM", "crypto", "BTC")
	btcEUR := mkPair(t, "crypto", "BTC", "fiat", "EUR")
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")

	// Two 3-hop routes to GBP converge on EUR->GBP (the bottleneck) and agree exactly at 0.072 (via USD->EUR, and via
	// BTC->EUR), but share that edge: one independent path, not two.
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
	// 0, not 1: the crypto legs here are cache-only, so each route's weakest link sits at the bootstrap cap and
	// cannot corroborate. The shared-edge collapse itself is pinned in the router package
	// (TestRouter_CorroborationSharedBottleneck).
	if corroboration != 0 {
		t.Errorf("corroborationCount=%d, want 0 — cache-only routes sit at the bootstrap cap and "+
			"must not corroborate", corroboration)
	}
	if !froze {
		t.Error("freeze did NOT engage on a single-source z≈50 bucket whose two routes share a " +
			"bottleneck edge — a shared-leg set must not suppress the anomaly freeze (R2)")
	}
}

// TestRouterTarget_FXDryRerouteGatedAndFlagged: when a configured FX leg dries and the router substitutes a crypto cross path, the reroute is confidence-gated (a thin substitute does not publish over the direct price) and flagged, never silent.
func TestRouterTarget_FXDryRerouteGatedAndFlagged(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")   // the FX leg that dries
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")   // reroute leg
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")   // reroute leg
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP") // target
	window := time.Minute
	now := time.Now().UTC()

	const directPrice = "0.050000000000" // the direct price that must keep serving

	// buildO wires target xlmGBP with a DRY USD/GBP leg and a reroute XLM-USD-EUR-GBP from a second chain. With thinXLMUSD the XLM/USD edge is a single-source priced pair (~0.119, below the floor); otherwise a cached non-FX leg (0.5 = cachedLegConfidence, exactly at the floor, so it publishes).
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
				got, directPrice, aggregate.RouteTrustFloor)
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

	// At the bootstrap cap a cache-only substitute must not publish: the cached XLM/USD leg enters at the cap that
	// every unscored edge carries, and the reroute floor is strictly above it, so the direct price keeps serving and
	// the substitution is flagged.
	t.Run("at_bootstrap_cap_direct_serves", func(t *testing.T) {
		o, cache := buildO(t, false)
		nextBucket(o)
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		got, err := cache.Get(context.Background(),
			cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String()).Result()
		if err != nil {
			t.Fatalf("target VWAP read: %v", err)
		}
		if got != directPrice {
			t.Errorf("target VWAP = %q, want the untouched direct price %q — a reroute whose weakest "+
				"leg sits at the bootstrap cap must NOT publish over the direct price", got, directPrice)
		}
		meta, ok := readCompositeMeta(t, cache, xlmGBP, window)
		if !ok {
			t.Fatal("no composite_meta written for the rerouted target")
		}
		if !meta.Rerouted || !meta.LowConfidence {
			t.Errorf("composite_meta rerouted=%v low_confidence=%v, want both true", meta.Rerouted, meta.LowConfidence)
		}
		if meta.CombinedConfidence != cachedLegConfidence {
			t.Errorf("CombinedConfidence = %v, want the cached-leg cap %v — fixture broken",
				meta.CombinedConfidence, cachedLegConfidence)
		}
	})
}

// TestRecordComposite_RetainsMonotonicClock: recordComposite must stamp time.Now() (monotonic), not .UTC(). The staleness checks use time.Since, and stripping the monotonic reading lets a backward NTP/VM step latch a stale freeze-suppressing corroboration count.
func TestRecordComposite_RetainsMonotonicClock(t *testing.T) {
	window := time.Minute
	o := New(nil, nil, Config{Windows: []time.Duration{window}})
	pair := mkPair(t, "crypto", "XLM", "fiat", "GBP")

	o.recordComposite(pair, window, big.NewRat(1, 2), 1, 0.9, false)

	sample, ok := o.lastComposites[compositeKey(pair, window)]
	if !ok {
		t.Fatal("no composite recorded")
	}
	if !strings.Contains(sample.at.String(), " m=") {
		t.Errorf("recordComposite stamped at=%q with NO monotonic reading — a backward wall-clock "+
			"step would then make time.Since read short and latch a stale freeze-suppressing "+
			"corroboration count. Store time.Now(), not time.Now().UTC() (M1).", sample.at.String())
	}
}

// TestRouteTarget_RerouteAboveBootstrapCapPublishes covers the positive side of the reroute floor: a leg-substitution
// reroute whose weakest leg scores strictly above the bootstrap cap still publishes over the direct price, flagged
// Rerouted.
func TestRouteTarget_RerouteAboveBootstrapCapPublishes(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	cache, _ := newTestRedis(t)
	setLegVWAP(t, cache, xlmGBP, window, "0.050000000000")
	o := New(nil, cache, Config{Windows: []time.Duration{window}})
	edges, err := aggregate.BuildEdges([]aggregate.Quote{
		{Pair: xlmEUR, Price: big.NewRat(2, 1), Confidence: aggregate.RouteTrustFloor + 0.01},
		{Pair: eurGBP, Price: big.NewRat(3, 10), Confidence: 1.0},
	})
	if err != nil {
		t.Fatalf("BuildEdges: %v", err)
	}
	chain := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}
	st := chainLegStatus{frozen: true, frozenLeg: xlmUSD}
	if outcome := o.routeTarget(context.Background(), chain, window, edges, st); outcome != "ok" {
		t.Fatalf("outcome = %q, want ok — a reroute above the bootstrap cap publishes", outcome)
	}
	got, err := cache.Get(context.Background(), cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String()).Result()
	if err != nil || got != "0.600000000000" {
		t.Errorf("target VWAP = %q (err %v), want the reroute composite 0.600000000000", got, err)
	}
	meta, ok := readCompositeMeta(t, cache, xlmGBP, window)
	if !ok || !meta.Rerouted || meta.LowConfidence {
		t.Errorf("composite_meta = %+v (ok %v), want rerouted and not low_confidence", meta, ok)
	}
	// The corroboration count is persisted, not only path_count.
	if meta.CorroborationCount != 1 || meta.PathCount != 1 {
		t.Errorf("composite_meta (corroboration_count, path_count) = (%d, %d), want (1, 1)",
			meta.CorroborationCount, meta.PathCount)
	}
}

// TestRouterTarget_CachedNonFXLegLimitsConfidence: a cached non-FX leg enters at cachedLegConfidence (0.5), not the FX max 1.0, so it can be the route's limiting edge instead of a stale crypto leg riding a hub at full confidence.
func TestRouterTarget_CachedNonFXLegLimitsConfidence(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD") // cached NON-FX leg
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")   // FX leg (stays 1.0)
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute

	cache, _ := newTestRedis(t)
	setLegVWAP(t, cache, xlmUSD, window, "0.100000000000")
	setLegVWAP(t, cache, usdGBP, window, "0.800000000000")

	o := New(nil, cache, Config{
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}},
		},
	})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	meta, ok := readCompositeMeta(t, cache, xlmGBP, window)
	if !ok {
		t.Fatal("no composite_meta written for the published target")
	}
	// Route confidence = min(cachedLegConfidence 0.5, FX 1.0) = 0.5.
	if meta.CombinedConfidence != cachedLegConfidence {
		t.Errorf("CombinedConfidence = %v, want %v — a cached NON-FX leg (XLM/USD) must enter at "+
			"the conservative cached-leg confidence so it can limit the route, not at the FX max "+
			"of 1.0 (L1)", meta.CombinedConfidence, cachedLegConfidence)
	}
}

// TestRouteTarget_FrozenLegRerouteRespectsFreeze: (a) a below-floor reroute around a leg frozen this tick must not publish over the direct price and must be flagged Rerouted; (b) a target frozen on its own market must not have its LKG overwritten by a composite.
// Both drive routeTarget directly with a constructed leg status.
func TestRouteTarget_FrozenLegRerouteRespectsFreeze(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	const directPrice = "0.050000000000"

	// ── (a) below-floor frozen-leg reroute: direct serves, flagged ──
	t.Run("below_floor_frozen_leg_reroute", func(t *testing.T) {
		cache, _ := newTestRedis(t)
		setLegVWAP(t, cache, xlmGBP, window, directPrice) // direct already serving
		o := New(nil, cache, Config{Windows: []time.Duration{window}})

		// Substitute route XLM→EUR→GBP; the XLM/EUR leg is thin (conf 0.2), so
		// the route's weakest-link confidence (0.2) is below rerouteMinConfidence.
		edges, err := aggregate.BuildEdges([]aggregate.Quote{
			{Pair: xlmEUR, Price: big.NewRat(2, 1), Confidence: 0.2},
			{Pair: eurGBP, Price: big.NewRat(3, 10), Confidence: 1.0},
		})
		if err != nil {
			t.Fatalf("BuildEdges: %v", err)
		}
		chain := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}
		// A leg of this chain FROZE this tick, but the router still reached the
		// target around it.
		st := chainLegStatus{frozen: true, frozenLeg: xlmUSD}

		outcome := o.routeTarget(context.Background(), chain, window, edges, st)
		if outcome != "low_confidence" {
			t.Errorf("outcome = %q, want low_confidence (a below-floor reroute is refused)", outcome)
		}
		got, err := cache.Get(context.Background(),
			cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String()).Result()
		if err != nil {
			t.Fatalf("target VWAP read: %v", err)
		}
		if got != directPrice {
			t.Errorf("target VWAP = %q, want the untouched direct price %q — a below-floor reroute "+
				"AROUND a frozen leg must NOT publish over the direct price, exactly like a dry-leg "+
				"reroute (H2)", got, directPrice)
		}
		meta, ok := readCompositeMeta(t, cache, xlmGBP, window)
		if !ok {
			t.Fatal("no composite_meta written for the frozen-leg reroute")
		}
		if !meta.Rerouted {
			t.Error("composite_meta.rerouted = false, want true — a frozen-leg reroute is a " +
				"substitution and must be flagged, not silent (H2)")
		}
		if !meta.LowConfidence {
			t.Error("composite_meta.low_confidence = false, want true — the below-floor reroute " +
				"was not published over direct")
		}
		if _, corroborated := o.routeCorroborationCount(xlmGBP, window); corroborated {
			t.Error("a below-floor frozen-leg reroute was recorded as corroboration — it must not " +
				"widen the freeze")
		}
	})

	// ── (b) target frozen on its own market: LKG not overwritten ──
	t.Run("target_frozen_not_overwritten", func(t *testing.T) {
		cache, _ := newTestRedis(t)
		setLegVWAP(t, cache, xlmGBP, window, directPrice)
		o := New(nil, cache, Config{Windows: []time.Duration{window}})
		// The target itself froze this tick on its own direct market.
		o.markFrozenThisTick(xlmGBP, window)

		// A high-confidence route that WOULD publish absent the target freeze.
		edges, err := aggregate.BuildEdges([]aggregate.Quote{
			{Pair: xlmEUR, Price: big.NewRat(2, 1), Confidence: 1.0},
			{Pair: eurGBP, Price: big.NewRat(3, 10), Confidence: 1.0},
		})
		if err != nil {
			t.Fatalf("BuildEdges: %v", err)
		}
		chain := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}

		outcome := o.routeTarget(context.Background(), chain, window, edges, chainLegStatus{})
		if outcome != outcomeFrozenLeg {
			t.Errorf("outcome = %q, want %q (a target frozen on its own market withholds the "+
				"composite)", outcome, outcomeFrozenLeg)
		}
		got, err := cache.Get(context.Background(),
			cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String()).Result()
		if err != nil {
			t.Fatalf("target VWAP read: %v", err)
		}
		if got != directPrice {
			t.Errorf("target VWAP = %q, want the untouched frozen LKG %q — a fresh composite must "+
				"not silently overwrite the value a freeze marker says is frozen (H2)", got, directPrice)
		}
		if _, corroborated := o.routeCorroborationCount(xlmGBP, window); corroborated {
			t.Error("a withheld-for-target-freeze composite was recorded as corroboration")
		}
	})
}

// TestRecordComposite_PublishesRouteCorroborationGauge: the router corroboration count behind the last composite is exported per (pair, window), giving the audit trail a series distinct from path_count and the venue source count.
func TestRecordComposite_PublishesRouteCorroborationGauge(t *testing.T) {
	cache, _ := newTestRedis(t)
	o := New(nil, cache, Config{})
	pair := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	o.recordComposite(pair, window, big.NewRat(72, 1000), 3, 0.8, false)

	got := testutil.ToFloat64(obs.AggregatorRouteCorroborationCount.WithLabelValues(pair.String(), windowLabel(window)))
	if got != 3 {
		t.Errorf("AggregatorRouteCorroborationCount(%s, %s) = %v, want 3", pair.String(), windowLabel(window), got)
	}
}

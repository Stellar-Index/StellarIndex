package orchestrator

import (
	"context"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Composite-reference corroboration (2026-08-29) — the tests that pin
// the product decision AND its safety envelope. Every scenario is the
// production shape: crypto:XLM/fiat:GBP quoted by ONE venue, its chain
// [crypto:XLM/fiat:USD, fiat:USD/fiat:GBP] with XLM/USD refreshed as a
// pair in the SAME tick (multi-venue) and USD/GBP snapped from the FX
// store. Tick 1 warms comparators (direct 0.08 = 0.10 × 0.80); tick 2
// is the bucket under test (direct 0.12, a +50% / z≈50 single-venue
// print); tick 3 repeats it so a hold can be checked.
//
// Deliberately, aggregate.Pairs lists the TARGET before its LEG so the
// refresh-order guarantee (composite_reference.go refreshOrder) is
// exercised on every scenario — without it every reading would be
// "leg_not_refreshed".

type compositeRefScenario struct {
	// legSources are the venues printing XLM/USD each tick (>= 2 makes
	// the leg strong enough by default).
	legSources []string
	// legPriceT2 is XLM/USD on ticks 2+3 (quote units for 100_000_000
	// base): 10_000_000 = 0.10 (flat), 15_000_000 = 0.15 (moved +50%
	// with the venue).
	legPriceT2 int64
	// fxObservedAge is how old the FX snap is at evaluation.
	fxObservedAge time.Duration
	// fxSource is the FX snap's provider label.
	fxSource string
	// targetSources are the venues printing XLM/GBP (1 = the
	// structurally single-venue case).
	targetSources []string
	// enabled toggles the mechanism.
	enabled bool
}

type compositeRefResult struct {
	o           *Orchestrator
	mr          *miniredis.Miniredis
	marker      *recordingFreezeMarker
	froze       bool
	heldOnTick3 bool
	prevAfter   *big.Rat
	servedT2    string
	servedT3    string
	metaT2      compositeMeta
	metaT2OK    bool
	t2Trades    []canonical.Trade
	stateKey    string
	xlmGBP      canonical.Pair
	xlmUSD      canonical.Pair
	usdGBP      canonical.Pair
	window      time.Duration
}

func runCompositeRefScenario(t *testing.T, sc compositeRefScenario) compositeRefResult {
	t.Helper()
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	now := time.Now().UTC()

	store := &mockStore{perPair: map[string][]canonical.Trade{}}
	cache, mr := newTestRedis(t)
	marker := &recordingFreezeMarker{}
	fx := &fakeFXStore{
		quote:      big.NewRat(80, 100), // USD/GBP = 0.80
		observedAt: now.Add(-sc.fxObservedAge),
		source:     sc.fxSource,
	}
	chain := TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}
	var cr CompositeReferenceConfig
	if sc.enabled {
		cr = CompositeReferenceConfig{Enabled: true, Targets: []canonical.Pair{xlmGBP}}
	}
	o := New(store, cache, Config{
		Pairs:          []canonical.Pair{xlmGBP, xlmUSD}, // target BEFORE leg on purpose
		Windows:        []time.Duration{window},
		Interval:       time.Hour, // no sample ages out mid-test
		Triangulations: []TriangulationChain{chain},
		FXStore:        fx,
		FreezeWriter:   marker,
		Baselines: stubBaselineSource{
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 100_000}},
			computedAt: now,
		},
		CompositeReference: cr,
	})

	setTrades := func(legQuote, targetQuote int64, ts time.Time) {
		leg := make([]canonical.Trade, 0, len(sc.legSources))
		for _, src := range sc.legSources {
			leg = append(leg, makeTradeOn(t, xlmUSD, src, 100_000_000, legQuote, ts))
		}
		target := make([]canonical.Trade, 0, len(sc.targetSources))
		for _, src := range sc.targetSources {
			target = append(target, makeTradeOn(t, xlmGBP, src, 100_000_000, targetQuote, ts))
		}
		store.perPair[xlmUSD.String()] = leg
		store.perPair[xlmGBP.String()] = target
	}
	served := func() string {
		v, _ := cache.Get(context.Background(),
			cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String()).Result()
		return v
	}

	// Tick 1: XLM/USD 0.10, XLM/GBP 0.08 — direct == composite.
	setTrades(10_000_000, 8_000_000, now.Add(-30*time.Second))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	// Tick 2: XLM/GBP jumps to 0.12 (+50%, z≈50, single venue).
	setTrades(sc.legPriceT2, 12_000_000, now.Add(-10*time.Second))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	res := compositeRefResult{
		o: o, mr: mr, marker: marker,
		froze:    len(marker.marks) > 0,
		servedT2: served(),
		t2Trades: store.perPair[xlmGBP.String()],
		stateKey: xlmGBP.String() + ":" + window.String(),
		xlmGBP:   xlmGBP, xlmUSD: xlmUSD, usdGBP: usdGBP, window: window,
	}
	res.metaT2, res.metaT2OK = readCompositeMeta(t, cache, xlmGBP, window)

	// Tick 3: the print PERSISTS at 0.12.
	setTrades(sc.legPriceT2, 12_000_000, now.Add(-5*time.Second))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	res.heldOnTick3 = o.freezeStates[res.stateKey].Active()
	res.prevAfter = o.prevVWAPs[res.stateKey]
	res.servedT3 = served()
	return res
}

// TestCompositeReference_VenueSpecificSpikeStillFreezes is the
// manipulation control WITH the mechanism ON: the deep XLM/USD leg
// (2 venues) does not move, the FX leg is fresh — the composite says
// 0.08 while the single venue prints 0.12. The composite REFUTES the
// print: freeze engages on tick 2, persists on tick 3, the comparator
// does not ratchet onto the manipulated print and the served value
// stays the last-known-good. The reason string names the basis and
// keeps `sources=1` (the composite never widens the count).
func TestCompositeReference_VenueSpecificSpikeStillFreezes(t *testing.T) {
	res := runCompositeRefScenario(t, compositeRefScenario{
		legSources:    []string{"kraken", "coinbase"},
		legPriceT2:    10_000_000, // XLM/USD flat at 0.10
		fxObservedAge: time.Hour,
		fxSource:      "massive",
		targetSources: []string{"soroswap"},
		enabled:       true,
	})
	if !res.froze {
		t.Fatal("venue-specific z≈50 spike did NOT freeze with the composite reference ON — " +
			"the reference (composite flat at 0.08 vs print 0.12) must REFUTE it")
	}
	reason := res.marker.marks[0].decision.Reason
	for _, want := range []string{
		"sources=1",
		"corroboration_basis=venue",
		"composite_refuted",
		"composite_leg_sources={crypto:XLM/fiat:USD:2,fiat:USD/fiat:GBP:1}",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("freeze reason %q lacks %q", reason, want)
		}
	}
	if !res.heldOnTick3 {
		t.Error("tick 3 (spike persists): freeze lifecycle is NOT active — the hold was dropped")
	}
	if res.prevAfter == nil || res.prevAfter.Cmp(big.NewRat(2, 25)) != 0 {
		t.Errorf("tick 3: prevVWAPs comparator = %v, want 2/25 (0.08)", res.prevAfter)
	}
	if res.servedT3 != "0.080000000000" {
		t.Errorf("tick 3: served VWAP = %q, want 0.080000000000 (LKG keeps serving)", res.servedT3)
	}
	if !res.metaT2OK || res.metaT2.CorroborationBasis != corroborationBasisVenue {
		t.Errorf("composite_meta.corroboration_basis = %q (ok=%v), want %q",
			res.metaT2.CorroborationBasis, res.metaT2OK, corroborationBasisVenue)
	}
}

// TestCompositeReference_MarketWideMoveDoesNotFreeze is the mirror:
// XLM/USD (2 venues) moves +50% to 0.15 on the same bucket, FX fresh →
// composite 0.12 agrees with the venue's 0.12 within tolerance → the
// move is market-wide → NO freeze on tick 2 or 3, the fresh value is
// served, the comparator advances, composite_meta carries
// corroboration_basis=composite + the leg strengths, the suppressed
// counter and verdict gauge move — and the served independence count
// is STILL 1 (effectiveSourceCount is untouched by the composite).
func TestCompositeReference_MarketWideMoveDoesNotFreeze(t *testing.T) {
	beforeSuppressed := testutil.ToFloat64(obs.AggregatorCompositeFreezeSuppressedTotal)
	res := runCompositeRefScenario(t, compositeRefScenario{
		legSources:    []string{"kraken", "coinbase"},
		legPriceT2:    15_000_000, // XLM/USD moved WITH the venue → composite 0.12
		fxObservedAge: time.Hour,
		fxSource:      "massive",
		targetSources: []string{"soroswap"},
		enabled:       true,
	})
	if res.froze {
		t.Fatalf("freeze ENGAGED on a market-wide move the current-bucket composite reproduces "+
			"(reason %q) — the composite must CORROBORATE it", res.marker.marks[0].decision.Reason)
	}
	if res.heldOnTick3 {
		t.Error("tick 3: a freeze is active on a corroborated move")
	}
	if res.prevAfter == nil || res.prevAfter.Cmp(big.NewRat(3, 25)) != 0 {
		t.Errorf("tick 3: prevVWAPs comparator = %v, want 3/25 (0.12 — the corroborated move published)", res.prevAfter)
	}
	if res.servedT2 != "0.120000000000" {
		t.Errorf("tick 2: served VWAP = %q, want 0.120000000000", res.servedT2)
	}
	if !res.metaT2OK {
		t.Fatal("no composite_meta written on tick 2")
	}
	if res.metaT2.CorroborationBasis != corroborationBasisComposite {
		t.Errorf("composite_meta.corroboration_basis = %q, want %q", res.metaT2.CorroborationBasis, corroborationBasisComposite)
	}
	if got := res.metaT2.CompositeLegSources; got[res.xlmUSD.String()] != 2 || got[res.usdGBP.String()] != 1 {
		t.Errorf("composite_meta.composite_leg_sources = %v, want {%s:2 %s:1}", got, res.xlmUSD, res.usdGBP)
	}
	// Invariant: the composite never raises the independence count.
	if n := res.o.effectiveSourceCount(res.xlmGBP, res.window, res.t2Trades); n != 1 {
		t.Errorf("effectiveSourceCount = %d, want 1 — the composite must never widen source_count", n)
	}
	if got := testutil.ToFloat64(obs.AggregatorCompositeFreezeSuppressedTotal) - beforeSuppressed; got < 1 {
		t.Errorf("composite_freeze_suppressed_total advanced by %v, want >= 1", got)
	}
	if g := testutil.ToFloat64(obs.AggregatorCompositeCorroboration.WithLabelValues(
		res.xlmGBP.String(), res.window.String(), string(compositeVerdictCorroborated))); g != 1 {
		t.Errorf("composite_corroboration{verdict=corroborated} = %v, want 1", g)
	}
	if g := testutil.ToFloat64(obs.AggregatorCompositeReferenceLegSources.WithLabelValues(
		res.xlmGBP.String(), res.window.String(), res.xlmUSD.String())); g != 2 {
		t.Errorf("composite_reference_leg_sources{leg=XLM/USD} = %v, want 2", g)
	}
}

// TestCompositeReference_StaleFXLegCannotCorroborate — the FX snap is
// older than its staleness budget (100h > 76h): even though XLM/USD
// moved with the venue (a genuine move), the reference is UNAVAILABLE
// and the bucket freezes exactly as before, with the reason saying why.
func TestCompositeReference_StaleFXLegCannotCorroborate(t *testing.T) {
	res := runCompositeRefScenario(t, compositeRefScenario{
		legSources:    []string{"kraken", "coinbase"},
		legPriceT2:    15_000_000,
		fxObservedAge: 100 * time.Hour,
		fxSource:      "massive",
		targetSources: []string{"soroswap"},
		enabled:       true,
	})
	assertUnavailableFroze(t, res, "composite_unavailable: fx_stale")
}

// TestCompositeReference_SingleVenueLegCannotCorroborate — XLM/USD
// printed on ONE venue this bucket, below min_leg_sources (2). The
// composite is only as strong as its weakest leg: no corroboration,
// freeze as before, reason `composite_unavailable: leg_sources=1`.
//
// The leg is kept FLAT here: a single-venue XLM/USD that itself jumped
// +50% freezes on its own 3-signal AND and publishes nothing, so the
// target reads "leg_not_refreshed" — equally fail-closed, but a
// different cause. The agreeing-but-thin case (the leg moved with the
// venue on one exchange) is pinned on the evaluator in
// TestCompositeReference_ToleranceBoundary/thin_leg_agreeing.
func TestCompositeReference_SingleVenueLegCannotCorroborate(t *testing.T) {
	res := runCompositeRefScenario(t, compositeRefScenario{
		legSources:    []string{"kraken"},
		legPriceT2:    10_000_000,
		fxObservedAge: time.Hour,
		fxSource:      "massive",
		targetSources: []string{"soroswap"},
		enabled:       true,
	})
	assertUnavailableFroze(t, res, "composite_unavailable: leg_sources=1")
	if !strings.Contains(res.marker.marks[0].decision.Reason, "composite_leg_sources={crypto:XLM/fiat:USD:1}") {
		t.Errorf("reason %q should carry the thin leg's count", res.marker.marks[0].decision.Reason)
	}
}

// TestCompositeReference_OracleFXLegCannotCorroborate — the FX leg
// must come from the FX source class (massive), never an oracle.
func TestCompositeReference_OracleFXLegCannotCorroborate(t *testing.T) {
	res := runCompositeRefScenario(t, compositeRefScenario{
		legSources:    []string{"kraken", "coinbase"},
		legPriceT2:    15_000_000,
		fxObservedAge: time.Hour,
		fxSource:      "reflector-fx",
		targetSources: []string{"soroswap"},
		enabled:       true,
	})
	assertUnavailableFroze(t, res, "composite_unavailable: fx_source_class=reflector-fx")
}

func assertUnavailableFroze(t *testing.T, res compositeRefResult, wantReason string) {
	t.Helper()
	if !res.froze {
		t.Fatalf("bucket did NOT freeze although the composite reference was unavailable (%s) — "+
			"an unavailable reference must leave the freeze semantics exactly as before", wantReason)
	}
	reason := res.marker.marks[0].decision.Reason
	if !strings.Contains(reason, wantReason) || !strings.Contains(reason, "corroboration_basis=venue") ||
		!strings.Contains(reason, "sources=1") {
		t.Errorf("freeze reason %q, want it to contain %q, corroboration_basis=venue and sources=1", reason, wantReason)
	}
	if !res.heldOnTick3 {
		t.Error("tick 3: the hold was dropped")
	}
	if res.servedT3 != "0.080000000000" {
		t.Errorf("tick 3: served VWAP = %q, want the LKG 0.080000000000", res.servedT3)
	}
	if g := testutil.ToFloat64(obs.AggregatorCompositeCorroboration.WithLabelValues(
		res.xlmGBP.String(), res.window.String(), string(compositeVerdictUnavailable))); g != 1 {
		t.Errorf("composite_corroboration{verdict=unavailable} = %v, want 1", g)
	}
}

// TestCompositeReference_MultiVenueTargetByteIdentical — a target with
// >= 2 real venues is never evaluated: every cache key and every
// freeze mark is byte-identical between mechanism ON and OFF, even on
// a bucket the composite WOULD corroborate (XLM/USD moved with it) and
// even though the print is a z≈50 move.
func TestCompositeReference_MultiVenueTargetByteIdentical(t *testing.T) {
	snapshot := func(res compositeRefResult) []string {
		keys := res.mr.Keys()
		sort.Strings(keys)
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			if res.mr.Type(k) != "string" {
				continue // streams carry time-based ids
			}
			v, _ := res.mr.Get(k)
			out = append(out, k+"="+v)
		}
		for _, m := range res.marker.marks {
			out = append(out, "mark:"+m.asset.String()+"/"+m.quote.String()+"="+m.decision.Reason)
		}
		return out
	}
	base := compositeRefScenario{
		legSources:    []string{"kraken", "coinbase"},
		legPriceT2:    15_000_000,
		fxObservedAge: time.Hour,
		fxSource:      "massive",
		targetSources: []string{"kraken", "bitstamp"}, // TWO real venues
	}
	off := base
	off.enabled = false
	on := base
	on.enabled = true
	a := snapshot(runCompositeRefScenario(t, off))
	b := snapshot(runCompositeRefScenario(t, on))
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("multi-venue target outputs differ between mechanism OFF and ON:\nOFF:\n%s\nON:\n%s",
			strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	for _, line := range b {
		if strings.Contains(line, "corroboration_basis") {
			t.Errorf("multi-venue target carries a corroboration_basis (%s) — it must never be evaluated", line)
		}
	}
	if len(a) == 0 {
		t.Fatal("snapshot is empty — the harness published nothing, the comparison is vacuous")
	}
}

// TestCompositeReference_ToleranceBoundary pins the bps arithmetic on
// the evaluator directly: 75 bps inside → corroborated, just outside →
// refuted; the composite never changes, only the verdict.
func TestCompositeReference_ToleranceBoundary(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	now := time.Now().UTC()
	fx := &fakeFXStore{quote: big.NewRat(80, 100), observedAt: now.Add(-time.Hour), source: "massive"}
	o := New(nil, nil, Config{
		Windows:            []time.Duration{window},
		Triangulations:     []TriangulationChain{{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}},
		FXStore:            fx,
		CompositeReference: CompositeReferenceConfig{Enabled: true, Targets: []canonical.Pair{xlmGBP}},
	})
	o.tickLegRefs = map[time.Duration]map[string]legRef{
		window: {xlmUSD.String(): {price: big.NewRat(10, 100), sources: 3}},
	}
	// composite = 0.10 × 0.80 = 0.08. 75 bps above = 0.0806; 76 bps = 0.080608.
	cases := []struct {
		name   string
		direct *big.Rat
		want   compositeVerdict
	}{
		{"exact", big.NewRat(8, 100), compositeVerdictCorroborated},
		{"plus_75bps", big.NewRat(80600, 1_000_000), compositeVerdictCorroborated},
		{"minus_75bps", big.NewRat(79400, 1_000_000), compositeVerdictCorroborated},
		{"plus_76bps", big.NewRat(80608, 1_000_000), compositeVerdictRefuted},
		{"plus_50pct", big.NewRat(12, 100), compositeVerdictRefuted},
	}
	// Only as strong as the weakest leg: the same composite that AGREES
	// exactly corroborates nothing when the XLM/USD leg is single-venue.
	t.Run("thin_leg_agreeing", func(t *testing.T) {
		o.tickLegRefs[window][xlmUSD.String()] = legRef{price: big.NewRat(10, 100), sources: 1}
		defer func() { o.tickLegRefs[window][xlmUSD.String()] = legRef{price: big.NewRat(10, 100), sources: 3} }()
		ref := o.resolveCompositeReference(context.Background(), xlmGBP, window, now, big.NewRat(8, 100))
		if ref.verdict != compositeVerdictUnavailable || ref.unavailable != "leg_sources=1" {
			t.Errorf("verdict %q unavailable=%q, want unavailable / leg_sources=1", ref.verdict, ref.unavailable)
		}
		if ref.price != nil {
			t.Error("an unavailable reference must carry no composite price")
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := o.resolveCompositeReference(context.Background(), xlmGBP, window, now, tc.direct)
			if ref.verdict != tc.want {
				t.Errorf("direct %v: verdict %q (divergence %.4f%%, unavailable=%q), want %q",
					tc.direct, ref.verdict, ref.divergencePct, ref.unavailable, tc.want)
			}
			if ref.price == nil || ref.price.Cmp(big.NewRat(8, 100)) != 0 {
				t.Errorf("composite price = %v, want 2/25", ref.price)
			}
			if ref.legSources[xlmUSD.String()] != 3 || ref.legSources[usdGBP.String()] != 1 {
				t.Errorf("legSources = %v", ref.legSources)
			}
		})
	}
}

// TestCompositeReference_RefreshOrderPutsLegsFirst — the reference is
// evaluated inside the target's refresh, so its priced legs must
// refresh earlier in the same tick whatever order the operator listed
// the pairs in; and with the mechanism off the order is untouched.
func TestCompositeReference_RefreshOrderPutsLegsFirst(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	btcUSD := mkPair(t, "crypto", "BTC", "fiat", "USD")
	cfg := Config{
		Pairs:          []canonical.Pair{xlmGBP, btcUSD, xlmUSD},
		Triangulations: []TriangulationChain{{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}},
	}
	got := refreshOrder(cfg)
	if len(got) != 3 || !got[0].Base.Equal(xlmGBP.Base) || !got[0].Quote.Equal(xlmGBP.Quote) {
		t.Errorf("mechanism off: order changed: %v", got)
	}
	cfg.CompositeReference = CompositeReferenceConfig{Enabled: true, Targets: []canonical.Pair{xlmGBP}}
	got = refreshOrder(cfg)
	want := []canonical.Pair{xlmUSD, xlmGBP, btcUSD}
	for i := range want {
		if !got[i].Base.Equal(want[i].Base) || !got[i].Quote.Equal(want[i].Quote) {
			t.Fatalf("mechanism on: order = %v, want %v", got, want)
		}
	}
}

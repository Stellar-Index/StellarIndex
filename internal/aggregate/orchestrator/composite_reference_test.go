package orchestrator

import (
	"context"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
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
	// legSecondVenueQuoteT2, when non-zero, is the quote every leg venue
	// AFTER the first prints on ticks 2+3 — a venue disagreement on the
	// leg (A1 leg-dispersion guard).
	legSecondVenueQuoteT2 int64
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
		for i, src := range sc.legSources {
			q := legQuote
			if i > 0 && sc.legSecondVenueQuoteT2 != 0 && legQuote != 10_000_000 {
				q = sc.legSecondVenueQuoteT2
			}
			leg = append(leg, makeTradeOn(t, xlmUSD, src, 100_000_000, q, ts))
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

// TestCompositeReference_LegDispersionCannotCorroborate (A1) — XLM/USD
// has two venues on the bucket, but they DISAGREE: Kraken 0.15 and
// Coinbase 0.1545 (+3 %). The leg VWAP sits 150 bps from each venue,
// above leg_dispersion_bps (= tolerance, 75), so two venues do not
// count as two: the reference is UNAVAILABLE, the single-venue spike
// still freezes, and the dispersion is in the reason. The agreeing
// two-venue case is TestCompositeReference_MarketWideMoveDoesNotFreeze.
func TestCompositeReference_LegDispersionCannotCorroborate(t *testing.T) {
	res := runCompositeRefScenario(t, compositeRefScenario{
		legSources:            []string{"kraken", "coinbase"},
		legPriceT2:            15_000_000,
		legSecondVenueQuoteT2: 15_450_000, // +3 % on the second venue
		fxObservedAge:         time.Hour,
		fxSource:              "massive",
		targetSources:         []string{"soroswap"},
		enabled:               true,
	})
	assertUnavailableFroze(t, res, "composite_unavailable: leg_dispersion=")
	reason := res.marker.marks[0].decision.Reason
	if !strings.Contains(reason, "composite_leg_dispersion_bps={crypto:XLM/fiat:USD:1") {
		t.Errorf("reason %q should carry the leg dispersion (~148 bps)", reason)
	}
	if g := testutil.ToFloat64(obs.AggregatorCompositeReferenceLegDispersionBps.WithLabelValues(
		res.xlmGBP.String(), res.window.String(), res.xlmUSD.String())); g < 100 || g > 200 {
		t.Errorf("composite_reference_leg_dispersion_bps{leg=XLM/USD} = %v, want ~148", g)
	}
}

// TestCompositeReference_LegDispersionBoundary pins the guard on the
// evaluator: dispersion at the band corroborates, just above it does
// not, and the leg VWAP / sources are otherwise identical.
func TestCompositeReference_LegDispersionBoundary(t *testing.T) {
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
	for _, tc := range []struct {
		name       string
		dispersion *big.Rat
		want       compositeVerdict
	}{
		{"none", nil, compositeVerdictCorroborated},
		{"at_75bps", big.NewRat(75, 10_000), compositeVerdictCorroborated},
		{"at_76bps", big.NewRat(76, 10_000), compositeVerdictUnavailable},
		{"3pct", big.NewRat(300, 10_000), compositeVerdictUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o.tickLegRefs = map[time.Duration]map[string]legRef{
				window: {xlmUSD.String(): {price: big.NewRat(10, 100), sources: 2, dispersion: tc.dispersion}},
			}
			ref := o.resolveCompositeReference(context.Background(), xlmGBP, window, now, big.NewRat(8, 100))
			if ref.verdict != tc.want {
				t.Errorf("verdict %q (unavailable=%q), want %q", ref.verdict, ref.unavailable, tc.want)
			}
			if tc.want == compositeVerdictUnavailable && !strings.HasPrefix(ref.unavailable, "leg_dispersion=") {
				t.Errorf("unavailable = %q, want leg_dispersion=…", ref.unavailable)
			}
		})
	}
}

// TestLegDispersion_MeasuresWorstVenue pins the statistic itself on the
// survivor slice: equal-size prints at 0.15 and 0.1545 give a leg VWAP
// of 0.15225 and a worst venue deviation of 0.00225/0.15225 ≈ 147.8 bps.
func TestLegDispersion_MeasuresWorstVenue(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	o := New(nil, nil, Config{})
	now := time.Now()
	trades := []canonical.Trade{
		makeTradeOn(t, xlmUSD, "kraken", 100_000_000, 15_000_000, now),
		makeTradeOn(t, xlmUSD, "coinbase", 100_000_000, 15_450_000, now),
	}
	vwap, err := o.computeNormalizedVWAP(trades, xlmUSD)
	if err != nil {
		t.Fatal(err)
	}
	disp, uncomputable := o.legDispersion(xlmUSD, trades, vwap)
	if got := ratBps(disp); got < 147.7 || got > 147.9 || uncomputable {
		t.Errorf("dispersion = %.3f bps (uncomputable=%v), want ≈147.8", got, uncomputable)
	}
	if d, u := o.legDispersion(xlmUSD, trades[:1], vwap); d != nil || u {
		t.Error("single-venue slice must report no dispersion (nil, computable)")
	}
}

// TestCompositeReference_UncomputableDispersionFailsClosed (A4) — a
// venue whose own VWAP cannot be computed (every print zero-base,
// bypassing canonical.Trade.Validate the way aggregate.VWAP's doc says
// callers occasionally do) must not SKIP the guard: the leg is refused
// with `leg_dispersion=uncomputable`, end to end through recordLegRef →
// referenceLeg, and the target's spike still freezes.
func TestCompositeReference_UncomputableDispersionFailsClosed(t *testing.T) {
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
	trades := []canonical.Trade{
		makeTradeOn(t, xlmUSD, "kraken", 100_000_000, 10_000_000, now),
		makeTradeOn(t, xlmUSD, "coinbase", 0, 10_000_000, now), // zero-base: venue VWAP uncomputable
	}
	if d, u := o.legDispersion(xlmUSD, trades, big.NewRat(10, 100)); d != nil || !u {
		t.Fatalf("legDispersion = (%v, uncomputable=%v), want (nil, true)", d, u)
	}
	o.tickLegRefs = make(map[time.Duration]map[string]legRef)
	o.recordLegRef(xlmUSD, window, big.NewRat(10, 100), trades)
	ref := o.resolveCompositeReference(context.Background(), xlmGBP, window, now, big.NewRat(8, 100))
	if ref.verdict != compositeVerdictUnavailable || ref.unavailable != "leg_dispersion=uncomputable" {
		t.Fatalf("verdict %q unavailable=%q, want unavailable / leg_dispersion=uncomputable (the guard must "+
			"fail CLOSED when it cannot run, even though the composite 0.08 agrees exactly)", ref.verdict, ref.unavailable)
	}
	if ref.price != nil {
		t.Error("an unavailable reference must carry no composite price")
	}
}

// TestCompositeReference_ReleaseBandHoldsVenueOffset (A2) — after a
// venue-specific freeze, the venue settles at a HELD +4 % offset from
// the (flat) composite: calm per-tick returns, but the dedicated 2 %
// composite release band refuses it, so the hold is kept past the
// initial 30 min. When the venue genuinely comes back to within 2 %
// the streak earns the auto-release. With the shared 5 % band the
// +4 % offset would have released (red proof).
func TestCompositeReference_ReleaseBandHoldsVenueOffset(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	clock := time.Now().UTC()

	store := &mockStore{perPair: map[string][]canonical.Trade{}}
	cache, _ := newTestRedis(t)
	marker := &recordingFreezeMarker{}
	fx := &fakeFXStore{quote: big.NewRat(80, 100), observedAt: clock.Add(-time.Hour), source: "massive"}
	o := New(store, cache, Config{
		Pairs:          []canonical.Pair{xlmUSD, xlmGBP},
		Windows:        []time.Duration{window},
		Interval:       time.Hour,
		Triangulations: []TriangulationChain{{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}},
		FXStore:        fx,
		FreezeWriter:   marker,
		Baselines: stubBaselineSource{
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 100_000}},
			computedAt: clock,
		},
		CompositeReference: CompositeReferenceConfig{Enabled: true, Targets: []canonical.Pair{xlmGBP}},
	})
	o.clock = func() time.Time { return clock }
	stateKey := xlmGBP.String() + ":" + window.String()
	tick := func(targetQuote int64) {
		t.Helper()
		ts := clock.Add(-10 * time.Second)
		store.perPair[xlmUSD.String()] = []canonical.Trade{
			makeTradeOn(t, xlmUSD, "kraken", 100_000_000, 10_000_000, ts),
			makeTradeOn(t, xlmUSD, "coinbase", 100_000_000, 10_000_000, ts),
		}
		store.perPair[xlmGBP.String()] = []canonical.Trade{
			makeTradeOn(t, xlmGBP, "soroswap", 100_000_000, targetQuote, ts),
		}
		if err := o.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(time.Minute)
	}
	active := func() bool { return o.freezeStates[stateKey].Active() }

	tick(8_000_000)  // warm: 0.08 == composite
	tick(12_000_000) // venue-specific +50 % → refuted → freeze
	if !active() {
		t.Fatal("setup: the venue-specific spike did not freeze")
	}
	clock = clock.Add(31 * time.Minute) // past the 30-min initial hold
	for i := 0; i < 5; i++ {
		tick(8_320_000) // held +4 % vs composite 0.08 — calm, but venue-specific
	}
	if !active() {
		t.Fatal("held +4 % venue-specific offset AUTO-RELEASED — the composite release band (2 %) must refuse it")
	}
	if st := o.freezeStates[stateKey]; st.UnfreezeStreak != 0 {
		t.Errorf("unfreeze streak = %d on a +4 %% offset, want 0", st.UnfreezeStreak)
	}
	for i := 0; i < 4; i++ {
		tick(8_120_000) // genuine move back to +1.5 % — inside the band
	}
	if active() {
		t.Fatal("venue back within 2 % of the composite did NOT auto-release — the positive path is broken, " +
			"so the negative assertion above is vacuous")
	}
}

// ─── the verdict gauges must not outlive the evaluated set ───────────────

// compositeSeriesFor counts the series c holds for one (pair, window),
// independently of whatever other tests in this package left on the
// package-level collector. A registry can hold a collector that another
// registry already holds, so this is a read-only lens on it.
func compositeSeriesFor(t *testing.T, c prometheus.Collector, pair, window string) int {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register collector for inspection: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	n := 0
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var gotPair, gotWindow string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "pair":
					gotPair = lp.GetValue()
				case "window":
					gotWindow = lp.GetValue()
				}
			}
			if gotPair == pair && gotWindow == window {
				n++
			}
		}
	}
	return n
}

// TestCompositeReference_VerdictGaugeRetiredWhenPairLeavesTheEvaluatedSet
// pins the lifetime of the three verdict series.
//
// The composite reference is evaluated ONLY for an allow-listed target
// whose bucket is single-venue (compositeReferenceEligible). The gauges
// it publishes are a PER-TICK verdict — so when the pair gains a second
// venue and stops being evaluated, a lingering `verdict="corroborated"`
// series claims a freeze decision consulted a reference that tick never
// built, and keeps claiming it until the aggregator restarts. Nothing
// deleted these series before this test (grep `Delete` in the package:
// only recordVenueVWAPs had one).
//
// Note the label-value trap: emitCompositeReference labels the window
// with window.String() ("1m0s"), not windowLabel ("1m"). A clear written
// against the other convention deletes nothing and this test fails.
func TestCompositeReference_VerdictGaugeRetiredWhenPairLeavesTheEvaluatedSet(t *testing.T) {
	// A target no other test in this package uses, so the series counted
	// here can only have come from this test.
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdCHF := mkPair(t, "fiat", "USD", "fiat", "CHF")
	xlmCHF := mkPair(t, "crypto", "XLM", "fiat", "CHF")
	window := time.Minute
	now := time.Now().UTC()

	store := &mockStore{perPair: map[string][]canonical.Trade{}}
	cache, _ := newTestRedis(t)
	o := New(store, cache, Config{
		Pairs:          []canonical.Pair{xlmCHF, xlmUSD},
		Windows:        []time.Duration{window},
		Interval:       time.Hour,
		Triangulations: []TriangulationChain{{Target: xlmCHF, Legs: []canonical.Pair{xlmUSD, usdCHF}}},
		FXStore:        &fakeFXStore{quote: big.NewRat(90, 100), observedAt: now.Add(-time.Hour), source: "massive"},
		FreezeWriter:   &recordingFreezeMarker{},
		Baselines: stubBaselineSource{
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 100_000}},
			computedAt: now,
		},
		CompositeReference: CompositeReferenceConfig{Enabled: true, Targets: []canonical.Pair{xlmCHF}},
	})

	setTrades := func(targetSources []string, ts time.Time) {
		store.perPair[xlmUSD.String()] = []canonical.Trade{
			makeTradeOn(t, xlmUSD, "kraken", 100_000_000, 10_000_000, ts),
			makeTradeOn(t, xlmUSD, "coinbase", 100_000_000, 10_000_000, ts),
		}
		target := make([]canonical.Trade, 0, len(targetSources))
		for _, src := range targetSources {
			target = append(target, makeTradeOn(t, xlmCHF, src, 100_000_000, 9_000_000, ts))
		}
		store.perPair[xlmCHF.String()] = target
	}

	// Tick 1: single-venue target → evaluated, verdict published.
	setTrades([]string{"soroswap"}, now.Add(-30*time.Second))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	pairLabel, windowLabelValue := xlmCHF.String(), window.String()
	if n := compositeSeriesFor(t, obs.AggregatorCompositeCorroboration, pairLabel, windowLabelValue); n != len(compositeVerdicts) {
		t.Fatalf("after an evaluated tick: %d verdict series for %s/%s, want %d",
			n, pairLabel, windowLabelValue, len(compositeVerdicts))
	}
	if g := testutil.ToFloat64(obs.AggregatorCompositeCorroboration.WithLabelValues(
		pairLabel, windowLabelValue, string(compositeVerdictCorroborated))); g != 1 {
		t.Fatalf("composite_corroboration{verdict=corroborated} = %v, want 1 "+
			"(the composite 0.10 x 0.90 = 0.09 reproduces the direct print)", g)
	}
	if n := compositeSeriesFor(t, obs.AggregatorCompositeReferenceLegSources, pairLabel, windowLabelValue); n == 0 {
		t.Fatal("after an evaluated tick: no leg-source series published")
	}

	// Tick 2: the target gains a SECOND venue → not evaluated at all.
	setTrades([]string{"soroswap", "aquarius"}, now.Add(-5*time.Second))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if o.compositeReferenceEligible(xlmCHF, store.perPair[xlmCHF.String()]) {
		t.Fatal("a two-venue bucket must not be composite-reference eligible — test setup is wrong")
	}
	if n := compositeSeriesFor(t, obs.AggregatorCompositeCorroboration, pairLabel, windowLabelValue); n != 0 {
		t.Errorf("%d verdict series survive a tick that never evaluated the reference, want 0 — "+
			"a dashboard/alert reading them would report a verdict this bucket's freeze never consulted", n)
	}
	if n := compositeSeriesFor(t, obs.AggregatorCompositeReferenceLegSources, pairLabel, windowLabelValue); n != 0 {
		t.Errorf("%d leg-source series survive an un-evaluated tick, want 0", n)
	}
	if n := compositeSeriesFor(t, obs.AggregatorCompositeReferenceLegDispersionBps, pairLabel, windowLabelValue); n != 0 {
		t.Errorf("%d leg-dispersion series survive an un-evaluated tick, want 0", n)
	}
}

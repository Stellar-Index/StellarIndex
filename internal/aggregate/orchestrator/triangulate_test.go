package orchestrator

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeFXStore is a minimal in-memory FXStore for unit tests. Records
// the queries it was asked to satisfy and returns canned responses.
type fakeFXStore struct {
	// quote, when non-nil, is returned for every FXQuoteAtOrBefore call.
	quote      *big.Rat
	observedAt time.Time
	source     string
	// err, when non-nil, is returned instead.
	err error
	// calls records each call so tests can assert on `cutoff` plumbing.
	calls []fxCall
}

type fxCall struct {
	pair      canonical.Pair
	cutoff    time.Time
	fxSources []string
}

func (f *fakeFXStore) FXQuoteAtOrBefore(_ context.Context, pair canonical.Pair, cutoff time.Time, fxSources []string) (*big.Rat, time.Time, string, error) {
	f.calls = append(f.calls, fxCall{pair: pair, cutoff: cutoff, fxSources: fxSources})
	if f.err != nil {
		return nil, time.Time{}, "", f.err
	}
	if f.quote == nil {
		return nil, time.Time{}, "", timescale.ErrNoFXQuote
	}
	return new(big.Rat).Set(f.quote), f.observedAt, f.source, nil
}

// helper: build canonical.Pair without test boilerplate.
func mkPair(t *testing.T, baseT, baseCode, quoteT, quoteCode string) canonical.Pair {
	t.Helper()
	mk := func(typ, code string) canonical.Asset {
		t.Helper()
		switch typ {
		case "fiat":
			a, err := canonical.ParseAsset("fiat:" + code)
			if err != nil {
				t.Fatalf("ParseAsset fiat:%s: %v", code, err)
			}
			return a
		case "crypto":
			a, err := canonical.NewCryptoAsset(code)
			if err != nil {
				t.Fatalf("NewCryptoAsset %s: %v", code, err)
			}
			return a
		}
		t.Fatalf("unknown asset type %s", typ)
		return canonical.Asset{}
	}
	p, err := canonical.NewPair(mk(baseT, baseCode), mk(quoteT, quoteCode))
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

// TestValidateTriangulationChain_HappyPath — well-formed chain
// passes validation.
func TestValidateTriangulationChain_HappyPath(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")

	chain := TriangulationChain{
		Target: xlmEUR,
		Legs:   []canonical.Pair{xlmUSD, usdEUR},
	}
	if err := ValidateTriangulationChain(chain); err != nil {
		t.Errorf("happy path failed: %v", err)
	}
}

// TestValidateTriangulationChain_BadStructure — naming the
// specific violation lets operators correct config without
// guessing.
func TestValidateTriangulationChain_BadStructure(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	eurGBP := mkPair(t, "fiat", "EUR", "fiat", "GBP")

	tests := []struct {
		name     string
		chain    TriangulationChain
		wantWord string
	}{
		{
			name:     "single-leg chain",
			chain:    TriangulationChain{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD}},
			wantWord: "1 legs",
		},
		{
			name:     "first-leg base mismatch",
			chain:    TriangulationChain{Target: xlmEUR, Legs: []canonical.Pair{usdEUR, xlmUSD}},
			wantWord: "first leg base",
		},
		{
			name:     "last-leg quote mismatch",
			chain:    TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdEUR}},
			wantWord: "last leg quote",
		},
		{
			name:     "fiat/fiat leg without USD",
			chain:    TriangulationChain{Target: xlmGBP, Legs: []canonical.Pair{xlmEUR, eurGBP}},
			wantWord: "without USD",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTriangulationChain(tc.chain)
			if err == nil {
				t.Fatal("expected error; got nil")
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error message missing %q: %v", tc.wantWord, err)
			}
		})
	}
}

// TestIsFXLeg_StructuralPredicate exercises the snap-rule's per-leg
// classification: only fiat-vs-fiat legs (e.g. USD/EUR) qualify.
// Crypto-vs-fiat (XLM/USD) and crypto-vs-crypto (XLM/USDT) stay on
// the cached-VWAP path.
func TestIsFXLeg_StructuralPredicate(t *testing.T) {
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	xlmUSDT := mkPair(t, "crypto", "XLM", "crypto", "USDT")

	if !isFXLeg(usdEUR) {
		t.Error("isFXLeg(USD/EUR) = false; want true (both sides fiat)")
	}
	if isFXLeg(xlmUSD) {
		t.Error("isFXLeg(XLM/USD) = true; want false (crypto base)")
	}
	if isFXLeg(xlmUSDT) {
		t.Error("isFXLeg(XLM/USDT) = true; want false (no fiat side)")
	}
}

// TestTriangulate_FrozenLegDoesNotPublishDerivedPrice — a
// freeze must not be launderable through triangulation.
//
// When Phase 1 or Phase 2 refuses to publish a pair, the orchestrator
// deliberately leaves that pair's last-known-good value in Redis (the
// API serves it with flags.frozen=true, which is the honest answer).
// The triangulation pass then read that very key as if it were this
// tick's fresh price, multiplied it by the other legs, and published
// the product to the TARGET pair — which carries no freeze marker of
// its own. The price we just declined to serve on XLM/USDT reached
// consumers on XLM/EUR one multiplication later, looking fresh.
//
// The chain must refuse (outcome "frozen_leg"), keeps the
// target's own prior value alive, and marks the target frozen so the
// derived pair tells the same truth as the leg it descends from.
//
// Proven red without the guard: the target key is written with "0.900000000000"
// (the frozen 1.00 leg × the 0.90 leg) and Mark is called once (the
// leg only) instead of twice.
func TestTriangulate_FrozenLegDoesNotPublishDerivedPrice(t *testing.T) {
	ctx := context.Background()
	leg1 := xlmUsdtPair(t) // crypto:XLM/crypto:USDT — the pair that freezes
	leg2 := mkPair(t, "crypto", "USDT", "fiat", "EUR")
	target := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	marker := &recordingFreezeMarker{}
	o := New(nil, cache, Config{
		Pairs:        []canonical.Pair{leg1},
		Windows:      []time.Duration{window},
		Anomaly:      newAnomalyChecker(t, leg1),
		FreezeWriter: marker,
		Triangulations: []TriangulationChain{{
			Target: target,
			Legs:   []canonical.Pair{leg1, leg2},
		}},
	})

	// Leg 1 holds its last-known-good $1.00 (what the freeze preserves);
	// leg 2 is a healthy 0.90. Unguarded, the chain publishes 1.00 × 0.90.
	leg1Key := cachekeys.VWAP(leg1.Base, leg1.Quote, window).String()
	leg2Key := cachekeys.VWAP(leg2.Base, leg2.Quote, window).String()
	targetKey := cachekeys.VWAP(target.Base, target.Quote, window).String()
	cache.Set(ctx, leg1Key, "1.000000000000", time.Minute)
	cache.Set(ctx, leg2Key, "0.900000000000", time.Minute)

	// prev = $1.00; this tick's single-source bucket prices XLM at
	// ~$2.10 — a 110% deviation, well past the 2% freeze threshold.
	o.prevVWAPs[leg1.String()+":"+window.String()] = big.NewRat(1, 1)
	o.store = &mockStore{trades: []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), time.Now()),
	}}

	before := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLeg))
	if err := o.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// 1. The derived price must NOT have been published.
	if mr.Exists(targetKey) {
		got, _ := mr.Get(targetKey)
		t.Errorf("target key %q written with %q — the frozen leg's LKG was laundered into a derived price",
			targetKey, got)
	}

	// 2. The target inherits the freeze, so the API flags it.
	var targetMark *recordedMark
	for i := range marker.marks {
		if marker.marks[i].asset.Equal(target.Base) && marker.marks[i].quote.Equal(target.Quote) {
			targetMark = &marker.marks[i]
		}
	}
	if targetMark == nil {
		t.Fatalf("no freeze marker written for the triangulated target %s (marks: %+v)",
			target.String(), marker.marks)
	}
	if !targetMark.decision.IsFrozen() {
		t.Errorf("target decision not frozen: %+v", targetMark.decision)
	}
	if !strings.Contains(targetMark.decision.Reason, "triangulation:leg_frozen") {
		t.Errorf("target freeze reason = %q, want it to name the frozen leg", targetMark.decision.Reason)
	}
	if !strings.Contains(targetMark.decision.Reason, leg1.String()) {
		t.Errorf("target freeze reason = %q, want it to identify leg %s",
			targetMark.decision.Reason, leg1.String())
	}

	// 3. The outcome is attributed, not silently folded into missing_leg.
	after := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLeg))
	if after-before != 1 {
		t.Errorf("triangulation outcome %q delta = %v, want 1", outcomeFrozenLeg, after-before)
	}

	// 4. The leg's own LKG is untouched — the freeze semantics the fix
	// piggybacks on must still hold.
	if got, _ := mr.Get(leg1Key); got != "1.000000000000" {
		t.Errorf("leg LKG = %q, want it preserved at 1.000000000000", got)
	}
}

// TestTriangulate_HealthyLegStillPublishes is the other half of the
// frozen-leg guard: the freeze check must be scoped to the pairs actually
// frozen this tick, not a blanket refusal that black-holes every
// chained pair.
func TestTriangulate_HealthyLegStillPublishes(t *testing.T) {
	ctx := context.Background()
	leg1 := mkPair(t, "crypto", "XLM", "crypto", "USDT")
	leg2 := mkPair(t, "crypto", "USDT", "fiat", "EUR")
	target := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	o := New(nil, cache, Config{
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{{
			Target: target,
			Legs:   []canonical.Pair{leg1, leg2},
		}},
	})
	cache.Set(ctx, cachekeys.VWAP(leg1.Base, leg1.Quote, window).String(), "1.000000000000", time.Minute)
	cache.Set(ctx, cachekeys.VWAP(leg2.Base, leg2.Quote, window).String(), "0.900000000000", time.Minute)

	if err := o.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got, err := mr.Get(cachekeys.VWAP(target.Base, target.Quote, window).String())
	if err != nil {
		t.Fatalf("target key missing — nothing was frozen, the chain must publish: %v", err)
	}
	if got != "0.900000000000" {
		t.Errorf("target = %q, want 0.900000000000 (1.00 × 0.90)", got)
	}
}

// perPairFreezeMarker is a [FreezeMarker] fake keyed by (asset, quote),
// unlike [recordingFreezeMarker] (which models a single global marker
// slot). Needed whenever a test freezes one pair while a DIFFERENT pair
// also exercises the phase-2 rehydrate-from-marker path in the same
// tick — a shared, asset-agnostic fake would leak the first pair's
// "present" state into the second pair's LoadState read.
type perPairFreezeMarker struct {
	marks []recordedMark
	state map[string]freeze.State
}

func perPairFreezeKey(asset, quote canonical.Asset) string {
	return asset.String() + "/" + quote.String()
}

func (m *perPairFreezeMarker) Mark(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error {
	return m.MarkHold(ctx, asset, quote, frozenValue, decision, freeze.State{}, 0)
}

func (m *perPairFreezeMarker) MarkHold(_ context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision, state freeze.State, ttl time.Duration) error {
	m.marks = append(m.marks, recordedMark{
		asset: asset, quote: quote, frozenValue: frozenValue, decision: decision, state: state, ttl: ttl,
	})
	if m.state == nil {
		m.state = make(map[string]freeze.State)
	}
	m.state[perPairFreezeKey(asset, quote)] = state
	return nil
}

func (m *perPairFreezeMarker) LoadState(_ context.Context, asset, quote canonical.Asset) (freeze.State, bool, error) {
	st, ok := m.state[perPairFreezeKey(asset, quote)]
	return st, ok, nil
}

func (m *perPairFreezeMarker) Clear(_ context.Context, asset, quote canonical.Asset) error {
	delete(m.state, perPairFreezeKey(asset, quote))
	return nil
}

// TestTriangulate_FrozenLegTargetPublishedDirectlyIsNotLaundered:
// a leg of the chain freezes and the route around
// it is unreachable (ErrNoRoute), but the TARGET published its own
// fresh direct print this same tick. That value is not a last-known-
// good — inheriting the freeze onto it would Expire a fresh key down to
// FreezeTTL and stamp flags.frozen=true on a price nothing actually
// froze. The fix must serve it unmarked: outcome
// frozen_leg_direct_served, no freeze marker, no second
// AnomalyFreezeEngagedTotal increment.
func TestTriangulate_FrozenLegTargetPublishedDirectlyIsNotLaundered(t *testing.T) {
	ctx := context.Background()
	leg1 := xlmUsdtPair(t)                             // crypto:XLM/crypto:USDT — freezes this tick
	leg2 := mkPair(t, "crypto", "USDT", "fiat", "EUR") // healthy, cached
	target := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	marker := &perPairFreezeMarker{}
	o := New(nil, cache, Config{
		// target is ALSO a directly-priced pair: the per-pair refresh
		// (which runs before triangulateAll) publishes it fresh this tick.
		Pairs:        []canonical.Pair{leg1, target},
		Windows:      []time.Duration{window},
		Anomaly:      newAnomalyChecker(t, leg1),
		FreezeWriter: marker,
		Triangulations: []TriangulationChain{{
			Target: target,
			Legs:   []canonical.Pair{leg1, leg2},
		}},
	})

	leg2Key := cachekeys.VWAP(leg2.Base, leg2.Quote, window).String()
	targetKey := cachekeys.VWAP(target.Base, target.Quote, window).String()
	cache.Set(ctx, leg2Key, "0.900000000000", time.Minute)

	// leg1: prev = $1.00, this tick single-source prints ~$2.10 — well
	// past the 2% freeze threshold, so leg1 freezes and contributes no
	// edge. leg2 is cached and healthy, but with leg1 gone there is no
	// path from XLM to EUR (CombineRoutes returns ErrNoRoute for target).
	o.prevVWAPs[leg1.String()+":"+window.String()] = big.NewRat(1, 1)
	o.store = &mockStore{perPair: map[string][]canonical.Trade{
		leg1.String(): {
			buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), time.Now()),
		},
		// target's own direct market: a plain, undeviated first print —
		// nothing here should ever be treated as anomalous.
		target.String(): {
			buildTrade(t, big.NewInt(100_000_000), big.NewInt(90_000_000), time.Now()),
		},
	}}

	beforeDirectServed := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLegDirectServed))
	beforeFrozenLeg := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLeg))
	beforeEngaged := testutil.ToFloat64(obs.AnomalyFreezeEngagedTotal.WithLabelValues(string(anomaly.ClassStablecoin)))

	if err := o.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// 1. The outcome is the new, distinct one — not folded into
	// frozen_leg (which would mean a freeze was inherited) or missing_leg
	// (which would mean the chains-dry alert fires for a target that is
	// not dry at all).
	afterDirectServed := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLegDirectServed))
	if afterDirectServed-beforeDirectServed != 1 {
		t.Errorf("%s counter delta = %v, want 1", outcomeFrozenLegDirectServed, afterDirectServed-beforeDirectServed)
	}
	afterFrozenLeg := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLeg))
	if afterFrozenLeg-beforeFrozenLeg != 0 {
		t.Errorf("%s counter delta = %v, want 0 (no inheritance)", outcomeFrozenLeg, afterFrozenLeg-beforeFrozenLeg)
	}

	// 2. The target's fresh direct print is actually there, unmarked —
	// nothing laundered it away.
	if !mr.Exists(targetKey) {
		t.Fatalf("target key %q missing — the direct print should have served", targetKey)
	}

	// 3. No freeze marker was written for the target: inheritLegFreeze
	// must not have run. Only leg1's OWN freeze (a different pair) may
	// appear.
	for _, m := range marker.marks {
		if m.asset.Equal(target.Base) && m.quote.Equal(target.Quote) {
			t.Errorf("freeze marker written for target %s (reason %q) — the fresh direct print was wrongly marked frozen",
				target.String(), m.decision.Reason)
		}
	}

	// 4. AnomalyFreezeEngagedTotal increments exactly once, for leg1's
	// own freeze. inheritLegFreeze must not fire a SECOND
	// increment (same class, since target shares leg1's Base asset) for
	// a target that was never frozen.
	afterEngaged := testutil.ToFloat64(obs.AnomalyFreezeEngagedTotal.WithLabelValues(string(anomaly.ClassStablecoin)))
	if afterEngaged-beforeEngaged != 1 {
		t.Errorf("AnomalyFreezeEngagedTotal(%s) delta = %v, want 1 (leg1's own freeze only)",
			anomaly.ClassStablecoin, afterEngaged-beforeEngaged)
	}
}

// An UNSCORABLE edge must never outrank a fully-scored one. The
// unscored fallback is a bare source-count factor (0.731 at 4 sources,
// 0.953 at 6) on a different scale from the multi-factor score, and
// uncapped it cleared the reroute + corroboration gates (both 0.5) that
// a scored edge only ties — so edges with no z-score, no liquidity
// measure and no cross-oracle check were the ones setting composites
// and widening the freeze's source-count leg.
func TestEdgeConfidence_UnscoredFallbackCannotOutrankAScoredEdge(t *testing.T) {
	t.Parallel()

	sixSourceTrades := make([]canonical.Trade, 0, 6)
	for i, src := range []string{"a", "b", "c", "d", "e", "f"} {
		sixSourceTrades = append(sixSourceTrades, canonical.Trade{
			Source: src,
			Ledger: uint32(i + 1),
		})
	}

	unscored := edgeConfidence(confidenceComputation{}, false, sixSourceTrades)
	if unscored > confidence.BootstrapConfidenceCap {
		t.Fatalf("unscored 6-source edge confidence = %v, want <= %v — an edge the scorer "+
			"COULD NOT score must not outrank one it did",
			unscored, confidence.BootstrapConfidenceCap)
	}

	// Ranking below the cap is preserved: fewer sources still reads lower.
	oneSource := edgeConfidence(confidenceComputation{}, false, sixSourceTrades[:1])
	if !(oneSource < unscored) {
		t.Errorf("single-source unscored (%v) must rank below six-source unscored (%v)", oneSource, unscored)
	}

	// A scored edge passes through untouched.
	scored := edgeConfidence(confidenceComputation{
		Score: confidence.Score{Confidence: 0.42},
	}, true, sixSourceTrades)
	if scored != 0.42 {
		t.Errorf("scored edge confidence = %v, want the score verbatim (0.42)", scored)
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
		if !got[i].frozenSince.IsZero() {
			t.Errorf("marker %d frozenSince = %s, want absent (the leg's freeze bucket is not the target's)",
				i, got[i].frozenSince)
		}
	}
}

// TestTriangulate_FrozenLegStaysRefusedOnATickItsWindowIsEmpty is the
// second half of the frozen-leg guard: a freeze must not be launderable through
// triangulation on the ticks AFTER the one that fired it either.
//
// The laundering guard read a set rebuilt at the top of every tick, and
// the only thing that ever wrote to it was engageFreeze. But
// refreshPairWindow returns BEFORE the freeze step when the window is
// empty, when it is under the USD-volume floor, and when the VWAP has no
// trades — and a pair whose market has just been manipulated on one thin
// venue is exactly the pair whose next bucket is empty. On that tick the
// pair is still frozen (its ADR-0019 hold runs for tens of minutes, its
// marker and its last-known-good value are both deliberately still in
// Redis) yet nothing re-entered it into the set, so the chain read the
// LKG as a fresh leg and published the product to a target that carries
// no frozen flag.
//
// Tick 1 here is the same-tick frozen-leg scenario. Tick 2 is the hole.
func TestTriangulate_FrozenLegStaysRefusedOnATickItsWindowIsEmpty(t *testing.T) {
	ctx := context.Background()
	leg1 := xlmUsdtPair(t) // the pair that freezes
	leg2 := mkPair(t, "crypto", "USDT", "fiat", "EUR")
	target := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	o := New(nil, cache, Config{
		Pairs:        []canonical.Pair{leg1},
		Windows:      []time.Duration{window},
		Anomaly:      newAnomalyChecker(t, leg1),
		FreezeWriter: &recordingFreezeMarker{},
		Triangulations: []TriangulationChain{{
			Target: target,
			Legs:   []canonical.Pair{leg1, leg2},
		}},
	})

	leg1Key := cachekeys.VWAP(leg1.Base, leg1.Quote, window).String()
	leg2Key := cachekeys.VWAP(leg2.Base, leg2.Quote, window).String()
	targetKey := cachekeys.VWAP(target.Base, target.Quote, window).String()
	cache.Set(ctx, leg1Key, "1.000000000000", time.Minute)
	cache.Set(ctx, leg2Key, "0.900000000000", time.Hour)

	// Tick 1: prev = $1.00, this bucket prices XLM at ~$2.10 on one
	// source. The pair freezes and the chain is refused.
	stateKey := leg1.String() + ":" + window.String()
	o.prevVWAPs[stateKey] = big.NewRat(1, 1)
	o.store = &mockStore{trades: []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), time.Now()),
	}}
	if err := o.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if !o.freezeStates[stateKey].Active() {
		t.Fatal("setup: the manipulated bucket did not freeze the leg")
	}
	if mr.Exists(targetKey) {
		t.Fatal("setup: tick 1 already published the derived price")
	}

	// Tick 2: the leg's window is EMPTY, so refreshPairWindow returns
	// before the freeze step. The freeze is 30 seconds old and its hold
	// has most of ten minutes left.
	o.store = &mockStore{}
	before := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLeg))
	nextBucket(o)
	if err := o.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}

	if !o.freezeStates[stateKey].Active() {
		t.Fatal("the leg's freeze ended on a tick that never evaluated it")
	}
	if got, err := mr.Get(leg1Key); err != nil || got != "1.000000000000" {
		t.Fatalf("leg LKG = (%q, %v); the scenario needs it still in cache", got, err)
	}
	if mr.Exists(targetKey) {
		got, _ := mr.Get(targetKey)
		t.Errorf("target key %q written with %q on a tick the frozen leg's window was empty — "+
			"the leg's last-known-good was laundered into a derived price with no frozen flag",
			targetKey, got)
	}
	after := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLeg))
	if after-before != 1 {
		t.Errorf("triangulation outcome %q delta on tick 2 = %v, want 1", outcomeFrozenLeg, after-before)
	}
}

// TestTriangulate_RefusedLegIsNotReadBackFromCache covers the non-freeze
// half of the laundering route: refreshPairWindow refuses a configured
// pair's window (empty, under the USD-volume floor, or unfetchable) and
// returns before writing anything, but the pair's previous value is still
// in Redis. Read
// back as a chain leg, that value entered the cross-rate graph as if this
// tick had priced it, and the target published a fresh-looking composite
// on a leg nobody priced.
func TestTriangulate_RefusedLegIsNotReadBackFromCache(t *testing.T) {
	window := 5 * time.Minute
	for _, tc := range []struct {
		name     string
		trades   []canonical.Trade
		minUSD   float64
		storeErr error
	}{
		{name: "empty_window"},
		{name: "fetch_error", storeErr: errors.New("injected fetch failure")},
		{
			name: "below_min_usd_volume",
			trades: []canonical.Trade{
				buildTrade(t, big.NewInt(100_000_000), big.NewInt(100_000_000), time.Now()),
			},
			minUSD: 1e12,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			leg1 := xlmUsdtPair(t)
			leg2 := mkPair(t, "crypto", "USDT", "fiat", "EUR")
			target := mkPair(t, "crypto", "XLM", "fiat", "EUR")
			chain := TriangulationChain{Target: target, Legs: []canonical.Pair{leg1, leg2}}

			cache, mr := newTestRedis(t)
			o := New(&mockStore{trades: tc.trades, returnErr: tc.storeErr}, cache, Config{
				Pairs:          []canonical.Pair{leg1},
				Windows:        []time.Duration{window},
				MinUSDVolume:   tc.minUSD,
				Triangulations: []TriangulationChain{chain},
			})
			cache.Set(ctx, cachekeys.VWAP(leg1.Base, leg1.Quote, window).String(), "1.000000000000", time.Hour)
			cache.Set(ctx, cachekeys.VWAP(leg2.Base, leg2.Quote, window).String(), "0.900000000000", time.Hour)

			if err := o.Tick(ctx); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			if _, outcome := o.legPriceFromCache(ctx, chain, leg1, window); outcome == "" {
				t.Error("legPriceFromCache served the leg's cached value on a tick its refresh refused it")
			}
			targetKey := cachekeys.VWAP(target.Base, target.Quote, window).String()
			if mr.Exists(targetKey) {
				got, _ := mr.Get(targetKey)
				t.Errorf("target %s published %q from a leg this tick refused to price", targetKey, got)
			}
		})
	}
}

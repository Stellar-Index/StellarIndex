package orchestrator

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

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

// TestTick_Triangulation_HappyPath — all legs cached → orchestrator
// computes the implied target VWAP and writes it to cache.
func TestTick_Triangulation_HappyPath(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	// Pre-populate leg VWAPs as if the per-pair refresh just ran.
	mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000")
	mr.Set(cachekeys.VWAP(usdEUR.Base, usdEUR.Quote, window).String(), "0.900000000000")

	o := New(nil, cache, Config{
		Pairs:   []canonical.Pair{}, // no per-pair refresh; just exercise the triangulation pass
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
		},
	})

	before := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	after := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
	if after-before != 1 {
		t.Errorf("ok counter delta = %v, want 1", after-before)
	}

	// 0.08 × 0.90 = 0.072.
	got, err := mr.Get(cachekeys.VWAP(xlmEUR.Base, xlmEUR.Quote, window).String())
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if got != "0.072000000000" {
		t.Errorf("target VWAP = %q, want 0.072000000000", got)
	}
}

// TestTick_Triangulation_MissingLeg — a leg's window was empty so
// the cache key is absent. Outcome counter increments
// missing_leg, target key is NOT written.
func TestTick_Triangulation_MissingLeg(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	// Only first leg cached; second leg absent.
	mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000")

	o := New(nil, cache, Config{
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
		},
	})

	before := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("missing_leg"))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	after := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("missing_leg"))
	if after-before != 1 {
		t.Errorf("missing_leg counter delta = %v, want 1", after-before)
	}

	if mr.Exists(cachekeys.VWAP(xlmEUR.Base, xlmEUR.Quote, window).String()) {
		t.Error("target VWAP should not exist when a leg is missing")
	}
}

// TestTick_Triangulation_ParseError — a malformed cached value
// (Postgres / upstream regression) surfaces as parse_error rather
// than panicking the tick.
func TestTick_Triangulation_ParseError(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000")
	mr.Set(cachekeys.VWAP(usdEUR.Base, usdEUR.Quote, window).String(), "not-a-number")

	o := New(nil, cache, Config{
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
		},
	})

	before := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("parse_error"))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	after := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("parse_error"))
	if after-before != 1 {
		t.Errorf("parse_error counter delta = %v, want 1", after-before)
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

// TestTick_Triangulation_FXSnap_HappyPath — when FXStore is wired and
// returns a quote for the FX leg, the orchestrator uses the snap
// price (not the leg's cached VWAP) and bypasses the fallback counter.
// Asserts the bucket-end timestamp passed to FXStore is the most-
// recent UTC-aligned boundary of the window.
func TestTick_Triangulation_FXSnap_HappyPath(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000")
	// Note: NO cached VWAP for usdEUR — proves the snap path is what
	// supplies the FX leg's price.

	fx := &fakeFXStore{
		quote:      new(big.Rat).SetFrac(big.NewInt(90), big.NewInt(100)),
		observedAt: time.Now().UTC().Add(-1 * time.Minute),
		source:     "polygon-forex",
	}

	o := New(nil, cache, Config{
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
		},
		FXStore: fx,
	})

	beforeOK := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
	beforeFB := testutil.ToFloat64(obs.AggregatorFXSnapFallbackTotal.WithLabelValues(usdEUR.String()))

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	afterOK := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
	afterFB := testutil.ToFloat64(obs.AggregatorFXSnapFallbackTotal.WithLabelValues(usdEUR.String()))

	if afterOK-beforeOK != 1 {
		t.Errorf("ok counter delta = %v, want 1", afterOK-beforeOK)
	}
	if afterFB != beforeFB {
		t.Errorf("fx-snap fallback counter incremented on happy path: %v→%v", beforeFB, afterFB)
	}

	// 0.08 (cached) × 0.90 (snap) = 0.072.
	got, err := mr.Get(cachekeys.VWAP(xlmEUR.Base, xlmEUR.Quote, window).String())
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if got != "0.072000000000" {
		t.Errorf("target VWAP = %q, want 0.072000000000", got)
	}

	if len(fx.calls) != 1 {
		t.Fatalf("FXStore called %d times, want 1", len(fx.calls))
	}
	call := fx.calls[0]
	if !call.pair.Equal(usdEUR) {
		t.Errorf("FXStore queried with pair %s, want %s", call.pair, usdEUR)
	}
	// bucketEnd must be window-aligned (Truncate to 5m boundary).
	if !call.cutoff.Equal(call.cutoff.Truncate(window)) {
		t.Errorf("cutoff %v not aligned to %v boundary", call.cutoff, window)
	}
	// fxSources must be the deterministic ordered set.
	if len(call.fxSources) < 2 {
		t.Errorf("FXStore called with %d FX sources, want at least 2", len(call.fxSources))
	}
}

// TestTick_Triangulation_FXSnap_FallbackOnNoQuote — when the snap
// path has no row at-or-before bucketEnd, the orchestrator falls back
// to the cached-VWAP path AND increments the fallback counter. The
// chain still publishes (degraded but functional).
func TestTick_Triangulation_FXSnap_FallbackOnNoQuote(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000")
	mr.Set(cachekeys.VWAP(usdEUR.Base, usdEUR.Quote, window).String(), "0.900000000000")

	fx := &fakeFXStore{} // quote==nil → returns ErrNoFXQuote

	o := New(nil, cache, Config{
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
		},
		FXStore: fx,
	})

	beforeOK := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
	beforeFB := testutil.ToFloat64(obs.AggregatorFXSnapFallbackTotal.WithLabelValues(usdEUR.String()))

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	afterOK := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
	afterFB := testutil.ToFloat64(obs.AggregatorFXSnapFallbackTotal.WithLabelValues(usdEUR.String()))

	if afterOK-beforeOK != 1 {
		t.Errorf("ok counter delta = %v, want 1 (chain still publishes via cached-VWAP fallback)", afterOK-beforeOK)
	}
	if afterFB-beforeFB != 1 {
		t.Errorf("fallback counter delta = %v, want 1", afterFB-beforeFB)
	}
	got, err := mr.Get(cachekeys.VWAP(xlmEUR.Base, xlmEUR.Quote, window).String())
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if got != "0.072000000000" {
		t.Errorf("target VWAP = %q, want 0.072000000000 (computed from cached-VWAP fallback)", got)
	}
}

// TestTick_Triangulation_FXSnap_DBErrorAborts — non-ErrNoFXQuote
// errors from the FX store mean we can't trust ANY chained-fiat
// output this tick. The chain skips publish and surfaces redis_error;
// the fallback counter does NOT increment (this isn't a planned
// fallback, it's an outage signal).
func TestTick_Triangulation_FXSnap_DBErrorAborts(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000")
	mr.Set(cachekeys.VWAP(usdEUR.Base, usdEUR.Quote, window).String(), "0.900000000000")

	fx := &fakeFXStore{err: errors.New("connection refused")}

	o := New(nil, cache, Config{
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
		},
		FXStore: fx,
	})

	beforeErr := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("redis_error"))
	beforeFB := testutil.ToFloat64(obs.AggregatorFXSnapFallbackTotal.WithLabelValues(usdEUR.String()))

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	afterErr := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("redis_error"))
	afterFB := testutil.ToFloat64(obs.AggregatorFXSnapFallbackTotal.WithLabelValues(usdEUR.String()))

	if afterErr-beforeErr != 1 {
		t.Errorf("redis_error counter delta = %v, want 1", afterErr-beforeErr)
	}
	if afterFB != beforeFB {
		t.Errorf("fallback counter incremented on hard DB error: %v→%v", beforeFB, afterFB)
	}
	if mr.Exists(cachekeys.VWAP(xlmEUR.Base, xlmEUR.Quote, window).String()) {
		t.Error("target VWAP should not exist when FX-store errors")
	}
}

// TestTick_Triangulation_FXStoreNil_LegsUseCachedVWAP — when no
// FXStore is wired, FX legs read from the cached-VWAP path same as
// non-FX legs. Pre-X2.5 behaviour is preserved as the safe default.
func TestTick_Triangulation_FXStoreNil_LegsUseCachedVWAP(t *testing.T) {
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
		// FXStore omitted
	})

	beforeFB := testutil.ToFloat64(obs.AggregatorFXSnapFallbackTotal.WithLabelValues(usdEUR.String()))

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	afterFB := testutil.ToFloat64(obs.AggregatorFXSnapFallbackTotal.WithLabelValues(usdEUR.String()))

	if afterFB != beforeFB {
		t.Errorf("fallback counter incremented when FXStore is nil: %v→%v", beforeFB, afterFB)
	}
	got, err := mr.Get(cachekeys.VWAP(xlmEUR.Base, xlmEUR.Quote, window).String())
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if got != "0.072000000000" {
		t.Errorf("target VWAP = %q, want 0.072000000000", got)
	}
}

// TestTick_Triangulation_NoChainsConfigured — the Tick proceeds
// normally and never touches the triangulation path. No counter
// increments.
func TestTick_Triangulation_NoChainsConfigured(t *testing.T) {
	cache, _ := newTestRedis(t)
	o := New(nil, cache, Config{
		Windows: []time.Duration{5 * time.Minute},
		// Triangulations omitted
	})

	beforeOK := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
	beforeMiss := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("missing_leg"))

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	afterOK := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
	afterMiss := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("missing_leg"))

	if afterOK != beforeOK || afterMiss != beforeMiss {
		t.Errorf("triangulation counters changed without configured chains: ok %v→%v, missing %v→%v",
			beforeOK, afterOK, beforeMiss, afterMiss)
	}
}

// TestTriangulate_FrozenLegDoesNotPublishDerivedPrice (MNY-22) — a
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
// Post-fix the chain refuses (outcome "frozen_leg"), keeps the
// target's own prior value alive, and marks the target frozen so the
// derived pair tells the same truth as the leg it descends from.
//
// Proven red pre-fix: the target key was written with "0.900000000000"
// (the frozen 1.00 leg × the 0.90 leg) and Mark was called once (the
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
	// leg 2 is a healthy 0.90. Pre-fix the chain published 1.00 × 0.90.
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
// MNY-22 guard: the freeze check must be scoped to the pairs actually
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

package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// stubBaselineSource implements orchestrator.BaselineSource with a
// fixed return.
type stubBaselineSource struct {
	multi      baseline.MultiBaseline
	computedAt time.Time
	err        error
}

// LatestBaseline reports an unset computedAt as written just now, so a
// fixture that is not about freshness is never read as stale.
func (s stubBaselineSource) LatestBaseline(_ context.Context, _ canonical.Pair) (baseline.MultiBaseline, time.Time, error) {
	if s.computedAt.IsZero() && s.err == nil {
		return s.multi, time.Now(), nil
	}
	return s.multi, s.computedAt, s.err
}

// xlmUSDPair returns an XLM/fiat:USD pair — distinct from the
// XLM/USDT pair the existing orchestrator tests use, so we don't
// fight the package-level fixture choices when we want USD volume
// approximation to fire.
func xlmUSDPair(t *testing.T) canonical.Pair {
	t.Helper()
	xlm, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse fiat:USD: %v", err)
	}
	pair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("new pair: %v", err)
	}
	return pair
}

// makeXLMUSDTrade builds a trade on the XLM/fiat:USD pair with the
// given source + amounts. Reuses the existing test fixture pattern.
func makeXLMUSDTrade(t *testing.T, source string, base, quote int64, ts time.Time) canonical.Trade {
	t.Helper()
	return canonical.Trade{
		Source:      source,
		Ledger:      uint32(ts.Unix() % 1_000_000),
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        xlmUSDPair(t),
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

// TestConfidence_ScoreFlowsToCacheKey — happy path: orchestrator
// runs two ticks (the first warms prevVWAP, the second produces a
// real return). After tick 2 the confidence: cache key is present
// and parses as a valid Score JSON.
func TestConfidence_ScoreFlowsToCacheKey(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()

	store := &mockStore{
		trades: []canonical.Trade{
			makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
			makeXLMUSDTrade(t, "phoenix", 1_000_000, 1_245_000, now.Add(-20*time.Second)),
		},
	}
	rdb, _ := newTestRedis(t)

	bsrc := stubBaselineSource{
		multi: baseline.MultiBaseline{
			Day30: &baseline.Baseline{Median: 0.0001, MAD: 0.001, N: 1000},
		},
		computedAt: now,
	}

	orch := New(store, rdb, Config{
		Pairs:     []canonical.Pair{pair},
		Windows:   []time.Duration{1 * time.Minute},
		Interval:  1 * time.Hour,
		Baselines: bsrc,
	})

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	nextBucket(orch)
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	confKey := cachekeys.Confidence(pair.Base, pair.Quote, time.Minute)
	body, err := rdb.Get(context.Background(), confKey.String()).Bytes()
	if err != nil {
		t.Fatalf("confidence key %q missing from cache: %v", confKey, err)
	}

	var score confidence.Score
	if err := json.Unmarshal(body, &score); err != nil {
		t.Fatalf("confidence value not valid JSON: %v\nraw: %s", err, body)
	}
	if score.Confidence < 0 || score.Confidence > 1 {
		t.Errorf("Confidence = %v, want in [0, 1]", score.Confidence)
	}
	if score.Factors.SourceCount == 0 {
		t.Error("Factors.SourceCount = 0, expected non-zero (2 distinct sources)")
	}
}

// TestConfidence_SkipsWhenBaselinesNil — Baselines field nil →
// confidence step is a no-op; no key written.
func TestConfidence_SkipsWhenBaselinesNil(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()

	store := &mockStore{
		trades: []canonical.Trade{
			makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
		},
	}
	rdb, _ := newTestRedis(t)

	orch := New(store, rdb, Config{
		Pairs:    []canonical.Pair{pair},
		Windows:  []time.Duration{1 * time.Minute},
		Interval: 1 * time.Hour,
		// Baselines: nil
	})

	_ = orch.Tick(context.Background())
	nextBucket(orch)
	_ = orch.Tick(context.Background())

	confKey := cachekeys.Confidence(pair.Base, pair.Quote, time.Minute)
	if exists, _ := rdb.Exists(context.Background(), confKey.String()).Result(); exists != 0 {
		t.Errorf("confidence key %q present despite nil Baselines", confKey)
	}
}

// TestConfidence_DivergenceWiredFromCache — pre-seed a cached
// divergence Result in Redis (the same shape `internal/divergence`
// writes), tick the orchestrator, and confirm the resulting
// confidence factor reflects the cached divergence.
//
// We can't directly read `Factors.CrossOracle` from inside the
// orchestrator (it's part of the JSON-encoded Score), so we do the
// inverse: set up two scenarios — one with no cached divergence
// (sentinel = -1 → factor = 0.7), one with a low-divergence cached
// result (factor → 1.0). Confirm the second has higher confidence.
func TestConfidence_DivergenceWiredFromCache(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()
	bsrc := stubBaselineSource{
		multi: baseline.MultiBaseline{
			Day30: &baseline.Baseline{Median: 0.0001, MAD: 0.001, N: 1000},
		},
		computedAt: now,
	}

	runOnce := func(t *testing.T, seedDiv *divergence.CachedResult) confidence.Score {
		t.Helper()
		store := &mockStore{
			trades: []canonical.Trade{
				makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
				makeXLMUSDTrade(t, "phoenix", 1_000_000, 1_245_000, now.Add(-20*time.Second)),
			},
		}
		rdb, _ := newTestRedis(t)
		if seedDiv != nil {
			body, err := json.Marshal(seedDiv)
			if err != nil {
				t.Fatalf("seed marshal: %v", err)
			}
			if err := rdb.Set(context.Background(),
				cachekeys.Divergence(pair).String(), body, 5*time.Minute).Err(); err != nil {
				t.Fatalf("seed cache set: %v", err)
			}
		}
		orch := New(store, rdb, Config{
			Pairs:     []canonical.Pair{pair},
			Windows:   []time.Duration{1 * time.Minute},
			Interval:  1 * time.Hour,
			Baselines: bsrc,
		})
		_ = orch.Tick(context.Background())
		nextBucket(orch)
		_ = orch.Tick(context.Background())
		body, err := rdb.Get(context.Background(),
			cachekeys.Confidence(pair.Base, pair.Quote, time.Minute).String()).Bytes()
		if err != nil {
			t.Fatalf("confidence read: %v", err)
		}
		var s confidence.Score
		if err := json.Unmarshal(body, &s); err != nil {
			t.Fatalf("confidence unmarshal: %v", err)
		}
		return s
	}

	noCache := runOnce(t, nil)
	withinTolerance := runOnce(t, &divergence.CachedResult{
		PairID:         pair.String(),
		DivergencePct:  0.3, // within 1% tolerance → factor 1.0
		SuccessCount:   3,
		AgreementCount: 2,
	})

	// "No data" returns the neutral 0.7; "within tolerance" returns
	// 1.0 — so the wired-up scenario must score the CrossOracle
	// factor strictly higher. Asserting on the per-factor value
	// (rather than the combined Confidence) sidesteps the
	// LiquidityFactor's behaviour for our small fixture trades —
	// the wiring is what's under test, not the combiner output.
	if withinTolerance.Factors.CrossOracle <= noCache.Factors.CrossOracle {
		t.Errorf("CrossOracle factor: with-cache=%v should exceed no-cache=%v",
			withinTolerance.Factors.CrossOracle, noCache.Factors.CrossOracle)
	}
	if noCache.Factors.CrossOracle != 0.7 {
		t.Errorf("no-cache CrossOracle = %v, want 0.7 (neutral)", noCache.Factors.CrossOracle)
	}
	if withinTolerance.Factors.CrossOracle != 1.0 {
		t.Errorf("within-tolerance CrossOracle = %v, want 1.0", withinTolerance.Factors.CrossOracle)
	}

	// ADR-0019 Phase 3 agreement transparency: the cached result's
	// AgreementCount flows into the served decomposition, and the
	// checked flag disambiguates neutral-because-unchecked from
	// neutral-because-diverging (CS-087 discipline).
	if noCache.Factors.CrossOracleChecked {
		t.Error("no-cache CrossOracleChecked = true, want false")
	}
	if noCache.Factors.CrossOracleAgreement != 0 {
		t.Errorf("no-cache CrossOracleAgreement = %d, want 0", noCache.Factors.CrossOracleAgreement)
	}
	if !withinTolerance.Factors.CrossOracleChecked {
		t.Error("with-cache CrossOracleChecked = false, want true")
	}
	if withinTolerance.Factors.CrossOracleAgreement != 2 {
		t.Errorf("with-cache CrossOracleAgreement = %d, want 2", withinTolerance.Factors.CrossOracleAgreement)
	}
}

// TestConfidence_DivergenceLowSuccessCountIgnored — a cached
// Result with SuccessCount < 2 doesn't count: the divergence input
// passes the "no data" sentinel even when DivergencePct is set.
//
// This guards against scoring a single reference's hiccup as a
// trustworthy multi-source signal.
func TestConfidence_DivergenceLowSuccessCountIgnored(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()
	bsrc := stubBaselineSource{
		multi: baseline.MultiBaseline{
			Day30: &baseline.Baseline{Median: 0.0001, MAD: 0.001, N: 1000},
		},
		computedAt: now,
	}

	store := &mockStore{
		trades: []canonical.Trade{
			makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
		},
	}
	rdb, _ := newTestRedis(t)

	body, _ := json.Marshal(&divergence.CachedResult{
		PairID:         pair.String(),
		DivergencePct:  0.3, // would be in-tolerance, but…
		SuccessCount:   1,   // … below trust floor; should be ignored
		AgreementCount: 1,   // must not leak into the served decomposition
	})
	if err := rdb.Set(context.Background(),
		cachekeys.Divergence(pair).String(), body, 5*time.Minute).Err(); err != nil {
		t.Fatalf("seed cache set: %v", err)
	}

	orch := New(store, rdb, Config{
		Pairs:     []canonical.Pair{pair},
		Windows:   []time.Duration{1 * time.Minute},
		Interval:  1 * time.Hour,
		Baselines: bsrc,
	})
	_ = orch.Tick(context.Background())
	nextBucket(orch)
	_ = orch.Tick(context.Background())

	scoreBody, err := rdb.Get(context.Background(),
		cachekeys.Confidence(pair.Base, pair.Quote, time.Minute).String()).Bytes()
	if err != nil {
		t.Fatalf("confidence read: %v", err)
	}
	var s confidence.Score
	if err := json.Unmarshal(scoreBody, &s); err != nil {
		t.Fatalf("confidence unmarshal: %v", err)
	}
	// 0.7 is the documented "no cross-oracle data" neutral. With a
	// single-source cached result we expect the same value.
	const wantNeutral = 0.7
	if s.Factors.CrossOracle < wantNeutral-1e-6 || s.Factors.CrossOracle > wantNeutral+1e-6 {
		t.Errorf("CrossOracle factor = %v, want %v (neutral; single-source ignored)",
			s.Factors.CrossOracle, wantNeutral)
	}
	// Below the trust floor the signal is UNCHECKED — the agreement
	// count must not leak through either (CS-087: a single
	// responder's corroboration is not a multi-source verdict).
	if s.Factors.CrossOracleChecked {
		t.Error("CrossOracleChecked = true below trust floor, want false")
	}
	if s.Factors.CrossOracleAgreement != 0 {
		t.Errorf("CrossOracleAgreement = %d below trust floor, want 0", s.Factors.CrossOracleAgreement)
	}
}

// TestConfidence_DivergenceMinSourcesFromConfig — GH-1046: the
// confidence step's cross-oracle trust floor must follow
// Config.DivergenceMinSources, not a hardcoded 2. A cached result with
// SuccessCount=2 clears the package default (2) but must be ignored
// once the operator raises the configured quorum to 3 — mirroring the
// divergence worker's own MinSourcesForWarning and the API adapter's
// mirror of it.
func TestConfidence_DivergenceMinSourcesFromConfig(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()
	bsrc := stubBaselineSource{
		multi: baseline.MultiBaseline{
			Day30: &baseline.Baseline{Median: 0.0001, MAD: 0.001, N: 1000},
		},
		computedAt: now,
	}

	store := &mockStore{
		trades: []canonical.Trade{
			makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
		},
	}
	rdb, _ := newTestRedis(t)

	body, _ := json.Marshal(&divergence.CachedResult{
		PairID:         pair.String(),
		DivergencePct:  0.3, // would be in-tolerance if trusted
		SuccessCount:   2,   // clears the package default (2)…
		AgreementCount: 2,
	})
	if err := rdb.Set(context.Background(),
		cachekeys.Divergence(pair).String(), body, 5*time.Minute).Err(); err != nil {
		t.Fatalf("seed cache set: %v", err)
	}

	orch := New(store, rdb, Config{
		Pairs:     []canonical.Pair{pair},
		Windows:   []time.Duration{1 * time.Minute},
		Interval:  1 * time.Hour,
		Baselines: bsrc,
		// … but the operator raised the quorum to 3 after a
		// flaky-reference incident. The confidence step must respect it.
		DivergenceMinSources: 3,
	})
	_ = orch.Tick(context.Background())
	nextBucket(orch)
	_ = orch.Tick(context.Background())

	scoreBody, err := rdb.Get(context.Background(),
		cachekeys.Confidence(pair.Base, pair.Quote, time.Minute).String()).Bytes()
	if err != nil {
		t.Fatalf("confidence read: %v", err)
	}
	var s confidence.Score
	if err := json.Unmarshal(scoreBody, &s); err != nil {
		t.Fatalf("confidence unmarshal: %v", err)
	}
	if s.Factors.CrossOracleChecked {
		t.Error("CrossOracleChecked = true below the CONFIGURED quorum (3), want false — SuccessCount=2 only clears the stale hardcoded default")
	}
	const wantNeutral = 0.7
	if s.Factors.CrossOracle < wantNeutral-1e-6 || s.Factors.CrossOracle > wantNeutral+1e-6 {
		t.Errorf("CrossOracle factor = %v, want %v (neutral; below configured quorum)", s.Factors.CrossOracle, wantNeutral)
	}
}

// TestDistinctSourceClassCount — exchange + oracle + aggregator
// are three distinct classes in the registry. Verifies the
// registry-backed implementation collapses same-class trades
// (two CEXes count as 1) and counts cross-class trades correctly.
func TestDistinctSourceClassCount(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()

	cases := []struct {
		name    string
		sources []string
		want    int
	}{
		{"empty", nil, 0},
		// Both CEXes — same Class:Subclass bucket → 1.
		{"two CEXes", []string{"binance", "coinbase"}, 1},
		// CEX + DEX — same Class but distinct Subclass → 2.
		// This is the ADR-0019 worked example case the Subclass
		// field exists for.
		{"CEX + DEX", []string{"binance", "soroswap"}, 2},
		// CEX + DEX + FX — three Subclasses under ClassExchange → 3.
		{"CEX + DEX + FX", []string{"binance", "soroswap", "exchangeratesapi"}, 3},
		// CEX + Oracle — distinct parent classes → 2.
		{"CEX + Oracle", []string{"binance", "reflector-dex"}, 2},
		// Four buckets: CEX + Oracle + Aggregator + AuthoritySanity.
		{"four buckets", []string{"binance", "reflector-dex", "coingecko", "ecb"}, 4},
		// Unknown source falls into the registry's default
		// (ClassExchange + empty Subclass). If no other exchange:cex
		// is present it counts as its own bucket.
		{"unknown alone", []string{"unknown_source"}, 1},
		// Two unknowns collapse (same fallback bucket).
		{"two unknowns", []string{"unknown_a", "unknown_b"}, 1},
		// Two same-DEX-subclass sources collapse to 1.
		{"two DEXes", []string{"soroswap", "phoenix"}, 1},
	}
	for _, tc := range cases {
		trades := make([]canonical.Trade, 0, len(tc.sources))
		for _, s := range tc.sources {
			trades = append(trades, makeXLMUSDTradeWithSource(t, pair, s, now))
		}
		got := distinctSourceClassCount(trades)
		if got != tc.want {
			t.Errorf("%s: distinctSourceClassCount = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// makeXLMUSDTradeWithSource is mkObservationTrade variant — same
// shape but with the given pair already resolved.
func makeXLMUSDTradeWithSource(t *testing.T, pair canonical.Pair, source string, ts time.Time) canonical.Trade {
	t.Helper()
	return canonical.Trade{
		Source:      source,
		Ledger:      uint32(ts.Unix() % 1_000_000),
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(1)),
		QuoteAmount: canonical.NewAmount(big.NewInt(1)),
	}
}

// TestPhase2Freeze_BlocksVWAPPublish — when the 3-signal AND
// fires, the orchestrator BAILS BEFORE the VWAP cache write. The
// previous bucket's value (or absence) stays in cache.
//
// Setup forces all three signals to fire:
//   - z >> 5: a return that's huge vs the baseline median+MAD
//   - source_count == 1: single-source trade slice
//   - low confidence: small bucket volume + single source
func TestPhase2Freeze_BlocksVWAPPublish(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()

	// First-tick prevVWAP comparator setup: tight price.
	// Second tick has a huge return relative to the prev →
	// z dominates, source_count=1, confidence collapses.
	tightPair := []canonical.Trade{
		makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_000_000, now.Add(-90*time.Second)), // tick 1
	}
	bigSpike := []canonical.Trade{
		makeXLMUSDTrade(t, "soroswap", 1_000_000, 5_000_000, now.Add(-30*time.Second)), // tick 2: 5x jump, single source
	}

	store := &mockStore{}
	rdb, _ := newTestRedis(t)

	// Tight baseline so any meaningful return reads as a huge z.
	bsrc := stubBaselineSource{
		multi: baseline.MultiBaseline{
			Day30: &baseline.Baseline{Median: 0.0, MAD: 0.0001, N: 1000},
		},
		computedAt: now,
	}

	orch := New(store, rdb, Config{
		Pairs:     []canonical.Pair{pair},
		Windows:   []time.Duration{1 * time.Minute},
		Interval:  1 * time.Hour,
		Baselines: bsrc,
		// No Anomaly checker → Phase 1 runs in skip mode; we're
		// isolating Phase 2 here.
	})

	// Tick 1: warms prevVWAP. No freeze possible (no comparator yet).
	store.trades = tightPair
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	vwapKey := cachekeys.VWAP(pair.Base, pair.Quote, time.Minute)
	if exists, _ := rdb.Exists(context.Background(), vwapKey.String()).Result(); exists != 1 {
		t.Fatalf("VWAP key missing after tick 1 — confidence skip shouldn't block tick 1 publish")
	}

	// Tick 2: huge spike, single source. Phase 2 should fire.
	// The tick-1 VWAP stays in cache (overwrite suppressed).
	tick1Value, err := rdb.Get(context.Background(), vwapKey.String()).Result()
	if err != nil {
		t.Fatalf("read tick-1 VWAP: %v", err)
	}
	store.trades = bigSpike
	nextBucket(orch)
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	tick2Value, err := rdb.Get(context.Background(), vwapKey.String()).Result()
	if err != nil {
		t.Fatalf("read tick-2 VWAP: %v", err)
	}
	if tick2Value != tick1Value {
		t.Errorf("VWAP cache changed across a Phase 2 freeze: tick1=%q tick2=%q (Phase 2 should preserve LKG)",
			tick1Value, tick2Value)
	}

	// Confidence cache must NOT carry forward the spike's score.
	confKey := cachekeys.Confidence(pair.Base, pair.Quote, time.Minute)
	bodyAfter, err := rdb.Get(context.Background(), confKey.String()).Bytes()
	if err == nil {
		// Some entry exists — must be from tick 1 (a fresh tick-1 publish
		// can write a confidence entry; that's fine). What matters is the
		// value is NOT from tick 2.
		var s confidence.Score
		if err := json.Unmarshal(bodyAfter, &s); err != nil {
			t.Fatalf("decode confidence: %v", err)
		}
		// Tick 1 had no prev → confidence step skipped → no key written.
		// So the cache should have NO confidence: entry at all.
		t.Errorf("confidence cache key present after Phase 2 freeze — should not have been written: %v", s)
	}
}

// TestConfidence_BaselineMissingDoesNotBlockVWAP — when the
// Baselines source returns an error, the VWAP cache write must
// still succeed. Confidence is enrichment, not a publish gate.
func TestConfidence_BaselineMissingDoesNotBlockVWAP(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()

	store := &mockStore{
		trades: []canonical.Trade{
			makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
		},
	}
	rdb, _ := newTestRedis(t)

	bsrc := stubBaselineSource{err: errors.New("not found")}

	orch := New(store, rdb, Config{
		Pairs:     []canonical.Pair{pair},
		Windows:   []time.Duration{1 * time.Minute},
		Interval:  1 * time.Hour,
		Baselines: bsrc,
	})

	_ = orch.Tick(context.Background())
	nextBucket(orch)
	_ = orch.Tick(context.Background())

	vwapKey := cachekeys.VWAP(pair.Base, pair.Quote, time.Minute)
	if exists, _ := rdb.Exists(context.Background(), vwapKey.String()).Result(); exists == 0 {
		t.Errorf("VWAP key missing despite baseline-source error: confidence failure must not block VWAP")
	}
	confKey := cachekeys.Confidence(pair.Base, pair.Quote, time.Minute)
	if exists, _ := rdb.Exists(context.Background(), confKey.String()).Result(); exists != 0 {
		t.Errorf("confidence key present despite baseline error")
	}
}

// TestApproxUSDVolume_CEXQuoteIsEightDecimals (M13) — bucket USD volume
// must scale each quote amount by its SOURCE's smallest-unit decimals,
// not a fixed 1e7. A CEX quote is 8dp (externalAmountDecimals = 8), so
// the pre-fix fixed-1e7 divisor overstated it 10×. Because
// LiquidityFactor is log-linear inside its band, that 10× swings the
// factor by exactly ln(10)/ln(ceiling/floor) — one third of the full
// [0,1] range on today's [1e3, 1e6] band (it was one HALF on the
// [1e3, 1e5] band that shipped until 2026-07-25, where the inflated
// figure also saturated at 1.0). A material distortion either way, not
// the "insensitive" error the old comment claimed.
func TestApproxUSDVolume_CEXQuoteIsEightDecimals(t *testing.T) {
	pair := xlmUSDPair(t)
	ts := time.Now().UTC()

	// $30,000 of CEX (binance) quote volume at 8 decimals.
	const usdWhole = 30_000
	quote8dp := int64(usdWhole) * 100_000_000
	trades := []canonical.Trade{makeXLMUSDTrade(t, "binance", 1_000_000, quote8dp, ts)}

	got := approxUSDVolume(trades, pair)
	if math.Abs(got-float64(usdWhole)) > 1.0 {
		t.Fatalf("approxUSDVolume = %.2f, want ~%d (8dp CEX scale, not 10× that)", got, usdWhole)
	}

	corrected := confidence.LiquidityFactor(got)                // F(30000)
	buggy := confidence.LiquidityFactor(float64(usdWhole) * 10) // what /1e7 produced: F(300000)
	if math.Abs(corrected-0.492376) > 1e-5 {
		t.Errorf("LiquidityFactor(30000) = %.6f, want ~0.492376 (= ln(30)/ln(1000))", corrected)
	}
	if math.Abs(buggy-0.825709) > 1e-5 {
		t.Errorf("pre-fix LiquidityFactor(300000) = %.6f, want ~0.825709 (= ln(300)/ln(1000))", buggy)
	}
	// Both volumes sit inside the log-linear band, so the swing is
	// exactly one decade's worth of it: ln(10)/ln(1e6/1e3) = 1/3.
	if math.Abs((buggy-corrected)-1.0/3.0) > 1e-9 {
		t.Errorf("factor swing = %.6f, want exactly 1/3 (one decade of a [1e3, 1e6] log band)",
			buggy-corrected)
	}
	if buggy-corrected < 0.25 {
		t.Errorf("factor swing = %.4f, want a material (>0.25) gap the fix removes", buggy-corrected)
	}
}

// TestApproxUSDVolume_PerSourceDecimals (M13) — resolution is
// per-source: an 8dp CEX quote and a 6dp FX quote of the same $30k each
// both read as $30k, summing to $60k. A single fixed divisor could not
// value both correctly.
func TestApproxUSDVolume_PerSourceDecimals(t *testing.T) {
	pair := xlmUSDPair(t)
	ts := time.Now().UTC()
	trades := []canonical.Trade{
		makeXLMUSDTrade(t, "binance", 1_000_000, 30_000*100_000_000, ts), // 8dp
		makeXLMUSDTrade(t, "massive", 1_000_000, 30_000*1_000_000, ts),   // 6dp
	}
	got := approxUSDVolume(trades, pair)
	if math.Abs(got-60_000) > 1.0 {
		t.Fatalf("approxUSDVolume = %.2f, want ~60000 ($30k @8dp + $30k @6dp)", got)
	}
}

// TestApproxUSDVolume_OnChainSourcesAreSevenDecimals (MNY-05) closes the
// third decimal class. The two tests above cover 8dp CEX and 6dp FX; the
// on-chain DEXes are 7dp and were the ones actually getting it wrong.
//
// Metadata.AmountScaleDecimals() falls back to 8 when AmountDecimals is
// unset, and soroswap/aquarius/phoenix/comet/sdex declared nothing — so
// every on-chain quote amount was divided by 1e8 instead of 1e7 and read
// as a TENTH of its real USD value. That is the direction that matters:
// LiquidityFactor is log-linear over [1e3, 1e5], so understating $30k as
// $3k drags the factor down and quietly suppresses confidence on exactly
// the venues this index is about.
//
// TestOnChainSourcesDeclareSevenDecimals (internal/sources/external) pins
// the registry DECLARATION. This pins the CONSUMPTION — that
// approxUSDVolume actually honours it. A declaration nothing reads
// correctly is not a fix, and neither test alone would catch the other's
// regression.
//
// Proven red: with AmountDecimals dropped from the on-chain registry
// entries, every subtest reports a tenth of its expected value.
func TestApproxUSDVolume_OnChainSourcesAreSevenDecimals(t *testing.T) {
	pair := xlmUSDPair(t)
	ts := time.Now().UTC()

	const usdWhole = 30_000
	quote7dp := int64(usdWhole) * 10_000_000 // 7dp stroops

	for _, source := range []string{"soroswap", "aquarius", "phoenix", "comet", "sdex"} {
		t.Run(source, func(t *testing.T) {
			trades := []canonical.Trade{makeXLMUSDTrade(t, source, 1_000_000, quote7dp, ts)}
			got := approxUSDVolume(trades, pair)
			if math.Abs(got-float64(usdWhole)) > 1.0 {
				t.Fatalf("approxUSDVolume(%s) = %.2f, want ~%d — 7dp on-chain amount "+
					"scaled by the wrong power of ten (a 1e8 divisor yields %.2f)",
					source, got, usdWhole, float64(usdWhole)/10)
			}
		})
	}

	// All three decimal classes in one bucket must each read as $30k,
	// summing to $90k. A single fixed divisor cannot value all three.
	t.Run("all three decimal classes sum correctly", func(t *testing.T) {
		trades := []canonical.Trade{
			makeXLMUSDTrade(t, "soroswap", 1_000_000, quote7dp, ts),            // 7dp on-chain
			makeXLMUSDTrade(t, "binance", 1_000_000, usdWhole*100_000_000, ts), // 8dp CEX
			makeXLMUSDTrade(t, "massive", 1_000_000, usdWhole*1_000_000, ts),   // 6dp FX
		}
		got := approxUSDVolume(trades, pair)
		if math.Abs(got-90_000) > 1.0 {
			t.Fatalf("approxUSDVolume = %.2f, want ~90000 ($30k each at 7dp, 8dp and 6dp)", got)
		}
	})
}

// TestApproxUSDVolume_NonUSDQuotedIsUnmeasuredNotZero (COR-14) pins the
// production side of the sentinel: XLM/EUR carries real EUR volume that
// this package cannot convert to dollars without a live FX rate, so it
// must report "unmeasured", not "$0 of liquidity".
//
// The distinction is not cosmetic. A zero LiquidityFactor dominates the
// weighted geometric mean, so returning 0 here served confidence=0 for
// every non-USD-quoted pair (8 of the 12 in defaultPairs()) whatever
// their z-score, source count, diversity or cross-oracle agreement —
// and pinned `confidence < 0.10`, one of the three legs the Phase 2
// freeze ANDs together, permanently true for them.
//
// Proven red pre-fix: approxUSDVolume returned 0.00 and the scored
// confidence came back 0.
func TestApproxUSDVolume_NonUSDQuotedIsUnmeasuredNotZero(t *testing.T) {
	xlm, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	eur, err := canonical.ParseAsset("fiat:EUR")
	if err != nil {
		t.Fatalf("parse fiat:EUR: %v", err)
	}
	pair, err := canonical.NewPair(xlm, eur)
	if err != nil {
		t.Fatalf("new pair: %v", err)
	}
	ts := time.Now().UTC()
	trades := []canonical.Trade{
		makeXLMUSDTradeWithSource(t, pair, "binance", ts),
		makeXLMUSDTradeWithSource(t, pair, "soroswap", ts),
	}

	got := approxUSDVolume(trades, pair)
	if got != confidence.LiquidityUnmeasured {
		t.Fatalf("approxUSDVolume(XLM/EUR) = %.2f, want the %.0f unmeasured sentinel — "+
			"0 would claim we measured no liquidity rather than that we could not measure any",
			got, confidence.LiquidityUnmeasured)
	}

	// End-to-end: the same bucket, scored. A healthy two-source,
	// two-class, low-z, mature-baseline window must not read as
	// zero-confidence just because its quote is not USD.
	score := confidence.Compute(confidence.Inputs{
		ZScore:                   0.3,
		SourceCount:              6,
		SourceClassCount:         2,
		LiquidityUSD:             got,
		CrossOracleDivergencePct: 0.4,
		BaselineAgeDays:          maxBaselineAgeDays,
	}, confidence.DefaultWeights())
	if score.Confidence <= 0 {
		t.Fatalf("healthy non-USD-quoted bucket scored %v, want > 0", score.Confidence)
	}
	// It must also clear the Phase 2 freeze's confidence threshold —
	// that leg has to be capable of DISAGREEING for the 3-signal AND to
	// mean anything on these pairs.
	if score.Confidence < DefaultPhase2ConfidenceMaxFreeze {
		t.Errorf("healthy non-USD-quoted bucket scored %v, below the %v freeze threshold — "+
			"the confidence leg of the 3-signal AND is still pinned true for these pairs",
			score.Confidence, DefaultPhase2ConfidenceMaxFreeze)
	}
}

// ─── RLT-260: the bootstrap cap's density gate must be reachable ──

// minuteSeries builds a 1-minute-aligned (vwap, bucket_end) series
// covering the 30 days ending at `now`, keeping only the minutes for
// which `keep` reports true (nil keeps every minute).
//
// Minute i's bucket END is now-30d+(i+1)m, so the last kept minute of
// a full series ends exactly at `now`: this is the maximal-density
// shape prices_1m can hold for the window
// timescale.Store.TimedVWAPsForPair1m reads, fed through the same
// SplitByLookback -> NewMultiBaseline path the refresher uses.
func minuteSeries(now time.Time, keep func(minute int) bool) []baseline.TimedVWAP {
	const minutesIn30d = 30 * 24 * 60
	start := now.Add(-baseline.Window30d)
	out := make([]baseline.TimedVWAP, 0, minutesIn30d)
	for i := 0; i < minutesIn30d; i++ {
		if keep != nil && !keep(i) {
			continue
		}
		out = append(out, baseline.TimedVWAP{
			// Nonzero (ReturnsFromVWAPs skips a zero predecessor) and
			// not perfectly flat, so every kept minute contributes a
			// return.
			VWAP:      100 + float64(i%11)*0.01,
			BucketEnd: start.Add(time.Duration(i+1) * time.Minute),
		})
	}
	return out
}

// scoreAtDensity runs the healthy-bucket inputs through
// confidence.Compute with the supplied density reading, so each test
// below differs only in the baseline behind it.
func scoreAtDensity(ageDays float64) confidence.Score {
	return confidence.Compute(confidence.Inputs{
		ZScore:                   0.3,
		SourceCount:              6,
		SourceClassCount:         2,
		LiquidityUSD:             250_000,
		CrossOracleDivergencePct: 0.4,
		BaselineAgeDays:          ageDays,
	}, confidence.DefaultWeights())
}

// uncappedGeoMean recomputes the normalised weighted geometric mean
// from the served decomposition — all six weights are 1.0 for these
// inputs (triangulation unchecked → weight 0, excluded), so this is
// what Compute must serve when the bootstrap cap does NOT bind.
func uncappedGeoMean(f confidence.Factors) float64 {
	product := f.ZScore * f.SourceCount * f.Diversity *
		f.Liquidity * f.CrossOracle * f.BaselineQuality
	return math.Pow(product, 1.0/6.0)
}

// TestBaselineAgeDays_MaximalDensityReleasesBootstrapCap — the real
// production boundary. A pair that traded in EVERY one of the 43,200
// minutes of the 30-day window, read through
// SplitByLookback -> NewMultiBaseline -> baselineAgeDays, must read
// exactly 30.0 days-equivalent and must NOT be capped at the
// bootstrap ceiling.
//
// Before RLT-260 this was unreachable by construction: N counts
// bucket-to-bucket RETURNS, so a full 43,200-bucket window yields
// 43,199 of them and N/1440 = 29.99931 — under every threshold in the
// package, forever. The cap was therefore unconditional and every
// asset's served confidence was pinned at 0.5.
func TestBaselineAgeDays_MaximalDensityReleasesBootstrapCap(t *testing.T) {
	now := time.Now().UTC()
	multi := baseline.NewMultiBaseline(baseline.SplitByLookback(minuteSeries(now, nil), now))
	if multi.Day30 == nil {
		t.Fatal("30d window in bootstrap on a maximal-density series")
	}
	if multi.Day30.N != 43_199 {
		t.Fatalf("Day30.N = %d, want 43,199 returns from 43,200 full minutes", multi.Day30.N)
	}

	age := baselineAgeDays(multi)
	if age != 30.0 {
		t.Errorf("baselineAgeDays(full 30d window) = %.8f, want exactly 30 — "+
			"a completely-observed window is 30 days-equivalent of history", age)
	}

	got := scoreAtDensity(age)
	want := uncappedGeoMean(got.Factors)
	if math.Abs(got.Confidence-want) > 1e-12 {
		t.Errorf("confidence on a maximal-density baseline = %.17f, want the uncapped "+
			"geometric mean %.17f (delta %g) — the bootstrap cap is still binding",
			got.Confidence, want, got.Confidence-want)
	}
	if got.Confidence <= confidence.BootstrapConfidenceCap {
		t.Errorf("confidence = %v <= the %v bootstrap cap on a fully-observed 30-day baseline",
			got.Confidence, confidence.BootstrapConfidenceCap)
	}
}

// TestBaselineAgeDays_ProductionDensityReleasesBootstrapCap — the
// shape actually measured on r1: the majors run ~99.2% minute
// coverage (baseline_quality 0.996). A pair that well observed is as
// mature as this index's data gets and must clear the cap; requiring
// literal perfection would leave the cap unconditional in practice
// even after the counting is fixed.
func TestBaselineAgeDays_ProductionDensityReleasesBootstrapCap(t *testing.T) {
	now := time.Now().UTC()
	// Drop one minute in 125 → 42,854 of 43,200 buckets (99.2%).
	series := minuteSeries(now, func(m int) bool { return m%125 != 0 })
	multi := baseline.NewMultiBaseline(baseline.SplitByLookback(series, now))
	if multi.Day30 == nil {
		t.Fatal("30d window in bootstrap on a 99.2 percent dense series")
	}

	age := baselineAgeDays(multi)
	// 42,854 of 43,200 minutes observed → 29.7597 days-equivalent.
	if want := 42_854.0 / 1440.0; math.Abs(age-want) > 1e-9 {
		t.Errorf("baselineAgeDays(99.2%% density) = %.6f, want %.6f", age, want)
	}

	got := scoreAtDensity(age)
	want := uncappedGeoMean(got.Factors)
	if math.Abs(got.Confidence-want) > 1e-12 {
		t.Errorf("confidence at 99.2%% density = %.17f, want the uncapped geometric mean "+
			"%.17f — still pinned at the bootstrap cap", got.Confidence, want)
	}
}

// TestBaselineAgeDays_SparseMaturePairStaysCapped — the W8.8
// (audit-2026-08-14) decision, pinned: the signal is sample DENSITY,
// not calendar age. A pair that has existed for the whole 30-day
// window but trades in only 200 minutes a day rests on a thin
// baseline and MUST stay capped. Relaxing the gate far enough to
// admit it would be the less-safe direction W8.8 refused.
func TestBaselineAgeDays_SparseMaturePairStaysCapped(t *testing.T) {
	now := time.Now().UTC()
	// 200 traded minutes per calendar day → 6,000 of 43,200 buckets.
	series := minuteSeries(now, func(m int) bool { return m%1440 < 200 })
	multi := baseline.NewMultiBaseline(baseline.SplitByLookback(series, now))
	if multi.Day30 == nil {
		t.Fatal("30d window in bootstrap on a 6,000-bucket series")
	}

	age := baselineAgeDays(multi)
	if want := 6000.0 / 1440.0; math.Abs(age-want) > 1e-9 {
		t.Errorf("baselineAgeDays(200 minutes a day) = %.6f, want %.6f", age, want)
	}

	got := scoreAtDensity(age)
	if got.Confidence != confidence.BootstrapConfidenceCap {
		t.Errorf("confidence on a calendar-mature but sparse baseline = %v, want the %v "+
			"bootstrap cap — density, not calendar age, is the gate (W8.8)",
			got.Confidence, confidence.BootstrapConfidenceCap)
	}
}

// gaugeValue reads one labelled series of a gauge from obs.Registry by name;
// ok is false when the series is not exported.
func gaugeValue(t *testing.T, name, label, value string) (float64, bool) {
	t.Helper()
	families, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// A baseline the refresher stopped rewriting is not scored against: past
// the one-day ceiling the pair reads as bootstrap (outcome baseline_stale),
// and its age is exported per pair either way.
func TestComputeConfidence_StaleBaselineReadsAsBootstrap(t *testing.T) {
	staleBefore := testutil.ToFloat64(obs.AggregatorConfidenceComputeTotal.WithLabelValues("baseline_stale"))
	bucketEnd := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	bsrc := stubBaselineSource{
		multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 1000}},
		computedAt: bucketEnd.Add(-25 * time.Hour),
	}
	bucketReturnCase(t, time.Minute, Config{Baselines: bsrc})
	if got := testutil.ToFloat64(obs.AggregatorConfidenceComputeTotal.WithLabelValues("baseline_stale")) - staleBefore; got < 1 {
		t.Errorf("confidence_compute_total{outcome=baseline_stale} rose by %v, want >= 1 for a 25h-old baseline", got)
	}
	age, ok := gaugeValue(t, "stellarindex_aggregator_baseline_age_seconds", "pair", xlmUSDPair(t).String())
	if want := (25*time.Hour + 5*time.Second).Seconds() + 60; !ok || age != want {
		t.Errorf("baseline_age_seconds = %v (exported=%v), want %v", age, ok, want)
	}
}

// The freeze record names the window whose z fired: here the 30d window's
// tighter MAD scores the +100% minute above the 1d window's.
func TestPhase2_FreezeReasonNamesTheAttributingWindow(t *testing.T) {
	marker := &recordingFreezeMarker{}
	bsrc := stubBaselineSource{
		multi: baseline.MultiBaseline{
			Day1:  &baseline.Baseline{Median: 0, MAD: 0.05, N: 1439},
			Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 40000},
		},
		computedAt: time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC),
	}
	if _, frozen := bucketReturnCase(t, time.Minute, Config{Baselines: bsrc, FreezeWriter: marker}); !frozen {
		t.Fatal("a +100% single-source minute did not freeze")
	}
	if len(marker.marks) == 0 {
		t.Fatal("no freeze recorded")
	}
	if reason := marker.marks[0].decision.Reason; !strings.Contains(reason, " z_window=30d ") {
		t.Errorf("freeze reason %q does not name the attributing window z_window=30d", reason)
	}
}

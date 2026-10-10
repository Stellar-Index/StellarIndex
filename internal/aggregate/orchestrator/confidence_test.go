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
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// stubBaselineSource implements BaselineSource with a fixed return.
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

// xlmUSDPair is XLM/fiat:USD, distinct from the XLM/USDT pair the other
// orchestrator tests use, so USD volume approximation fires.
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

// matureBaselines is a 30d baseline written just now.
func matureBaselines(now time.Time) stubBaselineSource {
	return stubBaselineSource{
		multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0.0001, MAD: 0.001, N: 1000}},
		computedAt: now,
	}
}

// confidenceRun ticks twice (the first warms prevVWAP) over trades and
// returns the cache. seed, when set, is stored as the cached divergence
// result; mod adjusts the Config.
func confidenceRun(t *testing.T, trades []canonical.Trade, bsrc BaselineSource, seed *divergence.CachedResult, mod func(*Config)) *redis.Client {
	t.Helper()
	pair := xlmUSDPair(t)
	rdb, _ := newTestRedis(t)
	if seed != nil {
		body, err := json.Marshal(seed)
		if err != nil {
			t.Fatalf("seed marshal: %v", err)
		}
		if err := rdb.Set(context.Background(), cachekeys.Divergence(pair).String(), body, 5*time.Minute).Err(); err != nil {
			t.Fatalf("seed cache set: %v", err)
		}
	}
	cfg := Config{
		Pairs:     []canonical.Pair{pair},
		Windows:   []time.Duration{time.Minute},
		Interval:  time.Hour,
		Baselines: bsrc,
	}
	if mod != nil {
		mod(&cfg)
	}
	orch := New(&mockStore{trades: trades}, rdb, cfg)
	_ = orch.Tick(context.Background())
	nextBucket(orch)
	_ = orch.Tick(context.Background())
	return rdb
}

// cachedScore reads the confidence: entry the run published.
func cachedScore(t *testing.T, rdb *redis.Client) confidence.Score {
	t.Helper()
	pair := xlmUSDPair(t)
	body, err := rdb.Get(context.Background(), cachekeys.Confidence(pair.Base, pair.Quote, time.Minute).String()).Bytes()
	if err != nil {
		t.Fatalf("confidence read: %v", err)
	}
	var s confidence.Score
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("confidence value not valid JSON: %v\nraw: %s", err, body)
	}
	return s
}

func twoSourceTrades(t *testing.T, now time.Time) []canonical.Trade {
	return []canonical.Trade{
		makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
		makeXLMUSDTrade(t, "phoenix", 1_000_000, 1_245_000, now.Add(-20*time.Second)),
	}
}

func TestConfidence_ScoreFlowsToCacheKey(t *testing.T) {
	now := time.Now().UTC()
	score := cachedScore(t, confidenceRun(t, twoSourceTrades(t, now), matureBaselines(now), nil, nil))
	if score.Confidence < 0 || score.Confidence > 1 {
		t.Errorf("Confidence = %v, want in [0, 1]", score.Confidence)
	}
	if score.Factors.SourceCount == 0 {
		t.Error("Factors.SourceCount = 0, expected non-zero (2 distinct sources)")
	}
}

// Confidence is enrichment: no baselines, or a baseline error, writes no
// confidence key and never blocks the VWAP publish.
func TestConfidence_NoBaselinesWritesNoKey(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()
	trades := []canonical.Trade{makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second))}
	for name, bsrc := range map[string]BaselineSource{
		"nil":   nil,
		"error": stubBaselineSource{err: errors.New("not found")},
	} {
		t.Run(name, func(t *testing.T) {
			rdb := confidenceRun(t, trades, bsrc, nil, nil)
			ctx := context.Background()
			confKey := cachekeys.Confidence(pair.Base, pair.Quote, time.Minute)
			if exists, _ := rdb.Exists(ctx, confKey.String()).Result(); exists != 0 {
				t.Errorf("confidence key %q present despite %s baselines", confKey, name)
			}
			vwapKey := cachekeys.VWAP(pair.Base, pair.Quote, time.Minute)
			if exists, _ := rdb.Exists(ctx, vwapKey.String()).Result(); exists == 0 {
				t.Error("VWAP key missing: confidence failure must not block VWAP")
			}
		})
	}
}

// The cached divergence result feeds the CrossOracle factor: no data is
// the neutral 0.7, an in-tolerance result with quorum is 1.0.
func TestConfidence_DivergenceWiredFromCache(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()
	run := func(seed *divergence.CachedResult) confidence.Score {
		return cachedScore(t, confidenceRun(t, twoSourceTrades(t, now), matureBaselines(now), seed, nil))
	}
	noCache := run(nil)
	withinTolerance := run(&divergence.CachedResult{
		PairID:         pair.String(),
		DivergencePct:  0.3, // within 1% tolerance
		SuccessCount:   3,
		AgreementCount: 2,
	})

	if noCache.Factors.CrossOracle != 0.7 {
		t.Errorf("no-cache CrossOracle = %v, want 0.7 (neutral)", noCache.Factors.CrossOracle)
	}
	if withinTolerance.Factors.CrossOracle != 1.0 {
		t.Errorf("within-tolerance CrossOracle = %v, want 1.0", withinTolerance.Factors.CrossOracle)
	}
	// The checked flag separates neutral-because-unchecked from
	// neutral-because-diverging.
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

// A cached divergence result below the trust floor (default 2, or the
// configured DivergenceMinSources) reads as unchecked: neutral factor and
// no agreement count leaking through.
func TestConfidence_DivergenceBelowTrustFloorIgnored(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()
	cases := []struct {
		name       string
		successes  int
		agreements int
		minSources int
	}{
		{"single responder under default floor", 1, 1, 0},
		{"two responders under configured quorum of 3", 2, 2, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trades := []canonical.Trade{makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second))}
			seed := &divergence.CachedResult{
				PairID:         pair.String(),
				DivergencePct:  0.3, // in tolerance, were it trusted
				SuccessCount:   tc.successes,
				AgreementCount: tc.agreements,
			}
			s := cachedScore(t, confidenceRun(t, trades, matureBaselines(now), seed, func(c *Config) {
				c.DivergenceMinSources = tc.minSources
			}))
			const wantNeutral = 0.7
			if s.Factors.CrossOracle < wantNeutral-1e-6 || s.Factors.CrossOracle > wantNeutral+1e-6 {
				t.Errorf("CrossOracle factor = %v, want %v (neutral)", s.Factors.CrossOracle, wantNeutral)
			}
			if s.Factors.CrossOracleChecked {
				t.Error("CrossOracleChecked = true below the trust floor, want false")
			}
			if s.Factors.CrossOracleAgreement != 0 {
				t.Errorf("CrossOracleAgreement = %d below the trust floor, want 0", s.Factors.CrossOracleAgreement)
			}
		})
	}
}

// Same Class:Subclass sources collapse to one bucket; unknown sources share
// the registry's default (exchange, no subclass).
func TestDistinctSourceClassCount(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()

	cases := []struct {
		name    string
		sources []string
		want    int
	}{
		{"empty", nil, 0},
		{"two CEXes", []string{"binance", "coinbase"}, 1},
		{"CEX + DEX", []string{"binance", "soroswap"}, 2},
		{"CEX + DEX + FX", []string{"binance", "soroswap", "exchangeratesapi"}, 3},
		{"CEX + Oracle", []string{"binance", "reflector-dex"}, 2},
		{"four buckets", []string{"binance", "reflector-dex", "coingecko", "ecb"}, 4},
		{"unknown alone", []string{"unknown_source"}, 1},
		{"two unknowns", []string{"unknown_a", "unknown_b"}, 1},
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

// A Phase 2 freeze (z >> 5, one source, collapsed confidence) bails before
// the VWAP write: the last-known-good value stays and no confidence is cached.
func TestPhase2Freeze_BlocksVWAPPublish(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()
	store := &mockStore{}
	rdb, _ := newTestRedis(t)
	// Tight baseline so any meaningful return reads as a huge z.
	orch := New(store, rdb, Config{
		Pairs:    []canonical.Pair{pair},
		Windows:  []time.Duration{time.Minute},
		Interval: time.Hour,
		Baselines: stubBaselineSource{
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0.0, MAD: 0.0001, N: 1000}},
			computedAt: now,
		},
	})
	ctx := context.Background()
	vwapKey := cachekeys.VWAP(pair.Base, pair.Quote, time.Minute)

	// Tick 1 warms prevVWAP; no comparator yet, so no freeze.
	store.trades = []canonical.Trade{makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_000_000, now.Add(-90*time.Second))}
	if err := orch.Tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	tick1Value, err := rdb.Get(ctx, vwapKey.String()).Result()
	if err != nil {
		t.Fatalf("VWAP key missing after tick 1: %v", err)
	}

	// Tick 2: 5x jump, single source.
	store.trades = []canonical.Trade{makeXLMUSDTrade(t, "soroswap", 1_000_000, 5_000_000, now.Add(-30*time.Second))}
	nextBucket(orch)
	if err := orch.Tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	tick2Value, err := rdb.Get(ctx, vwapKey.String()).Result()
	if err != nil {
		t.Fatalf("read tick-2 VWAP: %v", err)
	}
	if tick2Value != tick1Value {
		t.Errorf("VWAP cache changed across a Phase 2 freeze: tick1=%q tick2=%q (Phase 2 should preserve LKG)",
			tick1Value, tick2Value)
	}
	// Tick 1 had no prev, so confidence was skipped there too: no entry at all.
	confKey := cachekeys.Confidence(pair.Base, pair.Quote, time.Minute)
	if exists, _ := rdb.Exists(ctx, confKey.String()).Result(); exists != 0 {
		t.Error("confidence cache key present after Phase 2 freeze: should not have been written")
	}
}

// Bucket USD volume scales each quote by its SOURCE's decimals (CEX 8dp, FX
// 6dp, on-chain 7dp), not a fixed divisor.
func TestApproxUSDVolume_PerSourceDecimals(t *testing.T) {
	pair := xlmUSDPair(t)
	ts := time.Now().UTC()
	const usd = 30_000
	trade := func(source string, scale int64) canonical.Trade {
		return makeXLMUSDTrade(t, source, 1_000_000, usd*scale, ts)
	}
	cases := []struct {
		name   string
		trades []canonical.Trade
		want   float64
	}{
		{"binance 8dp", []canonical.Trade{trade("binance", 100_000_000)}, usd},
		{"massive 6dp", []canonical.Trade{trade("massive", 1_000_000)}, usd},
		{"soroswap 7dp", []canonical.Trade{trade("soroswap", 10_000_000)}, usd},
		{"aquarius 7dp", []canonical.Trade{trade("aquarius", 10_000_000)}, usd},
		{"phoenix 7dp", []canonical.Trade{trade("phoenix", 10_000_000)}, usd},
		{"comet 7dp", []canonical.Trade{trade("comet", 10_000_000)}, usd},
		{"sdex 7dp", []canonical.Trade{trade("sdex", 10_000_000)}, usd},
		{"8dp + 6dp", []canonical.Trade{trade("binance", 100_000_000), trade("massive", 1_000_000)}, 2 * usd},
		{"7dp + 8dp + 6dp", []canonical.Trade{
			trade("soroswap", 10_000_000), trade("binance", 100_000_000), trade("massive", 1_000_000),
		}, 3 * usd},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := approxUSDVolume(tc.trades, pair); math.Abs(got-tc.want) > 1.0 {
				t.Fatalf("approxUSDVolume = %.2f, want ~%.0f (a wrong power of ten reads 10x off)", got, tc.want)
			}
		})
	}
}

// A fixed 1e7 divisor overstates a CEX quote 10x, and LiquidityFactor is
// log-linear in its band, so that swings the factor by ln(10)/ln(1e6/1e3) = 1/3.
func TestApproxUSDVolume_CEXQuoteSwingOnFactor(t *testing.T) {
	const usdWhole = 30_000
	trades := []canonical.Trade{makeXLMUSDTrade(t, "binance", 1_000_000, int64(usdWhole)*100_000_000, time.Now().UTC())}
	got := approxUSDVolume(trades, xlmUSDPair(t))
	if math.Abs(got-float64(usdWhole)) > 1.0 {
		t.Fatalf("approxUSDVolume = %.2f, want ~%d (8dp CEX scale, not 10x that)", got, usdWhole)
	}

	corrected := confidence.LiquidityFactor(got)                // F(30000)
	buggy := confidence.LiquidityFactor(float64(usdWhole) * 10) // what /1e7 produced
	if math.Abs(corrected-0.492376) > 1e-5 {
		t.Errorf("LiquidityFactor(30000) = %.6f, want ~0.492376 (= ln(30)/ln(1000))", corrected)
	}
	if math.Abs(buggy-0.825709) > 1e-5 {
		t.Errorf("pre-fix LiquidityFactor(300000) = %.6f, want ~0.825709 (= ln(300)/ln(1000))", buggy)
	}
	if math.Abs((buggy-corrected)-1.0/3.0) > 1e-9 {
		t.Errorf("factor swing = %.6f, want exactly 1/3 (one decade of a [1e3, 1e6] log band)", buggy-corrected)
	}
	if buggy-corrected < 0.25 {
		t.Errorf("factor swing = %.4f, want a material (>0.25) gap the fix removes", buggy-corrected)
	}
}

// XLM/EUR volume cannot be valued in dollars without an FX rate, so it must
// report "unmeasured", not "$0": a zero liquidity factor would pin confidence
// at 0 and the Phase 2 freeze's confidence leg permanently true.
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
		t.Fatalf("approxUSDVolume(XLM/EUR) = %.2f, want the %.0f unmeasured sentinel", got, confidence.LiquidityUnmeasured)
	}

	// A healthy non-USD bucket must score above zero and clear the freeze's
	// confidence threshold, so that leg can still disagree.
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
	if score.Confidence < DefaultPhase2ConfidenceMaxFreeze {
		t.Errorf("healthy non-USD-quoted bucket scored %v, below the %v freeze threshold",
			score.Confidence, DefaultPhase2ConfidenceMaxFreeze)
	}
}

// minuteSeries builds a 1-minute (vwap, bucket_end) series over the 30 days
// ending at now, keeping minutes where keep is true (nil keeps all). It is
// the densest shape prices_1m can hold, fed through the refresher's
// SplitByLookback -> NewMultiBaseline path.
func minuteSeries(now time.Time, keep func(minute int) bool) []baseline.TimedVWAP {
	const minutesIn30d = 30 * 24 * 60
	start := now.Add(-baseline.Window30d)
	out := make([]baseline.TimedVWAP, 0, minutesIn30d)
	for i := 0; i < minutesIn30d; i++ {
		if keep != nil && !keep(i) {
			continue
		}
		out = append(out, baseline.TimedVWAP{
			// Nonzero and not flat, so every kept minute contributes a return.
			VWAP:      100 + float64(i%11)*0.01,
			BucketEnd: start.Add(time.Duration(i+1) * time.Minute),
		})
	}
	return out
}

// scoreAtDensity scores healthy-bucket inputs at the given baseline density.
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

// uncappedGeoMean is what Compute serves when the bootstrap cap does not
// bind: all six weights are 1.0 for these inputs (triangulation excluded).
func uncappedGeoMean(f confidence.Factors) float64 {
	product := f.ZScore * f.SourceCount * f.Diversity *
		f.Liquidity * f.CrossOracle * f.BaselineQuality
	return math.Pow(product, 1.0/6.0)
}

// The bootstrap cap keys on sample DENSITY, not calendar age: a densely
// observed 30d window releases it, a calendar-mature but sparse one must not.
func TestBaselineAgeDays_DensityGatesBootstrapCap(t *testing.T) {
	cases := []struct {
		name    string
		keep    func(minute int) bool
		wantN   int // 0 = not asserted
		wantAge float64
		ageTol  float64
		capped  bool
	}{
		// N counts returns, so 43,200 buckets give 43,199; the age must still
		// read exactly 30 or the cap is unreachable and confidence pins at 0.5.
		{"maximal density", nil, 43_199, 30.0, 0, false},
		// Drop one minute in 125 -> 42,854 buckets, the ~99.2% the majors show on r1.
		{"production density", func(m int) bool { return m%125 != 0 }, 0, 42_854.0 / 1440.0, 1e-9, false},
		// 200 traded minutes a day -> 6,000 buckets: thin baseline, stays capped.
		{"sparse mature pair", func(m int) bool { return m%1440 < 200 }, 0, 6000.0 / 1440.0, 1e-9, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			multi := baseline.NewMultiBaseline(baseline.SplitByLookback(minuteSeries(now, tc.keep), now))
			if multi.Day30 == nil {
				t.Fatal("30d window in bootstrap")
			}
			if tc.wantN != 0 && multi.Day30.N != tc.wantN {
				t.Fatalf("Day30.N = %d, want %d", multi.Day30.N, tc.wantN)
			}
			age := baselineAgeDays(multi)
			if math.Abs(age-tc.wantAge) > tc.ageTol {
				t.Errorf("baselineAgeDays = %.8f, want %.8f", age, tc.wantAge)
			}

			got := scoreAtDensity(age)
			if tc.capped {
				if got.Confidence != confidence.BootstrapConfidenceCap {
					t.Errorf("confidence = %v, want the %v bootstrap cap (density, not calendar age, is the gate)",
						got.Confidence, confidence.BootstrapConfidenceCap)
				}
				return
			}
			want := uncappedGeoMean(got.Factors)
			if math.Abs(got.Confidence-want) > 1e-12 {
				t.Errorf("confidence = %.17f, want the uncapped geometric mean %.17f (delta %g)",
					got.Confidence, want, got.Confidence-want)
			}
			if got.Confidence <= confidence.BootstrapConfidenceCap {
				t.Errorf("confidence = %v <= the %v bootstrap cap", got.Confidence, confidence.BootstrapConfidenceCap)
			}
		})
	}
}

// gaugeValue reads one labelled gauge series from obs.Registry; ok is false
// when it is not exported.
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

// Past the one-day ceiling a baseline reads as bootstrap (outcome
// baseline_stale); its age is exported per pair either way.
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

// ADR-0019: a frozen price still carries a confidence. The LKG bucket's score
// must outlive its window-length TTL for as long as the held value does, and
// stay the LKG's score, never the refused bucket's.
func TestFreezeHold_ConfidenceSurvivesEveryWindowOfTheHold(t *testing.T) {
	pair := xlmUSDPair(t)
	window := time.Minute
	now := time.Now().UTC()
	store := &mockStore{}
	cache, mr := newTestRedis(t)
	o := New(store, cache, Config{
		Pairs:        []canonical.Pair{pair},
		Windows:      []time.Duration{window},
		Interval:     time.Hour,
		FreezeWriter: &recordingFreezeMarker{},
		Baselines: stubBaselineSource{
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: maxDay30Returns}},
			computedAt: now,
		},
	})
	ctx := context.Background()
	confKey := cachekeys.Confidence(pair.Base, pair.Quote, window).String()
	vwapKey := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	tick := func(quote int64, age time.Duration) {
		t.Helper()
		store.trades = []canonical.Trade{makeTradeOn(t, pair, "soroswap", 100_000_000, quote, now.Add(-age))}
		nextBucket(o)
		if err := o.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}

	// Two calm buckets at 0.08: the second is scored and caches its score.
	tick(8_000_000, 40*time.Second)
	tick(8_000_000, 30*time.Second)
	lkgScore, err := mr.Get(confKey)
	if err != nil || lkgScore == "" {
		t.Fatalf("no confidence cached for the published LKG bucket (err %v) — fixture broken", err)
	}
	// A +50% single-venue bucket: z≈50, the Phase 2 freeze refuses it.
	tick(12_000_000, 10*time.Second)
	if st := o.freezeStates[pair.String()+":"+window.String()]; !st.Active() {
		t.Fatal("freeze did not engage on the z≈50 bucket — fixture broken")
	}

	for i := 1; i <= 3; i++ {
		mr.FastForward(window)
		if _, err := mr.Get(vwapKey); err != nil {
			t.Fatalf("%d window(s) into the hold: held value gone (%v) — fixture broken", i, err)
		}
		got, err := mr.Get(confKey)
		if err != nil {
			t.Fatalf("%d window(s) into the hold: confidence key gone (%v) — /v1/price would serve "+
				"flags.frozen with no confidence", i, err)
		}
		if got != lkgScore {
			t.Fatalf("%d window(s) into the hold: confidence = %s, want the LKG's own score %s", i, got, lkgScore)
		}
	}
}

package orchestrator

import (
	"context"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Regression for the outlier-trim drift artifact (r1:
// `stellarindex_aggregator_outlier_storm` for hours + an
// `anomaly_freeze_engaged` on crypto:XLM/fiat:GBP after Kraken stepped
// 0.1337 → 0.1364, a GENUINE +2% that matched the XLM/USD × GBP/USD
// cross). The whole-window median+MAD filter trimmed every post-step
// print — the band is the majority regime's dispersion — so the
// published window VWAP stayed pinned at the stale level and the drop
// counter re-counted the tail every tick.
//
// Replays that shape through refreshPairWindow on a THIN, SINGLE-SOURCE
// series (one Kraken print per minute) and asserts the orchestrator
// (a) drops nothing, (b) publishes a VWAP that carries the step, and
// (c) publishes the per-venue / per-stage window gauges the redesigned
// alert reads. A sibling case proves a lone +10% print on the same
// series IS still dropped.

func thinStepFixture(t *testing.T, now time.Time, totalMin, tailMin int) []canonical.Trade {
	t.Helper()
	trades := make([]canonical.Trade, 0, totalMin)
	for i := 0; i < totalMin; i++ {
		centre := int64(13_370_000) // 0.1337 at 10^8 quote / 10^8 base
		if i >= totalMin-tailMin {
			centre = 13_640_000 // +2.02%
		}
		noise := int64((i*7919)%21-10) * (centre / 10_000) // ±0.1%
		ts := now.Add(-time.Duration(totalMin-i) * time.Minute)
		trades = append(trades, buildTradeFrom(t, "kraken",
			big.NewInt(100_000_000), big.NewInt(centre+noise), ts))
	}
	return trades
}

func TestRefreshPairWindow_GenuineStepOnThinSingleSourceSeriesIsNotTrimmed(t *testing.T) {
	now := time.Now()
	store := &mockStore{trades: thinStepFixture(t, now, 300, 40)}
	rdb, mr := newTestRedis(t)
	pair := xlmUsdtPair(t)
	window := 6 * time.Hour
	orch := New(store, rdb, Config{
		Pairs:                 []canonical.Pair{pair},
		Windows:               []time.Duration{window},
		OutlierSigmaThreshold: 4.0, // the shipped default
	})

	before := testutil.ToFloat64(obs.AggregatorDroppedTradesTotal.WithLabelValues("outlier", pair.String()))
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := testutil.ToFloat64(obs.AggregatorDroppedTradesTotal.WithLabelValues("outlier", pair.String())) - before; got != 0 {
		t.Errorf("dropped{outlier} delta = %v, want 0 — an agreed +2%% step is not an outlier", got)
	}

	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	key := "vwap:" + xlm.String() + ":" + usdt.String() + ":" + strconv.Itoa(int(window.Seconds()))
	val, err := mr.Get(key)
	if err != nil {
		t.Fatalf("miniredis Get %q: %v", key, err)
	}
	vwap, err := strconv.ParseFloat(val, 64)
	if err != nil {
		t.Fatalf("cached VWAP %q: %v", val, err)
	}
	// 260 prints at 0.1337 + 40 at 0.1364 → equal-volume VWAP ≈ 0.13406.
	// The trimmed (defective) window publishes ≈ 0.1337 — pinned at
	// the stale level with the whole step erased.
	if vwap < 0.1339 || vwap > 0.1342 {
		t.Errorf("published VWAP = %v, want ≈0.13406 (the step carried); ≈0.1337 means the tail was trimmed", vwap)
	}

	// The redesigned alert inputs: one venue series at the pre-filter
	// VWAP, and the per-stage window counts with nothing trimmed.
	venue := testutil.ToFloat64(obs.AggregatorVenueVWAP.WithLabelValues(pair.String(), "6h", "kraken"))
	if venue < 0.1339 || venue > 0.1342 {
		t.Errorf("venue_vwap{kraken} = %v, want ≈0.13406", venue)
	}
	for _, stage := range []string{"fetched", "class", "outlier"} {
		if got := testutil.ToFloat64(obs.AggregatorWindowTrades.WithLabelValues(pair.String(), "6h", stage)); got != 300 {
			t.Errorf("window_trades{stage=%s} = %v, want 300", stage, got)
		}
	}
}

func TestRefreshPairWindow_SinglePrintSpikeOnThinSeriesIsStillTrimmed(t *testing.T) {
	now := time.Now()
	trades := thinStepFixture(t, now, 300, 0)
	trades[150] = buildTradeFrom(t, "kraken",
		big.NewInt(100_000_000), big.NewInt(14_707_000), trades[150].Timestamp) // +10% lone print
	store := &mockStore{trades: trades}
	rdb, _ := newTestRedis(t)
	pair := xlmUsdtPair(t)
	window := 6 * time.Hour
	orch := New(store, rdb, Config{
		Pairs:                 []canonical.Pair{pair},
		Windows:               []time.Duration{window},
		OutlierSigmaThreshold: 4.0,
	})
	before := testutil.ToFloat64(obs.AggregatorDroppedTradesTotal.WithLabelValues("outlier", pair.String()))
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := testutil.ToFloat64(obs.AggregatorDroppedTradesTotal.WithLabelValues("outlier", pair.String())) - before; got != 1 {
		t.Errorf("dropped{outlier} delta = %v, want 1 (exactly the lone +10%% print)", got)
	}
	if got := testutil.ToFloat64(obs.AggregatorWindowTrades.WithLabelValues(pair.String(), "6h", "outlier")); got != 299 {
		t.Errorf("window_trades{stage=outlier} = %v, want 299", got)
	}
}

// A stored zero-leg trade (an SDEX rounding fill) has no price: it is
// counted as unpriceable, never as an outlier or as a class-stage
// survivor the trim-fraction ratio would read as trimmed.
func TestRefreshPairWindow_ZeroLegTradeIsUnpriceableNotOutlier(t *testing.T) {
	now := time.Now()
	trades := thinStepFixture(t, now, 300, 0)
	trades = append(trades, buildTradeFrom(t, "kraken",
		big.NewInt(100*100_000_000), big.NewInt(0), now.Add(-30*time.Second)))
	store := &mockStore{trades: trades}
	rdb, mr := newTestRedis(t)
	pair := xlmUsdtPair(t)
	window := 6 * time.Hour
	orch := New(store, rdb, Config{
		Pairs:                 []canonical.Pair{pair},
		Windows:               []time.Duration{window},
		OutlierSigmaThreshold: 4.0,
	})
	counter := func(reason string) float64 {
		return testutil.ToFloat64(obs.AggregatorDroppedTradesTotal.WithLabelValues(reason, pair.String()))
	}
	beforeUnpriceable, beforeOutlier := counter("unpriceable"), counter("outlier")
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := counter("unpriceable") - beforeUnpriceable; got != 1 {
		t.Errorf("dropped{unpriceable} delta = %v, want 1", got)
	}
	if got := counter("outlier") - beforeOutlier; got != 0 {
		t.Errorf("dropped{outlier} delta = %v, want 0 — a zero-leg trade is not an outlier", got)
	}
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	val, err := mr.Get("vwap:" + xlm.String() + ":" + usdt.String() + ":" + strconv.Itoa(int(window.Seconds())))
	if err != nil {
		t.Fatalf("miniredis Get: %v", err)
	}
	if vwap, err := strconv.ParseFloat(val, 64); err != nil || vwap < 0.1335 || vwap > 0.1339 {
		t.Errorf("published VWAP = %q (err %v), want ≈0.1337", val, err)
	}
	if venue := testutil.ToFloat64(obs.AggregatorVenueVWAP.WithLabelValues(pair.String(), "6h", "kraken")); venue < 0.1335 || venue > 0.1339 {
		t.Errorf("venue_vwap{kraken} = %v, want ≈0.1337", venue)
	}
	for _, stage := range []string{"class", "outlier"} {
		if got := testutil.ToFloat64(obs.AggregatorWindowTrades.WithLabelValues(pair.String(), "6h", stage)); got != 300 {
			t.Errorf("window_trades{stage=%s} = %v, want 300", stage, got)
		}
	}
}

func TestRecordVenueVWAPs_DeletesAbsentSources(t *testing.T) {
	// A venue that leaves the window must not keep a stale level in
	// the max/min disagreement ratio.
	now := time.Now()
	pair := xlmUsdtPair(t)
	rdb, _ := newTestRedis(t)
	orch := New(&mockStore{}, rdb, Config{Pairs: []canonical.Pair{pair}})
	both := []canonical.Trade{
		buildTradeFrom(t, "binance", big.NewInt(100_000_000), big.NewInt(20_000_000), now),
		buildTradeFrom(t, "kraken", big.NewInt(100_000_000), big.NewInt(21_000_000), now),
	}
	orch.recordVenueVWAPs(pair, 5*time.Minute, both)
	if n := testutil.CollectAndCount(obs.AggregatorVenueVWAP, "stellarindex_aggregator_venue_vwap"); n < 2 {
		t.Fatalf("expected ≥2 venue series after first refresh, got %d", n)
	}
	orch.recordVenueVWAPs(pair, 5*time.Minute, both[:1])
	got, err := obs.AggregatorVenueVWAP.GetMetricWithLabelValues(pair.String(), "5m", "kraken")
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	// GetMetricWith re-creates the child at 0; a stale 0.21 here means
	// the absent venue's series survived the refresh.
	if v := testutil.ToFloat64(got); v != 0 {
		t.Errorf("absent venue kraken still reports %v after it left the window; want its series deleted", v)
	}
	if v := testutil.ToFloat64(obs.AggregatorVenueVWAP.WithLabelValues(pair.String(), "5m", "binance")); v != 0.2 {
		t.Errorf("binance venue_vwap = %v, want 0.2", v)
	}
	if got := windowLabel(24 * time.Hour); got != "24h" {
		t.Errorf("windowLabel(24h) = %q", got)
	}
	if got := windowLabel(90 * time.Second); got != "90s" {
		t.Errorf("windowLabel(90s) = %q", got)
	}
}

// The outlier centre is a per-print median, so the trade-count trim share
// cannot tell a trimmed honest block from trimmed dust. window_base_volume
// publishes each stage's base volume in whole units on every window, so the
// volume trim share is readable wherever the filter runs.
func TestRefreshPairWindow_RecordsWindowBaseVolumePerStage(t *testing.T) {
	t0 := time.Now().Add(-4 * time.Minute)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	cases := []struct {
		name                string
		trades              []canonical.Trade
		wantClass, wantKept float64
	}{
		{
			// 3 × 1,000,000 XLM at 0.100 against 4 × 30,000 XLM of wash at
			// 0.114: the count majority would drop the honest block, so the
			// window is withheld and the whole class volume reads as removed.
			name: "dust count majority over an honest block",
			trades: []canonical.Trade{
				wholeXLMTrade(t, "kraken", 1_000_000, 100, 1000, at(0)),
				wholeXLMTrade(t, "kraken", 30_000, 114, 1000, at(5)),
				wholeXLMTrade(t, "kraken", 1_000_000, 100, 1000, at(10)),
				wholeXLMTrade(t, "kraken", 30_000, 114, 1000, at(15)),
				wholeXLMTrade(t, "kraken", 1_000_000, 100, 1000, at(20)),
				wholeXLMTrade(t, "kraken", 30_000, 114, 1000, at(25)),
				wholeXLMTrade(t, "kraken", 30_000, 114, 1000, at(35)),
			},
			wantClass: 3_120_000,
			wantKept:  0,
		},
		{
			// A lone 8dp fat finger among 7dp honest prints is trimmed. Both
			// stages are in whole units, so the all-7dp survivors compare to
			// the 8dp-bearing class set without a 10x scale error.
			name: "fat finger trimmed across mixed scales",
			trades: []canonical.Trade{
				wholeXLMTrade(t, "sdex", 1_000_000, 100, 1000, at(0)),
				wholeXLMTrade(t, "sdex", 1_000_000, 100, 1000, at(1)),
				wholeXLMTrade(t, "sdex", 1_000_000, 100, 1000, at(2)),
				wholeXLMTrade(t, "sdex", 1_000_000, 100, 1000, at(3)),
				wholeXLMTrade(t, "sdex", 1_000_000, 100, 1000, at(4)),
				wholeXLMTrade(t, "kraken", 10_000, 200, 1000, at(6)),
			},
			wantClass: 5_010_000,
			wantKept:  5_000_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pair := xlmUsdtPair(t)
			orch, _ := newOrch5m(t, &mockStore{trades: tc.trades}, pair, Config{OutlierSigmaThreshold: 4.0})
			tickOnce(t, orch)
			class := testutil.ToFloat64(obs.AggregatorWindowBaseVolume.WithLabelValues(pair.String(), "5m", "class"))
			kept := testutil.ToFloat64(obs.AggregatorWindowBaseVolume.WithLabelValues(pair.String(), "5m", "outlier"))
			if class != tc.wantClass || kept != tc.wantKept {
				t.Fatalf("window_base_volume class=%v outlier=%v, want class=%v outlier=%v", class, kept, tc.wantClass, tc.wantKept)
			}
		})
	}
}

// TestRefreshPairWindow_DirectWriteClearsStaleProvenance is
// W1-flow-price-serve-2: when the direct per-pair refresh overwrites the
// shared VWAP key with a DIRECT value, it must clear any stale
// "triangulated" provenance a prior tick's composite left, so
// LookupTriangulatedVWAP cannot serve the thin direct price mislabeled as
// a robust composite.
func TestRefreshPairWindow_DirectWriteClearsStaleProvenance(t *testing.T) {
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000), time.Now().Add(-2*time.Minute)),
			buildTrade(t, big.NewInt(20_000_000_000), big.NewInt(3_518_000_000), time.Now().Add(-1*time.Minute)),
		},
	}
	rdb, mr := newTestRedis(t)
	window := 5 * time.Minute
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")

	// A PRIOR tick's composite left a stale triangulated provenance marker.
	provKey := cachekeys.VWAPProvenance(xlm, usdt, window).String()
	if err := rdb.Set(context.Background(), provKey, cachekeys.VWAPProvenanceTriangulated, window).Err(); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}

	o := New(store, rdb, Config{Pairs: []canonical.Pair{xlmUsdtPair(t)}, Windows: []time.Duration{window}})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The direct value was published...
	if !mr.Exists("vwap:" + xlm.String() + ":" + usdt.String() + ":300") {
		t.Fatal("direct VWAP not published — test precondition broken")
	}
	// ...so the stale triangulated marker must be gone.
	if mr.Exists(provKey) {
		v, _ := mr.Get(provKey)
		t.Errorf("stale provenance marker survived a direct refresh (=%q) — a thin direct price "+
			"would be served flagged triangulated (W1-flow-price-serve-2)", v)
	}
}

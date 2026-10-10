package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// mockStore is a hand-controlled Store for deterministic tick tests.
type mockStore struct {
	// trades is returned for every call, regardless of (pair, from, to).
	trades []canonical.Trade
	// perPair, when set, overrides trades keyed by canonical.Pair.String();
	// a missing pair returns an empty slice, not an error.
	perPair map[string][]canonical.Trade
	// perPairErr injects a fetch error for one pair.
	perPairErr map[string]error
	// returnErr, if set, is returned from every TradesInRange call.
	returnErr   error
	calls       int
	callsByPair map[string]int
	// lastLimit is the row cap passed on the most recent call.
	lastLimit int
}

func (m *mockStore) TradesInRange(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	m.calls++
	m.lastLimit = limit
	if m.callsByPair == nil {
		m.callsByPair = make(map[string]int)
	}
	m.callsByPair[p.String()]++
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	if m.perPairErr != nil {
		if err, ok := m.perPairErr[p.String()]; ok {
			return nil, err
		}
	}
	if m.perPair != nil {
		return newestN(m.perPair[p.String()], limit), nil
	}
	return newestN(m.trades, limit), nil
}

// newestN mirrors the producer's LIMIT: when a window holds more than
// limit trades it returns only the newest limit, in ascending time order.
func newestN(trades []canonical.Trade, limit int) []canonical.Trade {
	if limit <= 0 || len(trades) <= limit {
		return trades
	}
	sorted := slices.Clone(trades)
	slices.SortStableFunc(sorted, func(a, b canonical.Trade) int { return a.Timestamp.Compare(b.Timestamp) })
	return sorted[len(sorted)-limit:]
}

// newTestRedis spins up a miniredis + go-redis client.
func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// buildTrade is a binance XLM/USDT trade at the given raw amounts.
func buildTrade(t *testing.T, base, quote *big.Int, ts time.Time) canonical.Trade {
	t.Helper()
	return buildTradeFrom(t, "binance", base, quote, ts)
}

// buildTradeFrom is buildTrade with an explicit Source.
func buildTradeFrom(t *testing.T, source string, base, quote *big.Int, ts time.Time) canonical.Trade {
	t.Helper()
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	pair, _ := canonical.NewPair(xlm, usdt)
	return canonical.Trade{
		Source:      source,
		Ledger:      0,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}
}

func xlmUsdtPair(t *testing.T) canonical.Pair {
	t.Helper()
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	p, _ := canonical.NewPair(xlm, usdt)
	return p
}

// vwapKey5m spells the 5m VWAP cache key out by hand so every test also
// pins the vwap:<base>:<quote>:<window-seconds> shape.
func vwapKey5m(p canonical.Pair) string {
	return "vwap:" + p.Base.String() + ":" + p.Quote.String() + ":300"
}

// newOrch5m builds an orchestrator for pair over store; cfg.Windows
// defaults to 5m.
func newOrch5m(t *testing.T, store Store, pair canonical.Pair, cfg Config) (*Orchestrator, *miniredis.Miniredis) {
	t.Helper()
	rdb, mr := newTestRedis(t)
	cfg.Pairs = []canonical.Pair{pair}
	if cfg.Windows == nil {
		cfg.Windows = []time.Duration{5 * time.Minute}
	}
	return New(store, rdb, cfg), mr
}

func tickOnce(t *testing.T, o *Orchestrator) {
	t.Helper()
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// assertPublished checks both the write counter and the cache key.
func assertPublished(t *testing.T, o *Orchestrator, mr *miniredis.Miniredis, key string, want bool) {
	t.Helper()
	var wantWrites int64
	if want {
		wantWrites = 1
	}
	if got := o.Stats().VWAPWrites; got != wantWrites {
		t.Errorf("VWAPWrites = %d, want %d", got, wantWrites)
	}
	if got := mr.Exists(key); got != want {
		t.Errorf("key %q exists = %v, want %v", key, got, want)
	}
}

// minUSDDrops ticks o in a fresh bucket and returns the
// dropped_windows{min_usd_volume} delta.
func minUSDDrops(t *testing.T, o *Orchestrator) float64 {
	t.Helper()
	c := obs.AggregatorDroppedWindowsTotal.WithLabelValues("min_usd_volume")
	before := testutil.ToFloat64(c)
	nextBucket(o)
	tickOnce(t, o)
	return testutil.ToFloat64(c) - before
}

func TestTick_PublishesVWAP(t *testing.T) {
	now := time.Now()
	one := big.NewInt(100_000_000)
	for _, tc := range []struct {
		name   string
		trades []canonical.Trade
		cfg    Config
		want   string
	}{
		{
			name: "volume-weighted mean",
			trades: []canonical.Trade{
				buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000), now.Add(-2*time.Minute)),
				buildTrade(t, big.NewInt(20_000_000_000), big.NewInt(3_518_000_000), now.Add(-time.Minute)),
			},
			want: "0.175873333333",
		},
		{
			// A mixed-class VWAP would land near 5.07.
			name: "default class filter drops aggregator and oracle rows",
			trades: []canonical.Trade{
				buildTradeFrom(t, "binance", one, big.NewInt(20_000_000), now.Add(-2*time.Minute)),
				buildTradeFrom(t, "coingecko", one, big.NewInt(1_000_000_000), now.Add(-time.Minute)),
				buildTradeFrom(t, "reflector-dex", one, big.NewInt(500_000_000), now.Add(-30*time.Second)),
			},
			want: "0.200000000000",
		},
		{
			name: "DisableClassFilter includes every row",
			trades: []canonical.Trade{
				buildTradeFrom(t, "binance", one, big.NewInt(20_000_000), now.Add(-2*time.Minute)),
				buildTradeFrom(t, "coingecko", one, big.NewInt(1_000_000_000), now.Add(-time.Minute)),
				buildTradeFrom(t, "reflector-dex", one, big.NewInt(500_000_000), now.Add(-30*time.Second)),
			},
			cfg:  Config{DisableClassFilter: true},
			want: "5.066666666666",
		},
		{
			name: "zero outlier sigma leaves the outlier in",
			trades: []canonical.Trade{
				buildTradeFrom(t, "binance", one, big.NewInt(20_000_000), now.Add(-3*time.Minute)),
				buildTradeFrom(t, "binance", one, big.NewInt(20_000_000), now.Add(-2*time.Minute)),
				buildTradeFrom(t, "kraken", one, big.NewInt(20_000_000_000), now.Add(-time.Minute)),
			},
			want: "66.800000000000",
		},
		{
			// Router rows mix realized and limit amounts; seeded at 100.0 so
			// any contribution shows.
			name: "soroswap-router row never contributes",
			trades: []canonical.Trade{
				buildTradeFrom(t, "binance", one, big.NewInt(20_000_000), now.Add(-time.Minute)),
				buildTradeFrom(t, "soroswap-router", one, big.NewInt(10_000_000_000), now.Add(-30*time.Second)),
			},
			want: "0.200000000000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockStore{trades: tc.trades}
			pair := xlmUsdtPair(t)
			orch, mr := newOrch5m(t, store, pair, tc.cfg)
			tickOnce(t, orch)
			if store.calls != 1 {
				t.Errorf("store.calls = %d want 1", store.calls)
			}
			if got := orch.Stats().VWAPWrites; got != 1 {
				t.Errorf("VWAPWrites = %d want 1", got)
			}
			got, err := mr.Get(vwapKey5m(pair))
			if err != nil {
				t.Fatalf("miniredis Get: %v", err)
			}
			if got != tc.want {
				t.Errorf("VWAP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTick_EmptyWindowSkipsWrite(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		trades []canonical.Trade
	}{
		{"no trades", nil},
		{"every row off-class", []canonical.Trade{
			buildTradeFrom(t, "coingecko", big.NewInt(100_000_000), big.NewInt(20_000_000), now.Add(-time.Minute)),
			buildTradeFrom(t, "reflector-dex", big.NewInt(100_000_000), big.NewInt(20_000_000), now.Add(-30*time.Second)),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := xlmUsdtPair(t)
			orch, mr := newOrch5m(t, &mockStore{trades: tc.trades}, pair, Config{})
			tickOnce(t, orch)
			if mr.Exists(vwapKey5m(pair)) {
				t.Errorf("key %q should not exist after an empty window", vwapKey5m(pair))
			}
			if got := orch.Stats().EmptyWindows; got != 1 {
				t.Errorf("EmptyWindows = %d want 1", got)
			}
		})
	}
}

// A store outage is counted per pair and as a tick error, never as an
// empty window, and never aborts the tick.
func TestTick_StoreErrorIsCountedNotFatal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pair  func(*testing.T) canonical.Pair
		proxy bool
	}{
		{"direct pair, proxy off", xlmUsdtPair, false},
		{"fiat target expands to backers", xlmUsdFiatPair, true},
		{"non-fiat target has only the direct leg", xlmUsdtPair, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockStore{returnErr: context.DeadlineExceeded}
			orch, _ := newOrch5m(t, store, tc.pair(t), Config{EnableStablecoinFiatProxy: tc.proxy})
			beforeErr := testutil.ToFloat64(obs.AggregatorTicksTotal.WithLabelValues("error"))
			beforeEmpty := testutil.ToFloat64(obs.AggregatorEmptyWindowsTotal)
			tickOnce(t, orch)
			if store.calls == 0 {
				t.Fatal("store was never queried")
			}
			if got := orch.Stats().Errors; got != 1 {
				t.Errorf("Errors = %d want 1", got)
			}
			if got := orch.Stats().VWAPWrites; got != 0 {
				t.Errorf("VWAPWrites = %d want 0", got)
			}
			if got := testutil.ToFloat64(obs.AggregatorTicksTotal.WithLabelValues("error")) - beforeErr; got != 1 {
				t.Errorf("ticks{error} delta = %v want 1", got)
			}
			if got := testutil.ToFloat64(obs.AggregatorEmptyWindowsTotal) - beforeEmpty; got != 0 {
				t.Errorf("empty_windows delta = %v want 0 (an outage is not an empty window)", got)
			}
		})
	}
}

func TestTick_MultipleWindows(t *testing.T) {
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(1_000_000_000), big.NewInt(175_820_000), time.Now().Add(-time.Minute)),
		},
	}
	pair := xlmUsdtPair(t)
	orch, mr := newOrch5m(t, store, pair, Config{
		Windows: []time.Duration{5 * time.Minute, 1 * time.Hour, 24 * time.Hour},
	})
	tickOnce(t, orch)
	for _, secs := range []int{300, 3600, 86400} {
		key := "vwap:" + pair.Base.String() + ":" + pair.Quote.String() + ":" + strconv.Itoa(secs)
		if !mr.Exists(key) {
			t.Errorf("expected key %q", key)
		}
	}
	if orch.Stats().VWAPWrites != 3 {
		t.Errorf("VWAPWrites = %d want 3", orch.Stats().VWAPWrites)
	}
}

func TestTick_NoPairsIsNoOp(t *testing.T) {
	store := &mockStore{}
	rdb, _ := newTestRedis(t)
	orch := New(store, rdb, Config{Pairs: nil})
	tickOnce(t, orch)
	if store.calls != 0 {
		t.Errorf("store.calls = %d want 0 (no pairs → no fetches)", store.calls)
	}
}

// Run ticks once before the ticker's first fire so a fresh aggregator
// has warm keys immediately.
func TestRun_FirstTickFiresImmediately(t *testing.T) {
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(1_000_000_000), big.NewInt(175_820_000), time.Now()),
		},
	}
	pair := xlmUsdtPair(t)
	orch, mr := newOrch5m(t, store, pair, Config{Interval: 5 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = orch.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	key := vwapKey5m(pair)
	for time.Now().Before(deadline) && !mr.Exists(key) {
		time.Sleep(10 * time.Millisecond)
	}
	if !mr.Exists(key) {
		t.Error("immediate tick did not write Redis key within 500ms")
	}

	cancel()
	<-done
}

// filterForVWAP keeps exchange-class rows only, in order, and returns a
// fresh slice rather than compacting over the caller's backing array.
func TestFilterForVWAP_KeepsExchangeClassOnly(t *testing.T) {
	now := time.Now()
	sources := []string{
		"binance",          // exchange ✓
		"coingecko",        // aggregator ✗
		"coinmarketcap",    // aggregator ✗
		"reflector-dex",    // oracle ✗
		"ecb",              // authority_sanity ✗
		"kraken",           // exchange ✓
		"unknown-venue",    // unregistered → IncludeInVWAP=false ✗
		"exchangeratesapi", // exchange ✓ (institutional FX)
		"soroswap-router",  // router ✗
		"soroswap",         // exchange ✓
	}
	trades := make([]canonical.Trade, len(sources))
	for i, src := range sources {
		trades[i] = buildTradeFrom(t, src, big.NewInt(int64(i+1)), big.NewInt(int64(i+1)), now)
	}

	got := filterForVWAP(trades)
	wantSources := []string{"binance", "kraken", "exchangeratesapi", "soroswap"}
	if len(got) != len(wantSources) {
		t.Fatalf("filterForVWAP: len=%d want %d (%v)", len(got), len(wantSources), got)
	}
	for i, src := range wantSources {
		if got[i].Source != src {
			t.Errorf("filterForVWAP[%d].Source = %q want %q", i, got[i].Source, src)
		}
	}
	for i, src := range sources {
		if trades[i].Source != src {
			t.Errorf("input[%d] mutated to %q, want %q", i, trades[i].Source, src)
		}
	}
}

// The outlier filter discards the 1000× row and attributes the drop to
// the configured target pair, the label the runbook's topk-by-pair reads.
func TestTick_OutlierFilter_DropsAnomalousRowUnderPairLabel(t *testing.T) {
	now := time.Now()
	one := big.NewInt(100_000_000)
	store := &mockStore{
		trades: []canonical.Trade{
			buildTradeFrom(t, "binance", one, big.NewInt(20_000_000), now.Add(-4*time.Minute)),
			buildTradeFrom(t, "binance", one, big.NewInt(20_000_000), now.Add(-3*time.Minute)),
			buildTradeFrom(t, "kraken", one, big.NewInt(20_000_000), now.Add(-2*time.Minute)),
			buildTradeFrom(t, "kraken", one, big.NewInt(20_000_000_000), now.Add(-time.Minute)),
		},
	}
	pair := xlmUsdtPair(t)
	orch, mr := newOrch5m(t, store, pair, Config{OutlierSigmaThreshold: 1.0})

	dropped := obs.AggregatorDroppedTradesTotal.WithLabelValues("outlier", pair.String())
	before := testutil.ToFloat64(dropped)
	tickOnce(t, orch)
	if got := testutil.ToFloat64(dropped) - before; got != 1 {
		t.Errorf("dropped{outlier,pair=%q} delta = %v want 1", pair.String(), got)
	}
	// Unfiltered this would be ≈50.15.
	got, err := mr.Get(vwapKey5m(pair))
	if err != nil {
		t.Fatalf("miniredis Get: %v", err)
	}
	if got != "0.200000000000" {
		t.Errorf("outlier-filtered VWAP = %q want 0.200000000000", got)
	}
}

func TestTick_EmitsPrometheusMetrics(t *testing.T) {
	now := time.Now()
	store := &mockStore{
		trades: []canonical.Trade{
			buildTradeFrom(t, "binance",
				big.NewInt(100_000_000), big.NewInt(20_000_000), now.Add(-2*time.Minute)),
			buildTradeFrom(t, "coingecko", // class=aggregator → dropped
				big.NewInt(100_000_000), big.NewInt(20_000_000), now.Add(-1*time.Minute)),
		},
	}
	pair := xlmUsdtPair(t)
	orch, _ := newOrch5m(t, store, pair, Config{})

	beforeOK := testutil.ToFloat64(obs.AggregatorTicksTotal.WithLabelValues("ok"))
	beforeWrites := testutil.ToFloat64(obs.AggregatorVWAPWritesTotal)
	// Drop counters carry the configured target pair.
	pairLabel := pair.String()
	beforeDroppedClass := testutil.ToFloat64(obs.AggregatorDroppedTradesTotal.WithLabelValues("class", pairLabel))

	tickOnce(t, orch)

	if got := testutil.ToFloat64(obs.AggregatorTicksTotal.WithLabelValues("ok")) - beforeOK; got != 1 {
		t.Errorf("ticks{ok} delta = %v want 1", got)
	}
	if got := testutil.ToFloat64(obs.AggregatorVWAPWritesTotal) - beforeWrites; got != 1 {
		t.Errorf("vwap_writes delta = %v want 1", got)
	}
	if got := testutil.ToFloat64(obs.AggregatorDroppedTradesTotal.WithLabelValues("class", pairLabel)) - beforeDroppedClass; got != 1 {
		t.Errorf("dropped{class,%s} delta = %v want 1 (coingecko row)", pairLabel, got)
	}
}

// xlmUsdFiatPair builds the XLM/fiat:USD target pair used by the
// stablecoin-expansion tests.
func xlmUsdFiatPair(t *testing.T) canonical.Pair {
	t.Helper()
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	p, _ := canonical.NewPair(xlm, usd)
	return p
}

// backerTrade builds a trade for XLM/<stable> at (base, quote)
// volumes with the given source.
func backerTrade(t *testing.T, stable, source string, base, quote *big.Int, ts time.Time) canonical.Trade {
	t.Helper()
	xlm, _ := canonical.NewCryptoAsset("XLM")
	stableAsset, _ := canonical.NewCryptoAsset(stable)
	pair, _ := canonical.NewPair(xlm, stableAsset)
	return canonical.Trade{
		Source:      source,
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}
}

// A fiat:USD target fetches the direct pair plus every stablecoin backer
// and collapses the backer legs onto the target key.
func TestTick_StablecoinExpansion_FetchesAllBackerPairsAndCollapses(t *testing.T) {
	now := time.Now()
	targetPair := xlmUsdFiatPair(t)

	// The legs differ in price, weight AND amount scale, so a dropped leg
	// (0.20 or 0.30), an equal-weighted merge (0.25) or a merge without
	// per-source scale normalisation (≈0.2231) each miss 0.275.
	store := &mockStore{
		perPair: map[string][]canonical.Trade{
			// 1 XLM @ 0.20 USDT on an 8dp venue.
			"crypto:XLM/crypto:USDT": {
				backerTrade(t, "USDT", "binance",
					big.NewInt(100_000_000), big.NewInt(20_000_000), now.Add(-2*time.Minute)),
			},
			// 3 XLM @ 0.30 USDC on SDEX (7dp).
			"crypto:XLM/crypto:USDC": {
				backerTrade(t, "USDC", "sdex",
					big.NewInt(30_000_000), big.NewInt(9_000_000), now.Add(-1*time.Minute)),
			},
		},
	}
	orch, mr := newOrch5m(t, store, targetPair, Config{EnableStablecoinFiatProxy: true})
	tickOnce(t, orch)

	for _, p := range []string{
		"crypto:XLM/fiat:USD",
		"crypto:XLM/crypto:USDT",
		"crypto:XLM/crypto:USDC",
		"crypto:XLM/crypto:DAI",
		"crypto:XLM/crypto:PYUSD",
		"crypto:XLM/crypto:USDP",
	} {
		if store.callsByPair[p] == 0 {
			t.Errorf("expected TradesInRange call for %q (got %d)", p, store.callsByPair[p])
		}
	}

	val, err := mr.Get(vwapKey5m(targetPair))
	if err != nil {
		t.Fatalf("miniredis Get: %v", err)
	}
	// (1×0.20 + 3×0.30) / 4 XLM.
	if val != "0.275000000000" {
		t.Errorf("collapsed VWAP = %q, want 0.275000000000 (both backer legs, volume-weighted)", val)
	}
	for _, backer := range []string{"crypto:USDT", "crypto:USDC"} {
		badKey := "vwap:crypto:XLM:" + backer + ":300"
		if mr.Exists(badKey) {
			t.Errorf("unexpected backer-keyed VWAP %q", badKey)
		}
	}
}

func TestTick_StablecoinExpansion_SingleBackerFetchFailureDoesNotAbortWindow(t *testing.T) {
	store := &mockStore{
		perPair: map[string][]canonical.Trade{
			"crypto:XLM/crypto:USDT": {
				backerTrade(t, "USDT", "binance",
					big.NewInt(100_000_000), big.NewInt(20_000_000), time.Now().Add(-2*time.Minute)),
			},
		},
		perPairErr: map[string]error{
			"crypto:XLM/crypto:USDC": errors.New("simulated timescale timeout"),
		},
	}
	pair := xlmUsdFiatPair(t)
	orch, mr := newOrch5m(t, store, pair, Config{EnableStablecoinFiatProxy: true})
	tickOnce(t, orch)
	if !mr.Exists(vwapKey5m(pair)) {
		t.Error("VWAP key missing — USDC fetch error should not have aborted the window")
	}
}

func TestTick_StablecoinExpansion_DisabledFetchesOnlyDirectPair(t *testing.T) {
	now := time.Now()
	store := &mockStore{
		perPair: map[string][]canonical.Trade{
			"crypto:XLM/fiat:USD": {
				buildTradeFrom(t, "exchangeratesapi",
					big.NewInt(100_000_000), big.NewInt(20_000_000), now.Add(-1*time.Minute)),
			},
			"crypto:XLM/crypto:USDT": {
				backerTrade(t, "USDT", "binance",
					big.NewInt(100_000_000), big.NewInt(20_000_000), now),
			},
		},
	}
	orch, _ := newOrch5m(t, store, xlmUsdFiatPair(t), Config{})
	tickOnce(t, orch)

	if got := store.callsByPair["crypto:XLM/fiat:USD"]; got != 1 {
		t.Errorf("direct-pair calls = %d want 1", got)
	}
	if got := store.callsByPair["crypto:XLM/crypto:USDT"]; got != 0 {
		t.Errorf("USDT backer calls = %d want 0 (expansion disabled)", got)
	}
}

// ─── Anomaly evaluator + freeze writer ─────────────────────────────

// recordingFreezeMarker records marker writes and models the real
// marker's presence: Clear removes it and LoadState reports that, so an
// operator unfreeze (marker gone under a live freeze) is testable.
type recordingFreezeMarker struct {
	marks   []recordedMark
	clears  int
	present bool
	state   freeze.State
	err     error
}

type recordedMark struct {
	asset       canonical.Asset
	quote       canonical.Asset
	frozenValue string
	decision    anomaly.Decision
	state       freeze.State
	ttl         time.Duration
}

func (r *recordingFreezeMarker) Mark(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error {
	return r.MarkHold(ctx, asset, quote, frozenValue, decision, freeze.State{}, 0)
}

func (r *recordingFreezeMarker) MarkHold(_ context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision, state freeze.State, ttl time.Duration) error {
	r.marks = append(r.marks, recordedMark{
		asset:       asset,
		quote:       quote,
		frozenValue: frozenValue,
		decision:    decision,
		state:       state,
		ttl:         ttl,
	})
	if r.err != nil {
		return r.err
	}
	r.present = true
	r.state = state
	return nil
}

func (r *recordingFreezeMarker) LoadState(_ context.Context, _, _ canonical.Asset) (freeze.State, bool, error) {
	if !r.present {
		return freeze.State{}, false, nil
	}
	return r.state, true, nil
}

func (r *recordingFreezeMarker) Clear(_ context.Context, _, _ canonical.Asset) error {
	r.clears++
	r.present = false
	r.state = freeze.State{}
	return nil
}

// newAnomalyChecker builds a Checker with stablecoin-tight thresholds
// (warn 1%, freeze 2%), forcing the pair's base to the stablecoin class.
func newAnomalyChecker(t *testing.T, pair canonical.Pair) *anomaly.Checker {
	t.Helper()
	thresholds := anomaly.DefaultThresholds()
	thresholds[anomaly.ClassStablecoin] = anomaly.Thresholds{
		WarnPct: 1.0, FreezePct: 2.0,
	}
	classifier := anomaly.NewClassifier(map[string]anomaly.AssetClass{
		pair.Base.String(): anomaly.ClassStablecoin,
	})
	c, err := anomaly.NewChecker(thresholds, classifier)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	return c
}

// newPrimedAnomalyOrch is a 5m XLM/USDT orchestrator with the stablecoin
// checker, a prior bucket of 1.00, and one 1 XLM print at quote/1e8 USDT.
func newPrimedAnomalyOrch(t *testing.T, cfg Config, quote int64) (*Orchestrator, *miniredis.Miniredis) {
	t.Helper()
	pair := xlmUsdtPair(t)
	cfg.Anomaly = newAnomalyChecker(t, pair)
	store := &mockStore{trades: []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(quote), time.Now()),
	}}
	o, mr := newOrch5m(t, store, pair, cfg)
	o.prevVWAPs[pair.String()+":"+(5*time.Minute).String()] = big.NewRat(1, 1)
	return o, mr
}

// With no comparator every bucket publishes; with a checker the
// prevVWAPs slot fills so the next tick has one.
func TestTick_AnomalyAllow_PublishesAndUpdatesPrev(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checker bool
		trade   func(*testing.T) canonical.Trade
	}{
		{"checker wired", true, func(t *testing.T) canonical.Trade {
			return buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000), time.Now())
		}},
		{"nil checker", false, func(t *testing.T) canonical.Trade {
			return buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), time.Now())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := xlmUsdtPair(t)
			var cfg Config
			if tc.checker {
				cfg.Anomaly = newAnomalyChecker(t, pair)
			}
			o, mr := newOrch5m(t, &mockStore{trades: []canonical.Trade{tc.trade(t)}}, pair, cfg)
			tickOnce(t, o)
			if !mr.Exists(vwapKey5m(pair)) {
				t.Error("VWAP key should be present")
			}
			if tc.checker && o.prevVWAPs[pair.String()+":"+(5*time.Minute).String()] == nil {
				t.Error("prevVWAPs slot should populate on first publish")
			}
		})
	}
}

// A freeze leaves the cached LKG untouched, marks it, keeps the
// comparator on the LKG, publishes no closed bucket, and re-arms the LKG
// TTL to the whole hold (ADR-0019 uncorroborated hold + silence grace,
// not the flat cachekeys.FreezeTTL), so the API never serves frozen=true
// with an evicted value.
func TestTick_AnomalyFreeze_SkipsCacheAndMarks(t *testing.T) {
	for _, initialTTL := range []time.Duration{time.Minute, 10 * time.Second} {
		t.Run("initial TTL "+initialTTL.String(), func(t *testing.T) {
			marker := &recordingFreezeMarker{}
			pub := &recordingStreamPublisher{}
			// 1 XLM ≈ 2.10 USDT: 110% off the prior 1.00, single source.
			o, mr := newPrimedAnomalyOrch(t, Config{FreezeWriter: marker, StreamPublisher: pub}, 210_000_000)
			pair := xlmUsdtPair(t)
			stateKey := pair.String() + ":" + (5 * time.Minute).String()
			cacheKey := vwapKey5m(pair)
			o.cache.Set(context.Background(), cacheKey, "1.000000000000", initialTTL)

			tickOnce(t, o)

			if got, err := mr.Get(cacheKey); err != nil || got != "1.000000000000" {
				t.Errorf("cache after freeze = %q (err %v), want untouched 1.000000000000", got, err)
			}
			if len(marker.marks) != 1 {
				t.Fatalf("Mark called %d times, want 1", len(marker.marks))
			}
			m := marker.marks[0]
			if !m.decision.IsFrozen() {
				t.Errorf("decision passed to Mark wasn't frozen: %+v", m.decision)
			}
			if m.frozenValue != "1.000000000000" {
				t.Errorf("frozenValue = %q, want %q (LKG VWAP at 12 decimals)", m.frozenValue, "1.000000000000")
			}
			if o.prevVWAPs[stateKey].Cmp(big.NewRat(1, 1)) != 0 {
				t.Errorf("prevVWAPs slot moved during freeze; got %s, want 1/1", o.prevVWAPs[stateKey].FloatString(6))
			}
			if len(pub.calls) != 0 {
				t.Errorf("PublishClosedBucket called %d times during freeze, want 0", len(pub.calls))
			}

			wantTTL := freeze.DefaultUncorroboratedInitialHold + freeze.DefaultMarkerGrace
			ttl := mr.TTL(cacheKey)
			if ttl <= initialTTL {
				t.Errorf("LKG VWAP TTL = %v after freeze; want extended past %v", ttl, initialTTL)
			}
			if ttl > wantTTL {
				t.Errorf("LKG VWAP TTL = %v after freeze; want ≤ %v (hold + silence grace)", ttl, wantTTL)
			}
			if ttl < wantTTL-time.Minute {
				t.Errorf("LKG VWAP TTL = %v after freeze; want ≈ %v — the LKG must cover the whole hold", ttl, wantTTL)
			}
		})
	}
}

// A frozen composite's provenance marker and quality-flags meta must
// outlive the ordinary VWAP TTL with its value, or mid-hold the API
// serves the LKG as a direct VWAP without its triangulated/diverged flags.
func TestKeepFrozenVWAPAlive_ExtendsCompositeQualifiers(t *testing.T) {
	pair := xlmUsdtPair(t)
	cache, mr := newTestRedis(t)
	window := time.Hour
	o := New(nil, cache, Config{Pairs: []canonical.Pair{pair}, Windows: []time.Duration{window}})
	ctx := context.Background()
	ttl := cachekeys.VWAPTTL(window)
	valKey := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	atKey := cachekeys.VWAPObservedAt(pair.Base, pair.Quote, window).String()
	provKey := cachekeys.VWAPProvenance(pair.Base, pair.Quote, window).String()
	metaKey := cachekeys.VWAPCompositeMeta(pair.Base, pair.Quote, window).String()
	covKey := cachekeys.VWAPCoverage(pair.Base, pair.Quote, window).String()
	const meta = `{"path_count":2,"diverged":true}`
	cache.Set(ctx, valKey, "1.000000000000", ttl)
	cache.Set(ctx, atKey, cachekeys.FormatVWAPObservedAt(time.Now().Truncate(time.Minute)), ttl)
	cache.Set(ctx, provKey, cachekeys.VWAPProvenanceTriangulated, ttl)
	cache.Set(ctx, metaKey, meta, ttl)
	cache.Set(ctx, covKey, cachekeys.FormatVWAPCoverage(cachekeys.WindowCoverage{}), ttl)

	hold := 35 * time.Minute
	o.keepFrozenVWAPAlive(ctx, pair, window, hold)

	for _, k := range []string{valKey, atKey, provKey, metaKey, covKey} {
		if got := mr.TTL(k); got != hold {
			t.Errorf("TTL(%s) = %v after keepalive; want the hold %v", k, got, hold)
		}
	}
	past := ttl + time.Minute
	mr.FastForward(past)
	if got, err := mr.Get(provKey); err != nil || got != cachekeys.VWAPProvenanceTriangulated {
		t.Errorf("provenance after %v = %q (err %v); want %q", past, got, err, cachekeys.VWAPProvenanceTriangulated)
	}
	if got, err := mr.Get(metaKey); err != nil || got != meta {
		t.Errorf("composite meta after %v = %q (err %v); want %q", past, got, err, meta)
	}
	if got, err := mr.Get(valKey); err != nil || got != "1.000000000000" {
		t.Errorf("LKG value after %v = %q (err %v); want untouched", past, got, err)
	}
}

// A direct pair has no provenance marker and the keepalive must not
// invent one, or its LKG would be relabelled triangulated.
func TestKeepFrozenVWAPAlive_DirectPairGainsNoProvenance(t *testing.T) {
	pair := xlmUsdtPair(t)
	cache, mr := newTestRedis(t)
	window := 5 * time.Minute
	o := New(nil, cache, Config{Pairs: []canonical.Pair{pair}, Windows: []time.Duration{window}})
	ctx := context.Background()
	valKey := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	cache.Set(ctx, valKey, "1.000000000000", window)

	hold := 35 * time.Minute
	o.keepFrozenVWAPAlive(ctx, pair, window, hold)

	for _, k := range []string{
		cachekeys.VWAPProvenance(pair.Base, pair.Quote, window).String(),
		cachekeys.VWAPCompositeMeta(pair.Base, pair.Quote, window).String(),
	} {
		if mr.Exists(k) {
			t.Errorf("%s exists after keepalive on a direct pair; want absent", k)
		}
	}
	if got := mr.TTL(valKey); got != hold {
		t.Errorf("TTL(value) = %v; want %v", got, hold)
	}
}

// With no FreezeWriter a freeze is still observed on the metric.
func TestTick_AnomalyFreeze_NilFreezeWriter_StillEmitsMetric(t *testing.T) {
	o, _ := newPrimedAnomalyOrch(t, Config{}, 210_000_000)
	before := testutil.ToFloat64(obs.AnomalyFreezeEngagedTotal.WithLabelValues("stablecoin"))
	tickOnce(t, o)
	after := testutil.ToFloat64(obs.AnomalyFreezeEngagedTotal.WithLabelValues("stablecoin"))
	if after-before != 1 {
		t.Errorf("AnomalyFreezeEngagedTotal{stablecoin} delta = %v, want 1", after-before)
	}
}

// A move past warn_pct but under freeze_pct must leave an operator-side
// trace and must not count as a freeze. It does not set
// flags.divergence_warning, which belongs to the cross-reference service.
func TestTick_AnomalyWarn_EmitsMetric(t *testing.T) {
	// 1.015 → +1.5%: between the 1% warn and 2% freeze lines.
	o, _ := newPrimedAnomalyOrch(t, Config{}, 101_500_000)

	beforeWarn := testutil.ToFloat64(obs.AnomalyWarnTotal.WithLabelValues("stablecoin"))
	beforeFreeze := testutil.ToFloat64(obs.AnomalyFreezeEngagedTotal.WithLabelValues("stablecoin"))
	tickOnce(t, o)
	if got := testutil.ToFloat64(obs.AnomalyWarnTotal.WithLabelValues("stablecoin")) - beforeWarn; got != 1 {
		t.Errorf("AnomalyWarnTotal{stablecoin} delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeEngagedTotal.WithLabelValues("stablecoin")) - beforeFreeze; got != 0 {
		t.Errorf("AnomalyFreezeEngagedTotal{stablecoin} delta = %v, want 0 — a warn must not be recorded as a freeze", got)
	}
}

func TestDistinctSourceCount(t *testing.T) {
	mk := func(src string) canonical.Trade {
		return buildTradeFrom(t, src, big.NewInt(1), big.NewInt(1), time.Now())
	}
	tests := []struct {
		name   string
		trades []canonical.Trade
		want   int
	}{
		{"empty", nil, 0},
		{"single source", []canonical.Trade{mk("binance"), mk("binance")}, 1},
		{"three distinct", []canonical.Trade{mk("binance"), mk("kraken"), mk("coinbase"), mk("binance")}, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := distinctSourceCount(tc.trades); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// usdQuoteDecimals is the classification both valuation and MinUSDVolume
// applicability use: fiat:USD and abstract USD-pegged tickers always,
// classic/Soroban quotes only when on the operator's peg lists.
func TestUSDQuoteDecimals(t *testing.T) {
	usd, _ := canonical.NewFiatAsset("USD")
	eur, _ := canonical.NewFiatAsset("EUR")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	usdc, _ := canonical.NewCryptoAsset("USDC")
	dai, _ := canonical.NewCryptoAsset("DAI")
	pyusd, _ := canonical.NewCryptoAsset("PYUSD")
	usdp, _ := canonical.NewCryptoAsset("USDP")
	eurc, _ := canonical.NewCryptoAsset("EURC")
	cryptoXLM, _ := canonical.NewCryptoAsset("XLM")

	classicUSDC, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	classicUnpegged, err := canonical.NewClassicAsset("YXLM", "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	sacUSDC, err := canonical.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	sacUnpegged, err := canonical.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}

	classicPegs := []canonical.Asset{classicUSDC}
	sorobanPegs := []canonical.Asset{sacUSDC}

	tests := []struct {
		name        string
		quote       canonical.Asset
		wantDecimal int
		wantOK      bool
	}{
		{"fiat:USD always valuable", usd, 8, true},
		{"fiat:EUR unvaluable (non-USD fiat)", eur, 0, false},
		// The stablecoin-proxy expansion fetches BASE/crypto:USDT; valuing it
		// at $0 would hide that volume from MinUSDVolume.
		{"crypto:USDT valuable (abstract USD peg, off-chain 1e8)", usdt, 8, true},
		{"crypto:USDC valuable (abstract USD peg)", usdc, 8, true},
		{"crypto:DAI valuable (abstract USD peg)", dai, 8, true},
		{"crypto:PYUSD valuable (abstract USD peg)", pyusd, 8, true},
		{"crypto:USDP valuable (abstract USD peg)", usdp, 8, true},
		{"crypto:EURC unvaluable (EUR peg needs a live FX rate)", eurc, 0, false},
		{"crypto:XLM unvaluable (not a stablecoin)", cryptoXLM, 0, false},
		{"classic pegged USDC valuable", classicUSDC, 7, true},
		{"classic un-pegged asset unvaluable", classicUnpegged, 0, false},
		{"Soroban SAC pegged USDC valuable", sacUSDC, 7, true},
		{"Soroban SAC un-pegged contract unvaluable", sacUnpegged, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec, ok := usdQuoteDecimals(tc.quote, classicPegs, sorobanPegs)
			if ok != tc.wantOK || dec != tc.wantDecimal {
				t.Errorf("usdQuoteDecimals(%s) = (%d, %v), want (%d, %v)",
					tc.quote.String(), dec, ok, tc.wantDecimal, tc.wantOK)
			}
		})
	}
}

func TestTick_MinUSDVolumeFilter(t *testing.T) {
	pair := xlmUsdFiatPair(t)
	xlm := pair.Base

	// exchangeratesapi is FX class (IncludeInVWAP) and stamps amounts at
	// 1e6, which the gate honours per trade; q is in those units.
	mkFXTrade := func(q *big.Int, ts time.Time) canonical.Trade {
		return canonical.Trade{
			Source:      "exchangeratesapi",
			Ledger:      0,
			TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
			OpIndex:     0,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: canonical.NewAmount(q),
		}
	}

	for _, tc := range []struct {
		name     string
		q        int64
		source   string
		min      float64
		wantDrop float64
	}{
		{"thin window: rejected", 100_000, "", 10_000, 1},
		{"fat $10M window: published", 10_000_000_000_000, "", 10_000, 0},
		// $999,999,999.99999999 at the CEX 1e8 scale is within half a
		// float64 ulp of 1e9, so only an exact comparison sees it short.
		{"one 1e-8 unit under a $1B floor: rejected", 99_999_999_999_999_999, "binance", 1e9, 1},
		{"filter off (MinUSDVolume=0): thin window publishes", 100_000, "", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trade := mkFXTrade(big.NewInt(tc.q), time.Now())
			if tc.source != "" {
				trade.Source = tc.source
			}
			orch, mr := newOrch5m(t, &mockStore{trades: []canonical.Trade{trade}}, pair, Config{MinUSDVolume: tc.min})
			if got := minUSDDrops(t, orch); got != tc.wantDrop {
				t.Errorf("min_usd_volume drop counter delta = %v, want %v", got, tc.wantDrop)
			}
			assertPublished(t, orch, mr, vwapKey5m(pair), tc.wantDrop == 0)
		})
	}

	// Classic USD-pegged proxy legs are 7dp, not the off-chain 1e8: a
	// uniform 1e8 divisor would value this $10k window at $1k.
	t.Run("classic USD-pegged proxy: $10k publishes under min=10000", func(t *testing.T) {
		classicUSDC, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
		if err != nil {
			t.Fatalf("NewClassicAsset: %v", err)
		}
		xlmUSDCPair, _ := canonical.NewPair(xlm, classicUSDC)
		classicUSDCTrade := canonical.Trade{
			Source:      "soroswap",
			Ledger:      52_500_000,
			TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
			Timestamp:   time.Now(),
			Pair:        xlmUSDCPair,
			BaseAmount:  canonical.NewAmount(big.NewInt(1_000_000_000_000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(100_000_000_000)), // $10,000 at 7dp
		}
		store := &mockStore{perPair: map[string][]canonical.Trade{
			xlmUSDCPair.String(): {classicUSDCTrade},
			pair.String():        nil,
		}}
		orch, mr := newOrch5m(t, store, pair, Config{
			MinUSDVolume:              10_000,
			EnableStablecoinFiatProxy: true,
			USDPeggedClassicAssets:    []canonical.Asset{classicUSDC},
		})
		if got := minUSDDrops(t, orch); got != 0 {
			t.Errorf("min_usd_volume drop counter delta = %v; want 0 — $10k classic USDC at 7-dec clears $10k", got)
		}
		assertPublished(t, orch, mr, vwapKey5m(pair), true)
	})

	// A non-USD fiat window is held to the same USD floor at the FX snap;
	// with no admissible FX rate the floor is unverifiable, so it drops.
	t.Run("non-USD fiat pair: floor applied at the FX rate", func(t *testing.T) {
		gbp, _ := canonical.NewFiatAsset("GBP")
		eur, _ := canonical.NewFiatAsset("EUR")
		fresh := time.Now().Add(-time.Hour)
		for _, tc := range []struct {
			name    string
			quote   canonical.Asset
			amount  int64 // quote units at exchangeratesapi's 1e6 scale
			fx      *fakeFXStore
			publish bool
		}{
			// £9,000 is below a $10k floor read as dollars, but is $11,250 at 1.25.
			{"GBP above floor after conversion", gbp, 9_000_000_000, &fakeFXStore{quote: big.NewRat(125, 100), observedAt: fresh, source: "massive"}, true},
			// €9,500 at 1.05 = $9,975.
			{"EUR below floor after conversion", eur, 9_500_000_000, &fakeFXStore{quote: big.NewRat(105, 100), observedAt: fresh, source: "massive"}, false},
			{"EUR dust", eur, 100_000, &fakeFXStore{quote: big.NewRat(108, 100), observedAt: fresh, source: "massive"}, false},
			{"no FX store: fail-closed", eur, 100_000_000_000, nil, false},
			{"no FX quote: fail-closed", eur, 100_000_000_000, &fakeFXStore{}, false},
			{"stale FX quote: fail-closed", gbp, 100_000_000_000, &fakeFXStore{quote: big.NewRat(125, 100), observedAt: time.Now().Add(-100 * time.Hour), source: "massive"}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fiatPair, _ := canonical.NewPair(xlm, tc.quote)
				trade := mkFXTrade(big.NewInt(tc.amount), time.Now())
				trade.Pair = fiatPair
				cfg := Config{MinUSDVolume: 10_000}
				if tc.fx != nil {
					cfg.FXStore = tc.fx
				}
				orch, mr := newOrch5m(t, &mockStore{trades: []canonical.Trade{trade}}, fiatPair, cfg)
				nextBucket(orch)
				tickOnce(t, orch)
				if got := mr.Exists(vwapKey5m(fiatPair)); got != tc.publish {
					t.Errorf("published=%v, want %v (VWAPWrites=%d)", got, tc.publish, orch.Stats().VWAPWrites)
				}
				if tc.fx != nil {
					if len(tc.fx.calls) == 0 {
						t.Fatal("FX store never asked for a rate")
					}
					wantLeg, _ := canonical.NewPair(tc.quote, pair.Quote)
					for _, c := range tc.fx.calls {
						if c.pair.String() != wantLeg.String() {
							t.Errorf("FX leg requested = %s, want quote->USD %s", c.pair, wantLeg)
						}
					}
				}
			})
		}
	})

	// The floor applies to the class-filter survivors, not the pre-filter
	// total: $1k survives (FX, 1e6) beside a dropped $100k unregistered row
	// (1e8 fallback); $101k would clear $10k, the survivor must not.
	t.Run("class filter gutted window: drops despite pre-filter clearing threshold", func(t *testing.T) {
		survivor := mkFXTrade(big.NewInt(1_000_000_000), time.Now())
		discardedByClass := canonical.Trade{
			Source:      "test-no-vwap",
			Ledger:      0,
			TxHash:      "1111111111111111111111111111111111111111111111111111111111111111",
			OpIndex:     0,
			Timestamp:   time.Now(),
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(10_000_000_000_000)),
		}
		store := &mockStore{trades: []canonical.Trade{discardedByClass, survivor}}
		orch, mr := newOrch5m(t, store, pair, Config{MinUSDVolume: 10_000})
		if got := minUSDDrops(t, orch); got != 1 {
			t.Errorf("min_usd_volume drop counter delta = %v, want 1 — survivor $1k must fail the $10k gate", got)
		}
		assertPublished(t, orch, mr, vwapKey5m(pair), false)
	})
}

// Under the stablecoin proxy each backer leg is valued against its own
// abstract USD-pegged quote before the rewrite onto fiat:USD, so USDT
// volume counts toward the floor, and a thin USDT window is still gated.
func TestTick_MinUSDVolumeFilter_StablecoinProxyLegs(t *testing.T) {
	target := xlmUsdFiatPair(t)

	// Off-chain CEX convention: 1e8 for both legs. XLM @ $0.20.
	usdtTrade := func(txHash string, baseAmt, quoteAmt int64) canonical.Trade {
		tr := backerTrade(t, "USDT", "binance", big.NewInt(baseAmt), big.NewInt(quoteAmt), time.Now().Add(-time.Minute))
		tr.TxHash = txHash
		return tr
	}
	directUSDTrade := func(txHash string, baseAmt, quoteAmt int64) canonical.Trade {
		return canonical.Trade{
			Source:      "coinbase",
			TxHash:      txHash,
			Timestamp:   time.Now().Add(-2 * time.Minute),
			Pair:        target,
			BaseAmount:  canonical.NewAmount(big.NewInt(baseAmt)),
			QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
		}
	}

	for _, tc := range []struct {
		name     string
		perPair  map[string][]canonical.Trade
		wantDrop float64
		wantVWAP string // "" = must not publish
	}{
		{
			// 60,000 XLM @ 0.20 USDT = $12,000.
			name: "USDT-only window worth $12k publishes",
			perPair: map[string][]canonical.Trade{
				"crypto:XLM/crypto:USDT": {usdtTrade(
					"1111111111111111111111111111111111111111111111111111111111111111",
					6_000_000_000_000, 1_200_000_000_000)},
			},
			wantVWAP: "0.200000000000",
		},
		{
			// $6k direct + $6k USDT = $12k; the direct leg alone would drop.
			name: "mixed window: stablecoin leg counts toward the floor",
			perPair: map[string][]canonical.Trade{
				"crypto:XLM/fiat:USD": {directUSDTrade(
					"2222222222222222222222222222222222222222222222222222222222222222",
					3_000_000_000_000, 600_000_000_000)},
				"crypto:XLM/crypto:USDT": {usdtTrade(
					"3333333333333333333333333333333333333333333333333333333333333333",
					3_000_000_000_000, 600_000_000_000)},
			},
			wantVWAP: "0.200000000000",
		},
		{
			// 49,995 XLM @ 0.20 USDT = $9,999.
			name: "thin USDT window is still gated",
			perPair: map[string][]canonical.Trade{
				"crypto:XLM/crypto:USDT": {usdtTrade(
					"4444444444444444444444444444444444444444444444444444444444444444",
					4_999_500_000_000, 999_900_000_000)},
			},
			wantDrop: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, mr := newOrch5m(t, &mockStore{perPair: tc.perPair}, target, Config{
				MinUSDVolume:              10_000,
				EnableStablecoinFiatProxy: true,
			})
			if got := minUSDDrops(t, orch); got != tc.wantDrop {
				t.Errorf("min_usd_volume drop counter delta = %v, want %v", got, tc.wantDrop)
			}
			assertPublished(t, orch, mr, vwapKey5m(target), tc.wantVWAP != "")
			if tc.wantVWAP == "" {
				return
			}
			if got, err := mr.Get(vwapKey5m(target)); err != nil || got != tc.wantVWAP {
				t.Errorf("published VWAP = %q (err %v), want %q", got, err, tc.wantVWAP)
			}
		})
	}
}

// A target configured directly against an abstract stablecoin quote is
// valuable, so the floor applies (no proxy expansion involved).
func TestTick_MinUSDVolumeFilter_DirectStablecoinQuotedTarget(t *testing.T) {
	target := xlmUsdtPair(t)
	// 49,995 XLM @ 0.20 USDT = $9,999 at the off-chain 1e8 scale.
	dust := backerTrade(t, "USDT", "binance",
		big.NewInt(4_999_500_000_000), big.NewInt(999_900_000_000), time.Now().Add(-time.Minute))
	orch, mr := newOrch5m(t, &mockStore{trades: []canonical.Trade{dust}}, target, Config{MinUSDVolume: 10_000})
	if got := minUSDDrops(t, orch); got != 1 {
		t.Errorf("min_usd_volume drop counter delta = %v, want 1 — a $9,999 USDT-quoted window must be gated, not exempt", got)
	}
	assertPublished(t, orch, mr, vwapKey5m(target), false)
}

// A directly-configured SAC-quoted pair is floored once the SAC is on
// USDPeggedSorobanAssets; without a recognised peg it is unvaluable and
// dropped fail-closed, observably.
func TestTick_MinUSDVolumeFilter_SorobanQuotedPair(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	classicUSDC, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	sacUSDC, err := canonical.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	pair, err := canonical.NewPair(xlm, sacUSDC)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	// SAC quote_amount is at the 7-decimal classic invariant.
	mkTrade := func(q int64) canonical.Trade {
		return canonical.Trade{
			Source:      "soroswap",
			Ledger:      52_500_000,
			TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
			Timestamp:   time.Now(),
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(1_000_000_000_000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(q)),
		}
	}

	for _, tc := range []struct {
		name           string
		q              int64
		pegged         bool
		wantDrop       float64
		wantUnvaluable float64
		wantPublish    bool
	}{
		{"sub-floor $9,999.999: no VWAP served", 99_999_990_000, true, 1, 0, false},
		{"exactly $10,000: VWAP served", 100_000_000_000, true, 0, 0, true},
		{"SAC not pegged: unvaluable, dropped fail-closed", 99_999_990_000, false, 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{MinUSDVolume: 10_000}
			if tc.pegged {
				cfg.USDPeggedClassicAssets = []canonical.Asset{classicUSDC}
				cfg.USDPeggedSorobanAssets = []canonical.Asset{sacUSDC}
			}
			orch, mr := newOrch5m(t, &mockStore{trades: []canonical.Trade{mkTrade(tc.q)}}, pair, cfg)
			unvaluable := obs.AggregatorMinUSDVolumeUnvaluableTotal.WithLabelValues(pair.String())
			unvaluableDrops := obs.AggregatorDroppedWindowsTotal.WithLabelValues("min_usd_volume_unvaluable")
			beforeU, beforeUD := testutil.ToFloat64(unvaluable), testutil.ToFloat64(unvaluableDrops)
			if got := minUSDDrops(t, orch); got != tc.wantDrop {
				t.Errorf("min_usd_volume drop counter delta = %v, want %v", got, tc.wantDrop)
			}
			if got := testutil.ToFloat64(unvaluable) - beforeU; got != tc.wantUnvaluable {
				t.Errorf("AggregatorMinUSDVolumeUnvaluableTotal delta = %v, want %v", got, tc.wantUnvaluable)
			}
			if got := testutil.ToFloat64(unvaluableDrops) - beforeUD; got != tc.wantUnvaluable {
				t.Errorf("DroppedWindowsTotal[min_usd_volume_unvaluable] delta = %v, want %v", got, tc.wantUnvaluable)
			}
			assertPublished(t, orch, mr, vwapKey5m(pair), tc.wantPublish)
		})
	}
}

// recordingStreamPublisher captures stream publishes. Implements
// [StreamPublisher].
type recordingStreamPublisher struct {
	calls  []recordedPublish
	frozen []recordedFrozen
	err    error
}

type recordedFrozen struct {
	pair        canonical.Pair
	window      time.Duration
	observedAt  time.Time
	frozenSince time.Time
}

func (r *recordingStreamPublisher) PublishFrozenBucket(
	_ context.Context, pair canonical.Pair, window time.Duration, observedAt, frozenSince time.Time,
) error {
	r.frozen = append(r.frozen, recordedFrozen{pair: pair, window: window, observedAt: observedAt, frozenSince: frozenSince})
	return r.err
}

type recordedPublish struct {
	pair       canonical.Pair
	window     time.Duration
	value      string
	observedAt time.Time
	coverage   *cachekeys.WindowCoverage
}

func (r *recordingStreamPublisher) PublishClosedBucket(
	_ context.Context,
	pair canonical.Pair,
	window time.Duration,
	valueDecimal string,
	observedAt time.Time,
	coverage *cachekeys.WindowCoverage,
) error {
	r.calls = append(r.calls, recordedPublish{
		pair: pair, window: window, value: valueDecimal, observedAt: observedAt, coverage: coverage,
	})
	return r.err
}

// A published bucket fans out once with the same pair, window and value.
// observed_at is the minute-truncated bucket boundary, not the tick's
// wall clock, so closed-bucket payloads are byte-identical across
// subscribers and regions (ADR-0015).
func TestTick_StreamPublisher_FiresOnSuccessfulPublish(t *testing.T) {
	pair := xlmUsdtPair(t)
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000), time.Now()),
		},
	}
	pub := &recordingStreamPublisher{}
	o, _ := newOrch5m(t, store, pair, Config{StreamPublisher: pub})
	// 17.5 s past the minute, so a raw forward is distinguishable.
	tickTime := time.Date(2026, 9, 21, 14, 35, 17, 500_000_000, time.UTC)
	o.clock = func() time.Time { return tickTime }

	tickOnce(t, o)
	if len(pub.calls) != 1 {
		t.Fatalf("PublishClosedBucket called %d times, want 1", len(pub.calls))
	}
	c := pub.calls[0]
	if c.pair != pair {
		t.Errorf("pair = %v, want %v", c.pair, pair)
	}
	if c.window != 5*time.Minute {
		t.Errorf("window = %v, want 5m", c.window)
	}
	if c.value != "0.175820000000" {
		t.Errorf("value = %q, want 0.175820000000", c.value)
	}
	if want := time.Date(2026, 9, 21, 14, 35, 0, 0, time.UTC); !c.observedAt.Equal(want) {
		t.Errorf("observed_at = %s, want %s (minute-truncated bucket boundary)", c.observedAt, want)
	}
}

// A publish error never fails the tick: the cache write is the source of
// truth, the stream is enrichment.
func TestTick_StreamPublisher_ErrorDoesNotPropagate(t *testing.T) {
	pair := xlmUsdtPair(t)
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000), time.Now()),
		},
	}
	o, mr := newOrch5m(t, store, pair, Config{StreamPublisher: &recordingStreamPublisher{err: errors.New("redis down")}})
	tickOnce(t, o)
	if !mr.Exists(vwapKey5m(pair)) {
		t.Error("VWAP key should be present even when stream publish fails")
	}
}

// Equal real volume on sdex (7dp) and binance (8dp) must weigh equally:
// 0.11, not the un-normalized 13/110.
func TestComputeNormalizedVWAP_MixesOnChainAndCEXScale(t *testing.T) {
	pair := xlmUsdtPair(t)
	pow10 := func(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

	// 1000 XLM @ 0.10 at sdex's 7dp.
	onchain := buildTradeFrom(t, "sdex",
		new(big.Int).Mul(big.NewInt(1000), pow10(7)),
		new(big.Int).Mul(big.NewInt(100), pow10(7)),
		time.Now())
	// 1000 XLM @ 0.12 at binance's 8dp.
	cex := buildTradeFrom(t, "binance",
		new(big.Int).Mul(big.NewInt(1000), pow10(8)),
		new(big.Int).Mul(big.NewInt(120), pow10(8)),
		time.Now())

	o := New(nil, nil, Config{})
	got, err := o.computeNormalizedVWAP([]canonical.Trade{onchain, cex}, pair)
	if err != nil {
		t.Fatalf("computeNormalizedVWAP: %v", err)
	}
	if got.Cmp(big.NewRat(11, 100)) != 0 {
		t.Errorf("VWAP = %s, want 0.11 (equal-real-volume midpoint) — 13/110 (%s) is the un-normalized bug value",
			got.FloatString(10), big.NewRat(13, 110).FloatString(10))
	}
}

// Off-chain trades are valued at their own source's declared scale (FX
// 1e6, CEX 1e8); on-chain pegs keep the structural 7dp whatever the
// reporting source, since unregistered sources fall back to 8.
func TestUSDVolumeForPairPerTrade_PerSourceDecimals(t *testing.T) {
	usdPair := xlmUsdFiatPair(t)
	classicUSDC, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	classicPair, _ := canonical.NewPair(usdPair.Base, classicUSDC)
	classicPegs := []canonical.Asset{classicUSDC}

	mk := func(pair canonical.Pair, source string, quote int64, opIndex uint32) canonical.Trade {
		return canonical.Trade{
			Source:      source,
			Ledger:      52_500_000,
			TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
			OpIndex:     opIndex,
			Timestamp:   time.Now(),
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
		}
	}

	for _, tc := range []struct {
		name   string
		pair   canonical.Pair
		trades []canonical.Trade
		want   int64
	}{
		{
			"fiat:USD from a 6dp FX source", usdPair,
			[]canonical.Trade{mk(usdPair, "exchangeratesapi", 10_000_000_000, 0)},
			10_000,
		},
		{
			"fiat:USD from an 8dp CEX source", usdPair,
			[]canonical.Trade{mk(usdPair, "binance", 3_000_000_000_000, 0)},
			30_000,
		},
		{"mixed 6dp + 8dp bucket", usdPair, []canonical.Trade{
			mk(usdPair, "binance", 3_000_000_000_000, 0),
			mk(usdPair, "exchangeratesapi", 10_000_000_000, 1),
		}, 40_000},
		{
			"classic peg keeps the structural 7 whatever the source", classicPair,
			[]canonical.Trade{mk(classicPair, "test-unregistered-venue", 100_000_000_000, 0)},
			10_000,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			per := usdVolumeForPairPerTrade(tc.pair, tc.trades, classicPegs, nil)
			want := big.NewRat(tc.want, 1)
			if total := survivorUSDVolume(tc.trades, per); total.Cmp(want) != 0 {
				t.Fatalf("total = %s, want %d", total.FloatString(2), tc.want)
			}
			if len(tc.trades) == 1 {
				if got := per[tc.trades[0].ID()]; got.Cmp(want) != 0 {
					t.Errorf("per-trade USD = %s, want exactly %d", got.RatString(), tc.want)
				}
			}
		})
	}
}

// errContributionSink always fails, standing in for a DB outage.
type errContributionSink struct{ calls int }

func (s *errContributionSink) RecordContributions(context.Context, ContributionRecord) error {
	s.calls++
	return errors.New("price_source_contributions: connection refused")
}

// A lost contribution bucket is visible on a metric, not only a log line.
func TestFlushContributions_SinkFailureIsCounted(t *testing.T) {
	sink := &errContributionSink{}
	pair := xlmUsdtPair(t)
	orch, _ := newOrch5m(t, &mockStore{}, pair, Config{ContributionSink: sink})
	trades := []canonical.Trade{
		buildTrade(t, big.NewInt(100), big.NewInt(17), time.Now().Add(-time.Minute)),
	}

	before := testutil.ToFloat64(obs.AggregatorContributionWriteErrorsTotal)
	orch.flushContributions(context.Background(), pair, 5*time.Minute, trades, nil)

	if sink.calls != 1 {
		t.Fatalf("sink.calls = %d, want 1", sink.calls)
	}
	if got := testutil.ToFloat64(obs.AggregatorContributionWriteErrorsTotal) - before; got != 1 {
		t.Errorf("contribution_write_errors_total delta = %v, want 1", got)
	}
}

// wholeXLMTrade builds an XLM/USDT print of `xlm` whole units at num/den
// USDT, stamped at the source's registered amount scale.
func wholeXLMTrade(t *testing.T, source string, xlm, num, den int64, ts time.Time) canonical.Trade {
	t.Helper()
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(amountScaleDecimalsFor(source))), nil)
	base := new(big.Int).Mul(big.NewInt(xlm), unit)
	quote := new(big.Int).Quo(new(big.Int).Mul(base, big.NewInt(num)), big.NewInt(den))
	return buildTradeFrom(t, source, base, quote, ts)
}

// triangulateAll must snap FX at the tick's injected clock, not a second
// wall-clock read: the FX factor and the leg VWAPs it multiplies have to
// describe the same instant.
func TestTick_TriangulationFXSnapUsesInjectedClock(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	if err := mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000"); err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2019, 3, 4, 12, 43, 17, 0, time.UTC)
	fx := &fakeFXStore{
		quote:      big.NewRat(90, 100),
		observedAt: fixedNow.Add(-time.Hour),
		source:     "massive",
	}
	o := New(nil, cache, Config{
		Windows:        []time.Duration{window},
		Triangulations: []TriangulationChain{{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}}},
		FXStore:        fx,
	})
	o.clock = func() time.Time { return fixedNow }

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(fx.calls) != 1 {
		t.Fatalf("FXStore called %d times, want 1", len(fx.calls))
	}
	if want := fixedNow.Truncate(window); !fx.calls[0].cutoff.Equal(want) {
		t.Errorf("FX snap cutoff = %s, want %s from the injected clock", fx.calls[0].cutoff, want)
	}
}

// The publishing FX snap is held to the corroborator's admission rule: a
// quote older than the FX budget, or from a non-FX source, must not price
// a composite.
func TestTick_TriangulationRefusesUnusableFXSnap(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute
	fixedNow := time.Date(2019, 3, 4, 12, 43, 17, 0, time.UTC)

	for _, tc := range []struct {
		name       string
		observedAt time.Time
		source     string
		publishes  bool
	}{
		{"fresh FX quote", fixedNow.Add(-time.Hour), "massive", true},
		{"older than the FX budget", fixedNow.Add(-DefaultCompositeReferenceFXMaxAge - time.Hour), "massive", false},
		{"no observation time", time.Time{}, "massive", false},
		{"non-FX source", fixedNow.Add(-time.Hour), "band", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, mr := newTestRedis(t)
			if err := mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "0.080000000000"); err != nil {
				t.Fatal(err)
			}
			o := New(nil, cache, Config{
				Windows:        []time.Duration{window},
				Triangulations: []TriangulationChain{{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}}},
				FXStore:        &fakeFXStore{quote: big.NewRat(90, 100), observedAt: tc.observedAt, source: tc.source},
			})
			o.clock = func() time.Time { return fixedNow }
			if err := o.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			got, err := mr.Get(cachekeys.VWAP(xlmEUR.Base, xlmEUR.Quote, window).String())
			if tc.publishes && (err != nil || got != "0.072000000000") {
				t.Errorf("composite = %q (err %v), want 0.072000000000 = 0.08 x 0.90", got, err)
			}
			if !tc.publishes && err == nil {
				t.Errorf("composite %q published from an unusable FX snap", got)
			}
		})
	}
}

// TestTick_DecimalsLookup_NormalizesNonstandardLeg proves the forward-
// normalization wiring: a pair whose base leg is a confirmed 18-decimal
// Soroban token gets its published VWAP scaled
// by 10^(18-7), not served at the raw stroop-scale ratio.
func TestTick_DecimalsLookup_NormalizesNonstandardLeg(t *testing.T) {
	// Real on-chain contract id (the founding decimals
	// incident, per docs/operations/runbooks/dex.md)
	// — reused here purely as a valid, memorable C-strkey fixture.
	token, err := canonical.NewSorobanAsset("CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO")
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := canonical.NewPair(token, usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	// base_amount = 2.5 * 10^18 (18dp), quote_amount = 1.242 * 10^7 (7dp
	// USDC) → true price 0.4968 USDC/token. The raw (unadjusted) ratio
	// would be 0.4968 / 10^11.
	baseAmount := new(big.Int)
	baseAmount.SetString("2500000000000000000", 10)
	trade := canonical.Trade{
		Source:      "aquarius",
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Now().Add(-time.Minute),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(baseAmount),
		QuoteAmount: canonical.NewAmount(big.NewInt(12_420_000)),
	}

	store := &mockStore{trades: []canonical.Trade{trade}}
	rdb, mr := newTestRedis(t)

	orch := New(store, rdb, Config{
		Pairs:          []canonical.Pair{pair},
		Windows:        []time.Duration{5 * time.Minute},
		DecimalsLookup: fakeDecimalsLookup{token.String(): 18},
	})

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	key := "vwap:" + token.String() + ":" + usdc.String() + ":300"
	val, err := mr.Get(key)
	if err != nil {
		t.Fatalf("miniredis Get %q: %v", key, err)
	}
	if val[:6] != "0.4968" {
		t.Errorf("published VWAP = %q, want prefix 0.4968 (normalized) not a 10^-11-scale raw ratio", val)
	}
}

// TestTick_DecimalsLookup_NilIsByteIdenticalNoOp proves the default (nil
// DecimalsLookup, matching every deployment/test that predates this field)
// produces the exact same published VWAP as the 7dp path — the
// regression-safety half of constraint #5 (7dp assets untouched).
func TestTick_DecimalsLookup_NilIsByteIdenticalNoOp(t *testing.T) {
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000), time.Now().Add(-2*time.Minute)),
			buildTrade(t, big.NewInt(20_000_000_000), big.NewInt(3_518_000_000), time.Now().Add(-1*time.Minute)),
		},
	}
	rdb, mr := newTestRedis(t)

	// DecimalsLookup deliberately left unset (nil) — same Config shape
	// every pre-existing orchestrator test uses.
	orch := New(store, rdb, Config{
		Pairs:   []canonical.Pair{xlmUsdtPair(t)},
		Windows: []time.Duration{5 * time.Minute},
	})

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	key := "vwap:" + xlm.String() + ":" + usdt.String() + ":300"
	val, err := mr.Get(key)
	if err != nil {
		t.Fatalf("miniredis Get %q: %v", key, err)
	}
	if val[:5] != "0.175" {
		t.Errorf("stored VWAP = %q, want prefix 0.175 (unchanged from pre-normalization behaviour)", val)
	}
}

func TestTick_ExcludedSources_DropsStoredTrades(t *testing.T) {
	now := time.Now()
	store := &mockStore{
		trades: []canonical.Trade{
			buildTradeFrom(t, "binance",
				big.NewInt(100_000_000), big.NewInt(20_000_000), now.Add(-2*time.Minute)),
			buildTradeFrom(t, "kraken",
				big.NewInt(100_000_000), big.NewInt(1_000_000_000), now.Add(-1*time.Minute)),
		},
	}
	rdb, mr := newTestRedis(t)
	orch := New(store, rdb, Config{
		Pairs:           []canonical.Pair{xlmUsdtPair(t)},
		Windows:         []time.Duration{5 * time.Minute},
		ExcludedSources: []string{"kraken"},
	})
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	val, err := mr.Get("vwap:" + xlm.String() + ":" + usdt.String() + ":300")
	if err != nil {
		t.Fatalf("miniredis Get: %v", err)
	}
	if val[:4] != "0.20" {
		t.Errorf("VWAP = %q, want prefix 0.20 (kraken excluded)", val)
	}
}

// TestTick_AnomalyFreeze_StreamCarriesAMarker: a pair that freezes
// mid-stream must put the refused bucket on the closed-bucket stream as a
// frozen marker, once per bucket, or /v1/price/stream subscribers see only
// keepalives while /v1/price serves flags.frozen — a freeze would read as
// a quiet market.
func TestTick_AnomalyFreeze_StreamCarriesAMarker(t *testing.T) {
	pair := xlmUsdtPair(t)
	window := 5 * time.Minute
	cache, _ := newTestRedis(t)
	stream := &recordingStreamPublisher{}
	o := New(nil, cache, Config{
		Pairs:           []canonical.Pair{pair},
		Windows:         []time.Duration{window},
		Anomaly:         newAnomalyChecker(t, pair),
		FreezeWriter:    &recordingFreezeMarker{},
		StreamPublisher: stream,
	})
	t0 := time.Now().UTC().Truncate(closedBucket).Add(10 * time.Second)
	firstBucket := t0.Truncate(closedBucket)
	o.prevVWAPs[pair.String()+":"+window.String()] = big.NewRat(1, 1)
	o.store = &mockStore{trades: []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), t0.Add(-30*time.Second)),
	}}

	tick := func(at time.Time) {
		t.Helper()
		o.clock = func() time.Time { return at }
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	tick(t0)
	tick(t0.Add(20 * time.Second)) // replays the decided bucket
	tick(t0.Add(closedBucket))     // next bucket, still inside the hold

	if len(stream.calls) != 0 {
		t.Errorf("a refused bucket was published as a price: %+v", stream.calls)
	}
	if len(stream.frozen) != 2 {
		t.Fatalf("frozen markers = %d, want one per refused bucket (2): %+v", len(stream.frozen), stream.frozen)
	}
	for i, want := range []time.Time{firstBucket, firstBucket.Add(closedBucket)} {
		got := stream.frozen[i]
		if got.pair.String() != pair.String() || got.window != window || !got.observedAt.Equal(want) {
			t.Errorf("marker %d = %+v, want %s/%s at %s", i, got, pair, window, want)
		}
		if !got.frozenSince.Equal(firstBucket) {
			t.Errorf("marker %d frozenSince = %s, want the first refused bucket %s", i, got.frozenSince, firstBucket)
		}
	}
}

// TestTick_DeadQuoteIsNotMaskedByALiveSiblingQuote runs at the
// production entry point (Tick → refreshPairWindow → the write-time
// record → emitStalenessGauges).
//
// The scenario the finding describes: one base is configured against
// several quotes; one quote's feed goes dark while another keeps
// publishing. `stellarindex_price_staleness_seconds` is the ONLY input
// to the `stellarindex_api_price_stale` alert, so it must not be keyed
// by base alone — every publish of the live quote would reset the one
// timestamp the dead quote is judged by, and the gauge would read 0 for an
// asset whose other quote had served nothing for ten minutes.
//
// BTC is used (not XLM) so the native ↔ crypto:XLM dual-form merge is
// not in play: this pins the quote dimension alone.
func TestTick_DeadQuoteIsNotMaskedByALiveSiblingQuote(t *testing.T) {
	btc := mustCrypto(t, "BTC")
	live := mustStalenessPair(t, btc, mustCrypto(t, "USDT"))
	dead := mustStalenessPair(t, btc, mustFiat(t, "GBP"))

	clk := &stalenessTestClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	cache, _ := newTestRedis(t)
	store := &mockStore{perPair: map[string][]canonical.Trade{
		live.String(): liveTrades(live, clk.now),
		// dead: absent → empty window every tick, never a write.
	}}
	o := New(store, cache, Config{
		Pairs:   []canonical.Pair{live, dead},
		Windows: []time.Duration{5 * time.Minute},
	})
	o.clock = clk.Now

	for _, p := range []canonical.Pair{live, dead} {
		pairStaleness(p).Set(-1)
	}
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	for _, p := range []canonical.Pair{live, dead} {
		if got := testutil.ToFloat64(pairStaleness(p)); got != 0 {
			t.Fatalf("after first Tick: staleness for %s = %v, want 0 (first-sighting seed)", p, got)
		}
	}
	if o.Stats().VWAPWrites == 0 {
		t.Fatalf("precondition: the live quote %s never published — the test would prove nothing", live)
	}

	// Ten minutes on. The live quote publishes again; the dead quote
	// has produced nothing since it was first seen.
	clk.now = clk.now.Add(10 * time.Minute)
	store.perPair[live.String()] = liveTrades(live, clk.now)
	writesBefore := o.Stats().VWAPWrites
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if o.Stats().VWAPWrites == writesBefore {
		t.Fatalf("precondition: the live quote %s did not re-publish on the second Tick", live)
	}

	if got := testutil.ToFloat64(pairStaleness(dead)); got != 600 {
		t.Errorf("staleness{asset=crypto:BTC,quote=fiat:GBP} = %v, want 600 — %s has served nothing for 10 min; "+
			"a fresh %s must not reset the clock the alert judges it by", got, dead, live)
	}
	if got := testutil.ToFloat64(pairStaleness(live)); got != 0 {
		t.Errorf("staleness{asset=crypto:BTC,quote=crypto:USDT} = %v, want 0 — the quote label must name the dead quote, "+
			"not page the live one", got)
	}
}

// TestTick_AllQuotesLiveReadsFresh is the other half: keying by pair
// must not make a healthy asset read stale. Same harness, both quotes
// publishing on every Tick.
func TestTick_AllQuotesLiveReadsFresh(t *testing.T) {
	eth := mustCrypto(t, "ETH")
	a := mustStalenessPair(t, eth, mustCrypto(t, "USDT"))
	b := mustStalenessPair(t, eth, mustFiat(t, "GBP"))

	clk := &stalenessTestClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	cache, _ := newTestRedis(t)
	store := &mockStore{perPair: map[string][]canonical.Trade{
		a.String(): liveTrades(a, clk.now),
		b.String(): liveTrades(b, clk.now),
	}}
	o := New(store, cache, Config{
		Pairs:   []canonical.Pair{a, b},
		Windows: []time.Duration{5 * time.Minute},
	})
	o.clock = clk.Now

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	clk.now = clk.now.Add(10 * time.Minute)
	store.perPair[a.String()] = liveTrades(a, clk.now)
	store.perPair[b.String()] = liveTrades(b, clk.now)
	for _, p := range []canonical.Pair{a, b} {
		pairStaleness(p).Set(-1)
	}
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	for _, p := range []canonical.Pair{a, b} {
		if got := testutil.ToFloat64(pairStaleness(p)); got != 0 {
			t.Errorf("staleness for %s = %v, want 0 — both quotes published this Tick", p, got)
		}
	}
}

// TestTick_XLMDualFormIsMergedPerQuote pins how the native ↔ crypto:XLM
// merge composes with the quote dimension. The two forms are
// interchangeable for ONE quote (the API resolves
// `asset=native&quote=fiat:GBP` through either form's GBP key), so the
// merge is "freshest form" WITHIN a quote and "stalest quote" ACROSS
// them. A fresh native/USD says nothing about anybody's GBP.
//
// Driven through Tick, in both cfg.Pairs orders, because the merge this
// replaces was order-dependent once already.
func TestTick_XLMDualFormIsMergedPerQuote(t *testing.T) {
	xlm, native := mustCrypto(t, "XLM"), canonical.NativeAsset()
	usd, gbp := mustFiat(t, "USD"), mustFiat(t, "GBP")
	tickerUSD, nativeUSD := mustStalenessPair(t, xlm, usd), mustStalenessPair(t, native, usd)
	tickerGBP, nativeGBP := mustStalenessPair(t, xlm, gbp), mustStalenessPair(t, native, gbp)
	forward := []canonical.Pair{tickerUSD, nativeUSD, tickerGBP, nativeGBP}
	reversed := []canonical.Pair{nativeGBP, tickerGBP, nativeUSD, tickerUSD}

	for _, tc := range []struct {
		name  string
		pairs []canonical.Pair
		live  []canonical.Pair   // pairs that publish on every Tick
		want  map[string]float64 // by quote
	}{
		{"GBP dead on both forms, USD live on both", forward, []canonical.Pair{tickerUSD, nativeUSD}, map[string]float64{"fiat:USD": 0, "fiat:GBP": 600}},
		{"GBP dead on both forms, USD live on both (reversed)", reversed, []canonical.Pair{tickerUSD, nativeUSD}, map[string]float64{"fiat:USD": 0, "fiat:GBP": 600}},
		{"GBP live on native only, USD live on ticker only", forward, []canonical.Pair{nativeGBP, tickerUSD}, map[string]float64{"fiat:USD": 0, "fiat:GBP": 0}},
		{"GBP live on native only, USD live on ticker only (reversed)", reversed, []canonical.Pair{nativeGBP, tickerUSD}, map[string]float64{"fiat:USD": 0, "fiat:GBP": 0}},
		{"every quote live on every form", forward, forward, map[string]float64{"fiat:USD": 0, "fiat:GBP": 0}},
		{"nothing publishes", forward, nil, map[string]float64{"fiat:USD": 600, "fiat:GBP": 600}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &stalenessTestClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
			cache, _ := newTestRedis(t)
			store := &mockStore{perPair: map[string][]canonical.Trade{}}
			o := New(store, cache, Config{Pairs: tc.pairs, Windows: []time.Duration{5 * time.Minute}})
			o.clock = clk.Now

			for tick := 0; tick < 2; tick++ {
				for _, p := range tc.live {
					store.perPair[p.String()] = liveTrades(p, clk.now)
				}
				for _, p := range forward {
					pairStaleness(p).Set(-1)
				}
				before := o.Stats().VWAPWrites
				if err := o.Tick(context.Background()); err != nil {
					t.Fatalf("Tick %d: %v", tick, err)
				}
				if wrote := o.Stats().VWAPWrites - before; wrote != int64(len(tc.live)) {
					t.Fatalf("precondition: Tick %d published %d pairs, want %d", tick, wrote, len(tc.live))
				}
				if tick == 0 {
					clk.now = clk.now.Add(10 * time.Minute)
				}
			}

			for _, p := range forward {
				want := tc.want[p.Quote.String()]
				if got := testutil.ToFloat64(pairStaleness(p)); got != want {
					t.Errorf("staleness for %s = %v, want %v", p, got, want)
				}
			}
		})
	}
}

// TestTick_CompositeServedPairReadsFresh — the served VWAP key has TWO
// writers, and a pair-level clock stamped from only one of them is
// wrong for every pair the other one serves. BTC/EUR here has no direct
// trades; publishComposite writes its VWAP key on every Tick. With the
// stamp taken from refreshPairWindow alone, BTC/EUR read 600 s stale
// while publishing every tick, and because the gauge is the STALEST
// quote, it dragged crypto:BTC to 600 with it — a permanent false page
// for a healthy asset (second writer).
func TestTick_CompositeServedPairReadsFresh(t *testing.T) {
	fx := &fakeFXStore{quote: new(big.Rat).SetFrac(big.NewInt(90), big.NewInt(100)), source: "exchangeratesapi", observedAt: liveFXObservedAt}
	h := newCompositeStalenessHarness(t, fx, 0)

	if ok := h.tick(t, "ok"); ok != 1 {
		t.Fatalf("precondition: first Tick published the composite %v times, want 1", ok)
	}
	h.clk.now = h.clk.now.Add(10 * time.Minute)
	pairStaleness(h.direct).Set(-1)
	pairStaleness(h.target).Set(-1)
	if ok := h.tick(t, "ok"); ok != 1 {
		t.Fatalf("precondition: second Tick published the composite %v times, want 1", ok)
	}

	if got := testutil.ToFloat64(pairStaleness(h.target)); got != 0 {
		t.Errorf("staleness{asset=crypto:BTC,quote=fiat:EUR} = %v, want 0 — %s was published through its chain on this "+
			"Tick; a pair served by the composite writer is not a dead feed", got, h.target)
	}
	if got := testutil.ToFloat64(pairStaleness(h.direct)); got != 0 {
		t.Errorf("staleness{asset=crypto:BTC,quote=fiat:USD} = %v, want 0 — %s publishes directly every Tick", got, h.direct)
	}
	// The stamp is the Tick's injected clock, not a wall-clock read
	// taken inside the triangulation pass.
	if got := h.o.lastWriteAt[h.target.String()]; !got.Equal(h.clk.now) {
		t.Errorf("lastWriteAt[%s] = %v, want the Tick clock %v", h.target, got, h.clk.now)
	}
}

// TestTick_CompositeThatDoesNotPublishStillClimbs is the converse, and
// the reason the stamp sits AFTER the value write on the "ok" path
// only: a chain that resolves nothing, or resolves a composite it
// refuses to publish, has written no VWAP key, so the gauge must keep
// climbing for a target with no direct trades.
func TestTick_CompositeThatDoesNotPublishStillClimbs(t *testing.T) {
	liveFX := &fakeFXStore{quote: new(big.Rat).SetFrac(big.NewInt(90), big.NewInt(100)), source: "exchangeratesapi", observedAt: liveFXObservedAt}
	for _, tc := range []struct {
		name    string
		fx      FXStore
		minConf float64
		outcome string
		why     string
	}{
		// No FX row and no cached USD/EUR VWAP: the leg is dry.
		{"dry FX leg", &fakeFXStore{}, 0, "missing_leg", "the chain's FX leg is dry"},
		// A floor no route can clear: the composite resolves but is
		// refused as low-confidence and never written to the VWAP key.
		{"low confidence", liveFX, 1.5, "low_confidence", "the composite was refused as low-confidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCompositeStalenessHarness(t, tc.fx, tc.minConf)

			if n := h.tick(t, tc.outcome); n != 1 {
				t.Fatalf("precondition: first Tick ended in %s %v times, want 1", tc.outcome, n)
			}
			h.clk.now = h.clk.now.Add(10 * time.Minute)
			pairStaleness(h.target).Set(-1)
			okBefore := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
			if n := h.tick(t, tc.outcome); n != 1 {
				t.Fatalf("precondition: second Tick ended in %s %v times, want 1", tc.outcome, n)
			}
			if d := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok")) - okBefore; d != 0 {
				t.Fatalf("precondition: the composite published (%v) — this case must not publish", d)
			}

			if got := testutil.ToFloat64(pairStaleness(h.target)); got != 600 {
				t.Errorf("staleness{asset=crypto:BTC,quote=fiat:EUR} = %v, want 600 — %s and %s has no direct trades, "+
					"so nothing wrote its VWAP key for 10 min", got, tc.why, h.target)
			}
		})
	}
}

// TestTick_WedgedStoreCallIsCutAndTheNextTickRecovers covers the
// aggregator leg. Tick ran on the process-lifetime context with no
// deadline of its own, so one store call that stopped answering held
// the tick — and with it every price the aggregator publishes — until
// the process was restarted.
//
// No TickTimeout is set: the bound under test is the DEFAULT one, so
// this proves a production-shaped Config is protected, and the test
// compiles against code that predates the field.
func TestTick_WedgedStoreCallIsCutAndTheNextTickRecovers(t *testing.T) {
	btc, usd := mustCrypto(t, "BTC"), mustFiat(t, "USD")
	pair := mustStalenessPair(t, btc, usd)

	store := &wedgedStore{mockStore: &mockStore{perPair: map[string][]canonical.Trade{}}}
	store.setWedged(pair.String())
	cache, _ := newTestRedis(t)
	o := New(store, cache, Config{
		Pairs:    []canonical.Pair{pair},
		Windows:  []time.Duration{5 * time.Minute},
		Interval: 25 * time.Millisecond, // default wedge guard = 4 × this
	})

	// The caller's context stays live throughout; it is cancelled only
	// on the way out, to release the goroutine if the tick never returns.
	parent, stop := context.WithCancel(context.Background())
	defer stop()

	obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC", "fiat:USD").Set(-1)
	errTicksBefore := testutil.ToFloat64(obs.AggregatorTicksTotal.WithLabelValues("error"))
	done := make(chan error, 1)
	go func() { done <- o.Tick(parent) }()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Tick is still blocked on a store call that stopped answering — nothing bounds the tick, " +
			"so one wedged query stalls every price until the process is restarted")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Tick error = %v, want one wrapping context.DeadlineExceeded (the tick's own wedge guard)", err)
	}
	if parent.Err() != nil {
		t.Fatal("precondition: the caller's context must still be live — the tick has to be cut by its own deadline")
	}
	// A cut tick is an ERROR tick and still does its accounting: the
	// staleness gauge is emitted, not left at whatever it last read.
	if d := testutil.ToFloat64(obs.AggregatorTicksTotal.WithLabelValues("error")) - errTicksBefore; d != 1 {
		t.Errorf("ticks_total{outcome=error} delta = %v, want 1", d)
	}
	if got := testutil.ToFloat64(obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC", "fiat:USD")); got == -1 {
		t.Error("the cut tick did not emit the staleness gauge — a wedge would freeze it at its last reading")
	}

	// The store heals. The next tick has a fresh budget and publishes.
	store.setWedged("")
	store.perPair[pair.String()] = liveTrades(pair, time.Now().UTC())
	writes := o.Stats().VWAPWrites
	if err := o.Tick(parent); err != nil {
		t.Fatalf("Tick after the store healed: %v", err)
	}
	if o.Stats().VWAPWrites == writes {
		t.Error("the tick after the wedge cleared published nothing — the guard must not outlive the tick it cut")
	}
}

// TestTick_CallerCancellationIsNotATimeout — shutdown is unchanged: the
// caller's cancellation still ends the tick at once with
// context.Canceled (which Run deliberately does not log), and is not
// dressed up as a wedge.
func TestTick_CallerCancellationIsNotATimeout(t *testing.T) {
	pair := mustStalenessPair(t, mustCrypto(t, "BTC"), mustFiat(t, "USD"))
	cache, _ := newTestRedis(t)
	o := New(&mockStore{perPair: map[string][]canonical.Trade{}}, cache, Config{
		Pairs: []canonical.Pair{pair}, Windows: []time.Duration{5 * time.Minute},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := o.Tick(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Tick on a cancelled caller context = %v, want context.Canceled and not a deadline", err)
	}
}

// TestTick_EveryFXQueryInheritsTheTickDeadline pins the two FX query
// sites — the triangulation leg (triangulate.go legPrice) and
// the composite-reference evaluator (composite_reference.go) — to the
// tick's deadline. Neither sets one of its own; both are bounded only
// because Tick hands them a bounded context, so this is the test that
// fails if either is ever given a fresh one.
//
// The scenario is the market-wide move from the composite-reference
// suite: a single-venue XLM/GBP print jumps on tick 2, which is what
// sends the phase-2 freeze to consult the composite reference and so
// reach its FX query. The two sites are told apart by their cutoff: the
// triangulation leg asks at the window-aligned bucket end, the
// evaluator at the tick's own unaligned `now`.
func TestTick_EveryFXQueryInheritsTheTickDeadline(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	now := time.Now().UTC()
	// The DEFAULT wedge guard for the 1h Interval below (4 × Interval),
	// written as a literal so this file compiles against code that
	// predates the guard and fails there on behaviour, not on a symbol.
	const budget = 4 * time.Hour

	store := &mockStore{perPair: map[string][]canonical.Trade{}}
	cache, _ := newTestRedis(t)
	fx := &deadlineRecordingFX{observedAt: now.Add(-time.Hour)}
	o := New(store, cache, Config{
		Pairs:          []canonical.Pair{xlmGBP, xlmUSD},
		Windows:        []time.Duration{window},
		Interval:       time.Hour,
		Triangulations: []TriangulationChain{{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}},
		FXStore:        fx,
		FreezeWriter:   &recordingFreezeMarker{},
		Baselines: stubBaselineSource{
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: maxDay30Returns}},
			computedAt: now,
		},
		CompositeReference: CompositeReferenceConfig{Enabled: true, Targets: []canonical.Pair{xlmGBP}},
	})
	setTrades := func(legQuote, targetQuote int64, ts time.Time) {
		store.perPair[xlmUSD.String()] = []canonical.Trade{
			makeTradeOn(t, xlmUSD, "kraken", 100_000_000, legQuote, ts),
			makeTradeOn(t, xlmUSD, "coinbase", 100_000_000, legQuote, ts),
		}
		store.perPair[xlmGBP.String()] = []canonical.Trade{
			makeTradeOn(t, xlmGBP, "soroswap", 100_000_000, targetQuote, ts),
		}
	}

	setTrades(10_000_000, 8_000_000, now.Add(-30*time.Second))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	setTrades(15_000_000, 12_000_000, now.Add(-10*time.Second))
	nextBucket(o)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	var fromTriangulation, fromEvaluator int
	for i, c := range fx.calls {
		if c.cutoff.Equal(c.cutoff.Truncate(window)) {
			fromTriangulation++
		} else {
			fromEvaluator++
		}
		if !c.bounded {
			t.Errorf("FX query %d (cutoff %s) ran with NO deadline — it can hang the tick forever", i, c.cutoff)
			continue
		}
		if c.remaining <= 0 || c.remaining > budget {
			t.Errorf("FX query %d had %v left, want within the tick's %v budget", i, c.remaining, budget)
		}
	}
	if fromTriangulation == 0 {
		t.Error("precondition: the triangulation leg never queried the FX store — that site is unproven")
	}
	if fromEvaluator == 0 {
		t.Error("precondition: the composite-reference evaluator never queried the FX store — that site is unproven")
	}
}

// TestTick_CompositeRecordedOnlyOnPublish — the sample that feeds the
// confidence factor is written by the chain pass and ONLY on a
// successful publish. A chain that could not publish (missing leg here;
// a frozen leg takes the same path via outcomeFrozenLeg) must leave no
// sample behind for the confidence step to read as evidence.
func TestTick_CompositeRecordedOnlyOnPublish(t *testing.T) {
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
	})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	sample, ok := o.lastComposites[compositeKey(xlmEUR, window)]
	if !ok {
		t.Fatal("no composite recorded after a successful chain publish")
	}
	// 0.08 × 0.90 = 0.072 — the same value the chain wrote to cache.
	if sample.price.Cmp(ratOf(t, "0.072")) != 0 {
		t.Errorf("recorded composite = %v, want 0.072", sample.price.FloatString(6))
	}

	// Now break a leg: the next tick must not refresh the sample.
	mr.Del(cachekeys.VWAP(usdEUR.Base, usdEUR.Quote, window).String())
	before := o.lastComposites[compositeKey(xlmEUR, window)].at
	nextBucket(o)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got := o.lastComposites[compositeKey(xlmEUR, window)].at; !got.Equal(before) {
		t.Error("a missing-leg chain refreshed the composite sample — an unpublished " +
			"chain must not present itself as this tick's corroboration")
	}
}

// TestTick_CompositeCorroborationReachesTheCachedConfidence is the
// end-to-end wiring proof: a configured chain publishes a composite,
// and the NEXT tick's confidence score for that pair carries the
// composite comparison — checked, and lower when the two disagree.
//
// It also pins the invariant that makes this safe to ship: the
// source-count factor is identical in both runs. A composite is
// corroboration, never a second source, so it must not move the leg
// ADR-0019's 3-signal freeze AND reads (`source_count <= 1`).
func TestTick_CompositeCorroborationReachesTheCachedConfidence(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := time.Minute
	now := time.Now().UTC()

	// Direct VWAP for XLM/EUR from the trade fixture below: two sources,
	// quote/base = 1.242 and 1.245 → volume-weighted ≈ 1.2435.
	run := func(t *testing.T, legUSDEUR string) confidence.Score {
		t.Helper()
		store := &mockStore{
			trades: []canonical.Trade{
				makeTradeOn(t, xlmEUR, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
				makeTradeOn(t, xlmEUR, "phoenix", 1_000_000, 1_245_000, now.Add(-20*time.Second)),
			},
		}
		cache, mr := newTestRedis(t)
		mr.Set(cachekeys.VWAP(xlmUSD.Base, xlmUSD.Quote, window).String(), "1.000000000000")
		mr.Set(cachekeys.VWAP(usdEUR.Base, usdEUR.Quote, window).String(), legUSDEUR)

		o := New(store, cache, Config{
			Pairs:    []canonical.Pair{xlmEUR},
			Windows:  []time.Duration{window},
			Interval: time.Hour, // long enough that no sample ages out mid-test
			Triangulations: []TriangulationChain{
				{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
			},
			Baselines: stubBaselineSource{
				multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0.0001, MAD: 0.001, N: maxDay30Returns}},
				computedAt: now,
			},
		})
		// Tick 1 warms prevVWAP and publishes the first composite;
		// tick 2 scores the direct price against it.
		for i := 0; i < 2; i++ {
			if i > 0 {
				nextBucket(o)
			}
			if err := o.Tick(context.Background()); err != nil {
				t.Fatalf("tick %d: %v", i+1, err)
			}
		}
		body, err := cache.Get(context.Background(),
			cachekeys.Confidence(xlmEUR.Base, xlmEUR.Quote, window).String()).Bytes()
		if err != nil {
			t.Fatalf("confidence key missing: %v", err)
		}
		var score confidence.Score
		if err := json.Unmarshal(body, &score); err != nil {
			t.Fatalf("confidence not valid JSON: %v", err)
		}
		return score
	}

	// Composite = 1.0 × 1.2435 = the direct price → agreement.
	agree := run(t, "1.243500000000")
	// Composite = 1.0 × 1.75 → ~29% below the direct price.
	disagree := run(t, "1.750000000000")

	if !agree.Factors.TriangulationChecked || !disagree.Factors.TriangulationChecked {
		t.Fatalf("TriangulationChecked = (%v, %v), want both true — the chain published "+
			"a composite for this pair on the previous tick",
			agree.Factors.TriangulationChecked, disagree.Factors.TriangulationChecked)
	}
	if agree.Factors.TriangulationAgreement != 1.0 {
		t.Errorf("agreeing composite gave factor %v, want 1.0", agree.Factors.TriangulationAgreement)
	}
	if disagree.Factors.TriangulationAgreement >= 0.2 {
		t.Errorf("~29%% disagreement gave factor %v, want well under 0.2 — divergence "+
			"between a direct print and its composite is a manipulation signal",
			disagree.Factors.TriangulationAgreement)
	}
	if disagree.Confidence >= agree.Confidence {
		t.Errorf("disagreement did not lower confidence: %v (disagree) vs %v (agree)",
			disagree.Confidence, agree.Confidence)
	}
	if agree.Factors.SourceCount != disagree.Factors.SourceCount {
		t.Errorf("the composite moved the source-count factor (%v vs %v) — a derived "+
			"path must never count as a source for the freeze's 3-signal AND",
			agree.Factors.SourceCount, disagree.Factors.SourceCount)
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
		source:     "exchangeratesapi",
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

// TestTick_LongWindowVWAP_StopsServingAfterTheSilenceGrace checks, at
// the production entry point: the value the windowed /v1/price surface
// reads is written by Tick, and it must not outlive the aggregator.
//
// The scenario the finding describes: the aggregator stops (crash,
// deploy, OOM, failing Redis writes) at T0. Before this bound the
// `vwap:<pair>:86400` key lived for the WINDOW — 24 h — and every
// `GET /v1/price?asset=…&window=86400` in between returned HTTP 200
// with `observed_at` stamped at request time and `flags.stale` unset,
// i.e. a price computed the previous day asserted as current, with no
// field on the wire able to reveal it.
//
// Asserted through the real write path (Tick → refreshPairWindow →
// cachekeys.VWAPTTL) rather than against the TTL function alone, so a
// writer that stops honouring the bound fails here too.
func TestTick_LongWindowVWAP_StopsServingAfterTheSilenceGrace(t *testing.T) {
	pair := xlmUsdtPair(t)
	cache, mr := newTestRedis(t)
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(100_000_000), big.NewInt(21_000_000), time.Now()),
			buildTrade(t, big.NewInt(200_000_000), big.NewInt(42_000_000), time.Now()),
		},
	}

	// The 24 h window /v1/price?window=86400 serves.
	const window = 24 * time.Hour
	o := New(store, cache, Config{
		Pairs:   []canonical.Pair{pair},
		Windows: []time.Duration{window},
	})

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	key := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	if !mr.Exists(key) {
		t.Fatalf("precondition: Tick did not publish %s", key)
	}
	if ttl := mr.TTL(key); ttl > cachekeys.VWAPMaxAge {
		t.Errorf("TTL(%s) = %v, want ≤ the %v silence grace — a 24h window is an "+
			"aggregation span, not a freshness claim; this key is re-written every tick",
			key, ttl, cachekeys.VWAPMaxAge)
	}

	// The aggregator stops here: no further ticks. One grace later the
	// value must be gone, so the windowed handler answers its
	// documented 404 instead of stamping observed_at=now on it.
	mr.FastForward(cachekeys.VWAPMaxAge + time.Minute)
	if mr.Exists(key) {
		v, _ := mr.Get(key)
		t.Errorf("%s still serves %q %v after the last publish — a stopped aggregator's "+
			"VWAP must expire, not be served as a current price",
			key, v, cachekeys.VWAPMaxAge+time.Minute)
	}

	// And a live aggregator keeps it alive: one more tick re-publishes
	// the key, so the bound costs nothing while the writer is running.
	nextBucket(o)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick after grace: %v", err)
	}
	if !mr.Exists(key) {
		t.Errorf("%s missing after a fresh tick — the bound must not stop a running "+
			"aggregator from serving", key)
	}
}

// TestTick_ShortWindowVWAP_KeepsItsWindowTTL — the bound only ever
// TIGHTENS. The 5 m window is already inside the grace and must keep
// its own window as the TTL.
func TestTick_ShortWindowVWAP_KeepsItsWindowTTL(t *testing.T) {
	pair := xlmUsdtPair(t)
	cache, mr := newTestRedis(t)
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(100_000_000), big.NewInt(21_000_000), time.Now()),
		},
	}

	const window = 5 * time.Minute
	o := New(store, cache, Config{
		Pairs:   []canonical.Pair{pair},
		Windows: []time.Duration{window},
	})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	key := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	if ttl := mr.TTL(key); ttl != window {
		t.Errorf("TTL(%s) = %v, want the untouched %v window TTL", key, ttl, window)
	}
}

// TestTick_WindowAtTheRowCapCountsAsTruncated pins the truncation
// detector: a window whose fetch comes back at MaxTradesPerWindow rows
// was cut by the store's LIMIT, and operators only learn that from
// AggregatorWindowTruncatedTotal. A window under the cap must not count.
func TestTick_WindowAtTheRowCapCountsAsTruncated(t *testing.T) {
	const maxRows = 3
	now := time.Now()
	fiveTrades := make([]canonical.Trade, 5)
	for i := range fiveTrades {
		fiveTrades[i] = buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000),
			now.Add(-time.Duration(5-i)*time.Minute/10))
	}

	for _, tc := range []struct {
		name   string
		trades []canonical.Trade
		want   float64
	}{
		{"over the cap", fiveTrades, 1},
		{"under the cap", fiveTrades[:maxRows-1], 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockStore{trades: tc.trades}
			rdb, _ := newTestRedis(t)
			orch := New(store, rdb, Config{
				Pairs:              []canonical.Pair{xlmUsdtPair(t)},
				Windows:            []time.Duration{5 * time.Minute},
				MaxTradesPerWindow: maxRows,
			})

			before := testutil.ToFloat64(obs.AggregatorWindowTruncatedTotal)
			if err := orch.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			if store.lastLimit != maxRows {
				t.Fatalf("store fetched with limit %d, want the configured cap %d", store.lastLimit, maxRows)
			}
			if got := testutil.ToFloat64(obs.AggregatorWindowTruncatedTotal) - before; got != tc.want {
				t.Errorf("AggregatorWindowTruncatedTotal advanced by %v, want %v", got, tc.want)
			}
		})
	}
}

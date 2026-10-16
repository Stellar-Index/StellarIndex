package orchestrator

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

func pivotTrade(pair canonical.Pair, source string, base, quote int64, ts time.Time) canonical.Trade {
	return canonical.Trade{
		Source:      source,
		Ledger:      1,
		TxHash:      "1111111111111111111111111111111111111111111111111111111111111111",
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

// runProxyPivotTick prices crypto:XLM/fiat:GBP via [XLM/USD, USD/GBP 0.79], the XLM/USD leg being 6 XLM at 0.40 real USD (coinbase, 8dp) plus 4 XLM quoted in USDC on SDEX (7dp) folded in at par by the proxy; direct XLM/GBP prints 0.32. ownUSD=false drops the real-USD print. Returns the served value, composite_meta and proxy_pivot/ok deltas.
func runProxyPivotTick(t *testing.T, usdcQuote7dp int64, ownUSD bool) (string, compositeMeta, float64, float64) {
	t.Helper()
	now := time.Now()
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	xlmUSDC := mkPair(t, "crypto", "XLM", "crypto", "USDC")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	window := 5 * time.Minute

	store := &mockStore{perPair: map[string][]canonical.Trade{
		xlmUSD.String():  {pivotTrade(xlmUSD, "coinbase", 600_000_000, 240_000_000, now.Add(-2*time.Minute))},
		xlmUSDC.String(): {pivotTrade(xlmUSDC, "sdex", 40_000_000, usdcQuote7dp, now.Add(-90*time.Second))},
		xlmGBP.String():  {pivotTrade(xlmGBP, "coinbase", 100_000_000, 32_000_000, now.Add(-time.Minute))},
	}}
	if !ownUSD {
		delete(store.perPair, xlmUSD.String())
	}
	rdb, mr := newTestRedis(t)
	o := New(store, rdb, Config{
		Pairs:                     []canonical.Pair{xlmUSD, xlmGBP},
		Windows:                   []time.Duration{window},
		EnableStablecoinFiatProxy: true,
		Triangulations:            []TriangulationChain{{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}},
		FXStore: &fakeFXStore{
			quote:      big.NewRat(79, 100),
			observedAt: now.UTC().Add(-time.Minute),
			source:     "exchangeratesapi",
		},
	})

	beforePivot := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeProxyPivot))
	beforeOK := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok"))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	pivotDelta := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeProxyPivot)) - beforePivot
	okDelta := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues("ok")) - beforeOK

	served, err := mr.Get(cachekeys.VWAP(xlmGBP.Base, xlmGBP.Quote, window).String())
	if err != nil {
		t.Fatalf("served XLM/GBP: %v", err)
	}
	raw, err := mr.Get(cachekeys.VWAPCompositeMeta(xlmGBP.Base, xlmGBP.Quote, window).String())
	if err != nil {
		t.Fatalf("composite_meta: %v", err)
	}
	var meta compositeMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("decode composite_meta %q: %v", raw, err)
	}
	return served, meta, pivotDelta, okDelta
}

// TestTriangulation_ProxyPivotDepegRefusesComposite: a USDC de-peg (0.97) must not reach the composite at par; the leg's quote surfaces disagree by ~309 bps, so the composite (0.319909) is refused and the 0.32 direct print serves.
func TestTriangulation_ProxyPivotDepegRefusesComposite(t *testing.T) {
	served, meta, pivotDelta, okDelta := runProxyPivotTick(t, 16_494_800, true)

	if served != "0.320000000000" {
		t.Errorf("served XLM/GBP = %q, want the direct 0.320000000000 (the par-merged composite is 0.319909…)", served)
	}
	if pivotDelta != 1 || okDelta != 0 {
		t.Errorf("outcome deltas proxy_pivot=%v ok=%v, want 1 and 0", pivotDelta, okDelta)
	}
	if !strings.HasPrefix(meta.PivotSurfaceRefusal, "crypto:XLM/fiat:USD quote_surface=309.") {
		t.Errorf("pivot_surface_refusal = %q, want the XLM/USD leg at ~309 bps", meta.PivotSurfaceRefusal)
	}
	if got := meta.PivotProxyShare["crypto:XLM/fiat:USD"]; got != 0.4 {
		t.Errorf("pivot_proxy_share[XLM/USD] = %v, want 0.4 (4 of 10 XLM priced in USDC)", got)
	}
}

// USDC at par: the surfaces agree, the composite (0.40 × 0.79 = 0.316)
// publishes, and its composite_meta still carries the leg's proxy share.
func TestTriangulation_ProxyPivotAtParPublishesAndStampsShare(t *testing.T) {
	served, meta, pivotDelta, okDelta := runProxyPivotTick(t, 16_000_000, true)

	if served != "0.316000000000" {
		t.Errorf("served XLM/GBP = %q, want the composite 0.316000000000", served)
	}
	if pivotDelta != 0 || okDelta != 1 {
		t.Errorf("outcome deltas proxy_pivot=%v ok=%v, want 0 and 1", pivotDelta, okDelta)
	}
	if meta.PivotSurfaceRefusal != "" {
		t.Errorf("pivot_surface_refusal = %q, want empty at par", meta.PivotSurfaceRefusal)
	}
	if got := meta.PivotProxyShare["crypto:XLM/fiat:USD"]; got != 0.4 {
		t.Errorf("pivot_proxy_share[XLM/USD] = %v, want 0.4", got)
	}
	if meta.PivotUnverified {
		t.Error("pivot_unverified = true, want false: the leg's own-quote prints verified the par")
	}
}

// A leg made only of USDC prints has no own-quote surface to check the
// par against, so a de-peg there cannot be refused: the composite still
// publishes, and its composite_meta must say the pivot is unverified.
func TestTriangulation_ProxyPivotAllProxyLegFlagsUnverified(t *testing.T) {
	served, meta, pivotDelta, okDelta := runProxyPivotTick(t, 16_000_000, false)

	if served != "0.316000000000" {
		t.Errorf("served XLM/GBP = %q, want the composite 0.316000000000", served)
	}
	if pivotDelta != 0 || okDelta != 1 {
		t.Errorf("outcome deltas proxy_pivot=%v ok=%v, want 0 and 1", pivotDelta, okDelta)
	}
	if got := meta.PivotProxyShare["crypto:XLM/fiat:USD"]; got != 1 {
		t.Errorf("pivot_proxy_share[XLM/USD] = %v, want 1", got)
	}
	if !meta.PivotUnverified {
		t.Error("pivot_unverified = false, want true: no own-quote prints verified the par")
	}
}

// The composite REFERENCE reads the same leg: two agreeing venues do not
// make a leg one USD when its stablecoin and own-quote prints disagree,
// so the leg cannot corroborate a direct print either.
func TestReferenceLeg_QuoteSurfaceDisagreementCannotCorroborate(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	window := 5 * time.Minute
	cache, _ := newTestRedis(t)
	o := New(nil, cache, Config{Windows: []time.Duration{window}})
	cfg := CompositeReferenceConfig{}.withDefaults()

	cases := []struct {
		name string
		lr   legRef
		want string
	}{
		{"agreeing surfaces", legRef{surfaceDivergence: big.NewRat(75, 10_000)}, ""},
		{"de-pegged surface", legRef{surfaceDivergence: big.NewRat(309, 10_000)}, "leg_quote_surface=309.0bps"},
		{"uncomputable surface", legRef{surfaceUncomputable: true}, "leg_quote_surface=uncomputable"},
	}
	for _, tc := range cases {
		tc.lr.price, tc.lr.sources = big.NewRat(40, 100), 2
		o.setTickLegRef(xlmUSD, window, tc.lr)
		_, _, why := o.referenceLeg(context.Background(), xlmUSD, window, time.Now(), cfg)
		if why != tc.want {
			t.Errorf("%s: referenceLeg refusal = %q, want %q", tc.name, why, tc.want)
		}
	}
}

// Each source pair is capped at its own newest N, so a sparse stablecoin pair reaches back hours past a dense own-quote pair. The bucket
// keeps only the span every read covers; otherwise intraday drift (or an old de-peg) enters the VWAP from one side only.
func TestFetchForTarget_ClipsBackersToTheCappedSpan(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	xlmUSDT := mkPair(t, "crypto", "XLM", "crypto", "USDT")
	now := time.Now().UTC()
	tr := func(pair canonical.Pair, src string, quote int64, ago time.Duration, n string) canonical.Trade {
		x := pivotTrade(pair, src, 100_000_000, quote, now.Add(-ago))
		x.TxHash = strings.Repeat(n, 64)
		return x
	}
	store := &mockStore{perPair: map[string][]canonical.Trade{
		xlmUSD.String(): {
			tr(xlmUSD, "coinbase", 18_000_000, 6*time.Hour, "1"),
			tr(xlmUSD, "coinbase", 18_400_000, 2*time.Hour, "2"),
			tr(xlmUSD, "coinbase", 18_400_000, time.Hour, "3"),
		},
		xlmUSDT.String(): {
			tr(xlmUSDT, "binance", 17_000_000, 10*time.Hour, "4"), // a de-peg print, older than the USD read reaches
			tr(xlmUSDT, "binance", 18_400_000, 90*time.Minute, "5"),
		},
	}}
	cache, _ := newTestRedis(t)
	o := New(store, cache, Config{EnableStablecoinFiatProxy: true, MaxTradesPerWindow: 2})

	trades, _, proxied, coverage, err := o.fetchForTarget(context.Background(), xlmUSD, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-2 * time.Hour); !coverage.Truncated || !coverage.CoveredFrom.Equal(want) {
		t.Fatalf("coverage = %+v, want truncated from %v", coverage, want)
	}
	if len(trades) != 3 {
		t.Fatalf("kept %d trades, want 3 (two USD, one USDT inside the span)", len(trades))
	}
	for _, x := range trades {
		if x.Timestamp.Before(coverage.CoveredFrom) {
			t.Errorf("kept a %s print from %v, before the covered span", x.Source, x.Timestamp)
		}
	}
	lr := o.newLegRef(xlmUSD, big.NewRat(184, 1000), trades, proxied)
	if why := lr.quoteSurfaceRefusal(75); why != "" {
		t.Errorf("surface refusal = %q, want none: inside the span both surfaces print 0.184", why)
	}
}

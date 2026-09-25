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

// runProxyPivotTick prices crypto:XLM/fiat:GBP through
// [XLM/fiat:USD, USD/GBP 0.79] where the XLM/USD leg is 6 XLM @ 0.40 real
// USD (coinbase, 8dp) plus 4 XLM quoted in USDC on SDEX (7dp) at
// usdcPrice, folded in at par by the stablecoin-fiat proxy. The direct
// XLM/GBP market prints 0.32 real GBP. Returns the served XLM/GBP value,
// its composite_meta and the proxy_pivot / ok outcome deltas.
func runProxyPivotTick(t *testing.T, usdcQuote7dp int64) (string, compositeMeta, float64, float64) {
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

// A USDC de-peg must not reach the published composite at par: XLM is
// 0.40 real USD but 0.41237 USDC (USDC at 0.97). The par-merged leg VWAP
// is 0.404948, × 0.79 = 0.319909 GBP — which would overwrite the correct
// 0.32 direct print unflagged. The leg's two quote surfaces disagree by
// ~309 bps, so the composite is refused and the direct print serves.
func TestTriangulation_ProxyPivotDepegRefusesComposite(t *testing.T) {
	served, meta, pivotDelta, okDelta := runProxyPivotTick(t, 16_494_800)

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
	served, meta, pivotDelta, okDelta := runProxyPivotTick(t, 16_000_000)

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

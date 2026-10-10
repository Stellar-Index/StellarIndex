package aggregate

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// stubGlobalReader fakes the three storage seams ComputeGlobalPrice
// reads against. Tests configure the returns per-tier and assert
// which tier won.
type stubGlobalReader struct {
	vwap struct {
		price      string
		asOf       time.Time
		tradeCount int64
		sources    []string
		ok         bool
		err        error
	}
	agg struct {
		rows []canonical.OracleUpdate
		err  error
	}
	tri struct {
		price string
		asOf  time.Time
		ok    bool
		err   error
	}

	// call counters — let tests verify higher tiers short-circuit
	// without invoking the lower ones.
	vwapCalls, aggCalls, triCalls int
}

func (s *stubGlobalReader) LatestVWAP(_ context.Context, _, _ canonical.Asset) (string, time.Time, int64, []string, bool, error) {
	s.vwapCalls++
	return s.vwap.price, s.vwap.asOf, s.vwap.tradeCount, s.vwap.sources, s.vwap.ok, s.vwap.err
}

func (s *stubGlobalReader) LatestAggregatorPrices(_ context.Context, _, _ canonical.Asset, _ []string) ([]canonical.OracleUpdate, error) {
	s.aggCalls++
	return s.agg.rows, s.agg.err
}

func (s *stubGlobalReader) LookupTriangulated(_ context.Context, _, _ canonical.Asset, _ time.Duration) (string, time.Time, bool, error) {
	s.triCalls++
	return s.tri.price, s.tri.asOf, s.tri.ok, s.tri.err
}

func usdcUSDPair(t *testing.T) (canonical.Asset, canonical.Asset) {
	t.Helper()
	base, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("classic: %v", err)
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}
	return base, quote
}

func TestComputeGlobalPrice_VWAPTierWins(t *testing.T) {
	reader := &stubGlobalReader{}
	reader.vwap.price = "1.00050000000000"
	reader.vwap.asOf = time.Now().UTC()
	reader.vwap.tradeCount = 12
	reader.vwap.sources = []string{"coinbase", "binance"}
	reader.vwap.ok = true

	base, quote := usdcUSDPair(t)
	res, err := ComputeGlobalPrice(context.Background(), base, quote, reader, DefaultGlobalPriceOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != AuthorityVWAPNative {
		t.Errorf("authority = %q, want vwap_native", res.Authority)
	}
	if res.Price != "1.00050000000000" {
		t.Errorf("price = %q, want 1.00050000000000", res.Price)
	}
	if res.TradeCount != 12 {
		t.Errorf("trade_count = %d, want 12", res.TradeCount)
	}
	// Should short-circuit — no aggregator or triangulated call.
	if reader.aggCalls != 0 || reader.triCalls != 0 {
		t.Errorf("higher tiers should short-circuit; agg=%d tri=%d", reader.aggCalls, reader.triCalls)
	}
}

func TestComputeGlobalPrice_VWAPBelowThreshold_FallsThrough(t *testing.T) {
	// VWAP exists but trade_count=3 < default 5 threshold → fall
	// through to aggregator tier.
	reader := &stubGlobalReader{}
	reader.vwap.price = "0.99000000000000"
	reader.vwap.tradeCount = 3
	reader.vwap.ok = true

	price, _ := new(big.Int).SetString("100000000", 10) // 1.00 @ 8dp
	reader.agg.rows = []canonical.OracleUpdate{
		{
			Source:    "coingecko",
			Timestamp: time.Now().UTC(),
			Price:     canonical.NewAmount(price),
			Decimals:  8,
		},
		{
			Source:    "coinmarketcap",
			Timestamp: time.Now().UTC().Add(-30 * time.Second),
			Price:     canonical.NewAmount(price),
			Decimals:  8,
		},
	}

	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko", "coinmarketcap"}
	res, err := ComputeGlobalPrice(context.Background(), base, quote, reader, opts)
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != AuthorityAggregatorAvg {
		t.Errorf("authority = %q, want aggregator_avg", res.Authority)
	}
	// 100M @ 8dp scales to 10_000_000_000_000 @ 14dp → "1.00000000000000".
	if res.Price != "1.00000000000000" {
		t.Errorf("price = %q, want 1.00000000000000", res.Price)
	}
	if len(res.Sources) != 2 {
		t.Errorf("sources = %v, want 2", res.Sources)
	}
	// Triangulation never called.
	if reader.triCalls != 0 {
		t.Errorf("triangulated tier should short-circuit; calls=%d", reader.triCalls)
	}
}

func TestComputeGlobalPrice_AggregatorStale_FallsThrough(t *testing.T) {
	// VWAP misses, aggregator rows exist but are all older than
	// MaxAggregatorAge → fall through to triangulated.
	reader := &stubGlobalReader{}
	reader.vwap.ok = false

	price, _ := new(big.Int).SetString("100000000", 10)
	reader.agg.rows = []canonical.OracleUpdate{
		{
			Source:    "coingecko",
			Timestamp: time.Now().UTC().Add(-1 * time.Hour), // stale
			Price:     canonical.NewAmount(price),
			Decimals:  8,
		},
	}

	reader.tri.price = "0.99875000000000"
	reader.tri.asOf = time.Now().UTC()
	reader.tri.ok = true

	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko"}
	res, err := ComputeGlobalPrice(context.Background(), base, quote, reader, opts)
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != AuthorityTriangulated {
		t.Errorf("authority = %q, want triangulated", res.Authority)
	}
	if res.Price != "0.99875000000000" {
		t.Errorf("price = %q, want 0.99875000000000", res.Price)
	}
}

func TestComputeGlobalPrice_AllTiersMiss(t *testing.T) {
	reader := &stubGlobalReader{}
	reader.vwap.ok = false
	reader.tri.ok = false

	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko"}
	_, err := ComputeGlobalPrice(context.Background(), base, quote, reader, opts)
	if !errors.Is(err, ErrNoPrice) {
		t.Errorf("err = %v, want ErrNoPrice", err)
	}
}

func TestComputeGlobalPrice_VWAPErrorPropagates(t *testing.T) {
	// A storage failure in tier 1 must NOT silently degrade to
	// tier 2 — the operator wants to see the failure and the
	// aggregator tier might mask a broken VWAP path.
	reader := &stubGlobalReader{}
	reader.vwap.err = errors.New("simulated DB failure")

	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko"}
	_, err := ComputeGlobalPrice(context.Background(), base, quote, reader, opts)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Lower tiers must not have been called.
	if reader.aggCalls != 0 || reader.triCalls != 0 {
		t.Errorf("error should short-circuit lower tiers; agg=%d tri=%d", reader.aggCalls, reader.triCalls)
	}
}

func TestComputeGlobalPrice_NoAggregatorsConfigured_SkipsTier(t *testing.T) {
	// When opts.AggregatorSources is empty, tier 2 is skipped
	// entirely — tier 1 falls straight through to tier 3.
	reader := &stubGlobalReader{}
	reader.vwap.ok = false
	reader.tri.price = "0.500"
	reader.tri.ok = true

	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = nil
	res, err := ComputeGlobalPrice(context.Background(), base, quote, reader, opts)
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != AuthorityTriangulated {
		t.Errorf("authority = %q, want triangulated", res.Authority)
	}
	if reader.aggCalls != 0 {
		t.Errorf("aggregator tier should be skipped when no sources configured; calls=%d", reader.aggCalls)
	}
}

func TestComputeGlobalPrice_NilReader_Errors(t *testing.T) {
	base, quote := usdcUSDPair(t)
	_, err := ComputeGlobalPrice(context.Background(), base, quote, nil, DefaultGlobalPriceOptions())
	if err == nil {
		t.Fatal("expected error for nil reader")
	}
}

func TestAverageAggregatorPrices_DifferentDecimals(t *testing.T) {
	// Verify the cross-decimal scaling works: CG-style 8dp +
	// hypothetical 6dp source averaging to a sensible value.
	price8, _ := new(big.Int).SetString("100050000", 10) // 1.00050 @ 8dp
	price6, _ := new(big.Int).SetString("999000", 10)    // 0.99900 @ 6dp

	rows := []canonical.OracleUpdate{
		{Source: "a", Timestamp: time.Now(), Price: canonical.NewAmount(price8), Decimals: 8},
		{Source: "b", Timestamp: time.Now(), Price: canonical.NewAmount(price6), Decimals: 6},
	}
	avg, _, ok := averageAggregatorPrices(rows)
	if !ok {
		t.Fatal("averageAggregatorPrices: ok=false")
	}
	// avg = (1.00050 + 0.99900) / 2 = 0.99975
	if avg != "0.99975000000000" {
		t.Errorf("avg = %q, want 0.99975000000000", avg)
	}
}

func sourceSet(rows []canonical.OracleUpdate) map[string]bool {
	m := make(map[string]bool, len(rows))
	for _, r := range rows {
		m[r.Source] = true
	}
	return m
}

func TestAverageAggregatorPrices_RejectsZeroPrices(t *testing.T) {
	zero := big.NewInt(0)
	rows := []canonical.OracleUpdate{
		{Source: "a", Timestamp: time.Now(), Price: canonical.NewAmount(zero), Decimals: 8},
	}
	_, _, ok := averageAggregatorPrices(rows)
	if ok {
		t.Error("all-zero-prices input must return ok=false")
	}
}

// A source published above 14 dp must not be truncated before it is
// averaged, and a positive mean below 1e-14 must miss rather than serve
// "0.00000000000000" as a price.
func TestAverageAggregatorPrices_AboveCommonDecimals(t *testing.T) {
	row := func(src string, price int64, dec uint8) canonical.OracleUpdate {
		return canonical.OracleUpdate{Source: src, Timestamp: time.Now(), Price: canonical.NewAmount(big.NewInt(price)), Decimals: dec}
	}

	// (1.5e-14 + 0.5e-14) / 2 = 1e-14 exactly.
	avg, _, ok := averageAggregatorPrices([]canonical.OracleUpdate{row("a", 15, 15), row("b", 5, 15)})
	if !ok || avg != "0.00000000000001" {
		t.Fatalf("avg = (%q, %v), want (\"0.00000000000001\", true)", avg, ok)
	}

	if avg, _, ok := averageAggregatorPrices([]canonical.OracleUpdate{row("a", 1, 20)}); ok {
		t.Fatalf("a 1e-20 mean rendered as %q with ok=true; want ok=false", avg)
	}
}

// The mean is rounded half-up once at 14 dp, not floored: a mean whose
// 15th decimal is 5 rounds up, and one just below it rounds down.
func TestAverageAggregatorPrices_RoundsHalfUp(t *testing.T) {
	row := func(src string, price int64, dec uint8) canonical.OracleUpdate {
		return canonical.OracleUpdate{Source: src, Timestamp: time.Now(), Price: canonical.NewAmount(big.NewInt(price)), Decimals: dec}
	}
	cases := []struct {
		name string
		rows []canonical.OracleUpdate
		want string
	}{
		// (1.00000000000001 + 1.00000000000002) / 2 = 1.000000000000015
		{"tie at 15th decimal", []canonical.OracleUpdate{row("a", 100_000_000_000_001, 14), row("b", 100_000_000_000_002, 14)}, "1.00000000000002"},
		// 1.0000000000000149 sits just below the tie.
		{"below tie", []canonical.OracleUpdate{row("a", 10_000_000_000_000_149, 16), row("b", 10_000_000_000_000_149, 16)}, "1.00000000000001"},
		// 0.6e-14 is a positive mean that rounds to one unit, not zero.
		{"sub-unit mean", []canonical.OracleUpdate{row("a", 6, 15)}, "0.00000000000001"},
	}
	for _, tc := range cases {
		avg, _, ok := averageAggregatorPrices(tc.rows)
		if !ok || avg != tc.want {
			t.Errorf("%s: avg = (%q, %v), want (%q, true)", tc.name, avg, ok, tc.want)
		}
	}
}

// aliasAwareReader returns a VWAP keyed by the exact base form, so a
// test can prove tryVWAPTier loops the asset aliases. Only the base
// listed in `byBase` returns a hit; every other form misses.
type aliasAwareReader struct {
	byBase    map[string]int64 // base.String() → tradeCount
	vwapCalls []string         // base forms queried, in order
}

func (r *aliasAwareReader) LatestVWAP(_ context.Context, base, _ canonical.Asset) (string, time.Time, int64, []string, bool, error) {
	r.vwapCalls = append(r.vwapCalls, base.String())
	if tc, ok := r.byBase[base.String()]; ok {
		return "0.12340000000000", time.Now().UTC(), tc, []string{"binance"}, true, nil
	}
	return "", time.Time{}, 0, nil, false, nil
}

func (r *aliasAwareReader) LatestAggregatorPrices(_ context.Context, _, _ canonical.Asset, _ []string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (r *aliasAwareReader) LookupTriangulated(_ context.Context, _, _ canonical.Asset, _ time.Duration) (string, time.Time, bool, error) {
	return "", time.Time{}, false, nil
}

// TestComputeGlobalPrice_VWAPTierLoopsAliases pins alias looping:
// the global view must find the XLM VWAP regardless of which
// canonical form (`native` vs `crypto:XLM`) the configured pair set
// publishes under. A tryVWAPTier querying only the literal base
// would miss when the caller passes `native` but the VWAP lives under
// `crypto:XLM`, and the view would degrade to
// aggregator_avg.
func TestComputeGlobalPrice_VWAPTierLoopsAliases(t *testing.T) {
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}

	// VWAP only exists under crypto:XLM, but the caller queries native.
	reader := &aliasAwareReader{byBase: map[string]int64{"crypto:XLM": 20}}
	res, err := ComputeGlobalPrice(context.Background(), canonical.NativeAsset(), quote, reader, DefaultGlobalPriceOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != AuthorityVWAPNative {
		t.Errorf("authority = %q, want vwap_native (alias loop should find the crypto:XLM VWAP)", res.Authority)
	}
	if res.TradeCount != 20 {
		t.Errorf("trade_count = %d, want 20", res.TradeCount)
	}
	// The literal form must be tried first, then the alias.
	if len(reader.vwapCalls) != 2 || reader.vwapCalls[0] != "native" || reader.vwapCalls[1] != "crypto:XLM" {
		t.Errorf("alias query order = %v, want [native crypto:XLM]", reader.vwapCalls)
	}
}

// TestComputeGlobalPrice_VWAPTierAliasUnderThreshold — an alias hit
// that's below the trade-count floor must NOT win the VWAP tier; the
// loop keeps trying and ultimately falls through to a lower tier.
func TestComputeGlobalPrice_VWAPTierAliasUnderThreshold(t *testing.T) {
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}
	// crypto:XLM has a VWAP but only 2 trades < default floor of 5.
	reader := &aliasAwareReader{byBase: map[string]int64{"crypto:XLM": 2}}
	_, err = ComputeGlobalPrice(context.Background(), canonical.NativeAsset(), quote, reader, DefaultGlobalPriceOptions())
	if !errors.Is(err, ErrNoPrice) {
		t.Errorf("err = %v, want ErrNoPrice (alias below threshold, no other tier)", err)
	}
}

// aliasTierReader keys the aggregator and triangulated seams by the
// exact base form queried, so a test can prove tryAggregatorTier and
// tryTriangulatedTier walk the asset's alias family (not just the
// literal base). VWAP always misses so the fallback tiers run.
type aliasTierReader struct {
	aggByBase map[string][]canonical.OracleUpdate
	triByBase map[string]string
	aggBases  []string // aggregator base forms queried, in order
	triBases  []string // triangulation base forms queried, in order
}

func (r *aliasTierReader) LatestVWAP(_ context.Context, _, _ canonical.Asset) (string, time.Time, int64, []string, bool, error) {
	return "", time.Time{}, 0, nil, false, nil
}

func (r *aliasTierReader) LatestAggregatorPrices(_ context.Context, base, _ canonical.Asset, _ []string) ([]canonical.OracleUpdate, error) {
	r.aggBases = append(r.aggBases, base.String())
	return r.aggByBase[base.String()], nil
}

func (r *aliasTierReader) LookupTriangulated(_ context.Context, base, _ canonical.Asset, _ time.Duration) (string, time.Time, bool, error) {
	r.triBases = append(r.triBases, base.String())
	if p, ok := r.triByBase[base.String()]; ok {
		return p, time.Now().UTC(), true, nil
	}
	return "", time.Time{}, false, nil
}

// TestComputeGlobalPrice_AggregatorTierLoopsAliases pins that the
// aggregator tier must find the headline price when the base's
// aggregator coverage lives under an alias form (here crypto:XLM) and
// the caller queries native. A tryAggregatorTier querying only
// the literal base makes a `native` query miss the crypto:XLM
// aggregator average and degrade to triangulated / ErrNoPrice.
func TestComputeGlobalPrice_AggregatorTierLoopsAliases(t *testing.T) {
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}
	price, _ := new(big.Int).SetString("12340000", 10) // 0.12340000 @ 8dp
	reader := &aliasTierReader{
		aggByBase: map[string][]canonical.OracleUpdate{
			"crypto:XLM": {{
				Source:    "coingecko",
				Timestamp: time.Now().UTC(),
				Price:     canonical.NewAmount(price),
				Decimals:  8,
			}},
		},
	}
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko"}
	res, err := ComputeGlobalPrice(context.Background(), canonical.NativeAsset(), quote, reader, opts)
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != AuthorityAggregatorAvg {
		t.Errorf("authority = %q, want aggregator_avg (alias loop should find the crypto:XLM average)", res.Authority)
	}
	if res.Price != "0.12340000000000" {
		t.Errorf("price = %q, want 0.12340000000000", res.Price)
	}
	// Literal form tried first, then the alias.
	if len(reader.aggBases) < 2 || reader.aggBases[0] != "native" || reader.aggBases[1] != "crypto:XLM" {
		t.Errorf("aggregator query order = %v, want native then crypto:XLM", reader.aggBases)
	}
}

// TestComputeGlobalPrice_TriangulatedTierLoopsAliasesSACLast pins
// the alias loop for tier 3: the triangulated tier must reach the SAC form,
// and only as the LAST resort. Calling LookupTriangulated
// once with the literal base means a `native` query never sees a bridge
// path published under the SAC form.
func TestComputeGlobalPrice_TriangulatedTierLoopsAliasesSACLast(t *testing.T) {
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}
	reader := &aliasTierReader{
		triByBase: map[string]string{canonical.XLMSacContractID: "0.98765000000000"},
	}
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko"} // configured, but no rows anywhere
	res, err := ComputeGlobalPrice(context.Background(), canonical.NativeAsset(), quote, reader, opts)
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != AuthorityTriangulated {
		t.Errorf("authority = %q, want triangulated", res.Authority)
	}
	if res.Price != "0.98765000000000" {
		t.Errorf("price = %q, want 0.98765000000000", res.Price)
	}
	wantOrder := []string{"native", "crypto:XLM", canonical.XLMSacContractID}
	if len(reader.triBases) != len(wantOrder) {
		t.Fatalf("triangulation query order = %v, want %v (SAC reached last)", reader.triBases, wantOrder)
	}
	for i := range wantOrder {
		if reader.triBases[i] != wantOrder[i] {
			t.Errorf("triangulation query[%d] = %q, want %q", i, reader.triBases[i], wantOrder[i])
		}
	}
}

// TestAssetAliases pins that this package's wrapper is a pure
// delegation to [canonical.AssetAliases] . The alias TABLE itself is tested once, in
// canonical — the whole point of the hoist is that there is no second
// copy here to drift. What this asserts is the property tryVWAPTier
// depends on: the literal comes first and the SAC form comes LAST, so
// the VWAP tier can only land on a thin Soroban pool after both SDEX
// (`native`) and CEX (`crypto:XLM`) have missed.
func TestAssetAliases(t *testing.T) {
	for _, in := range []string{"native", "crypto:XLM", canonical.XLMSacContractID} {
		a, err := canonical.ParseAsset(in)
		if err != nil {
			t.Fatalf("parse %s: %v", in, err)
		}
		got, want := assetAliases(a), canonical.AssetAliases(a)
		if len(got) != len(want) {
			t.Fatalf("assetAliases(%s) len = %d, want %d (must delegate)", in, len(got), len(want))
		}
		for i := range want {
			if !got[i].Equal(want[i]) {
				t.Errorf("assetAliases(%s)[%d] = %q, want %q (must delegate)",
					in, i, got[i].String(), want[i].String())
			}
		}
		if got[0].String() != in {
			t.Errorf("assetAliases(%s)[0] = %q, want the literal input first", in, got[0].String())
		}
	}

	// The manipulation-surface ordering, stated directly: a `native`
	// read reaches the SAC form only as the LAST resort.
	got := assetAliases(canonical.NativeAsset())
	if len(got) != 3 || got[len(got)-1].String() != canonical.XLMSacContractID {
		t.Errorf("assetAliases(native) = %v, want the XLM SAC form last", got)
	}

	// A non-XLM asset returns only itself.
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("classic: %v", err)
	}
	if got := assetAliases(usdc); len(got) != 1 || !got[0].Equal(usdc) {
		t.Errorf("assetAliases(USDC) = %v, want just itself", got)
	}
}

// TestComputeGlobalPrice_VWAPTierReachesSACOnlyLast is the
// manipulation-surface guard for the SAC alias: adding the XLM SAC to the
// alias family widened what tryVWAPTier can find, and the whole safety
// of that widening is the ORDER. Two assertions, both load-bearing:
//
//  1. when a deep `native` VWAP exists, the SAC pool is never even
//     queried — a Soroban pool cannot displace SDEX depth; and
//  2. when both established forms miss, the SAC IS reached, so a
//     Soroban-only market still gets a price instead of a 404.
func TestComputeGlobalPrice_VWAPTierReachesSACOnlyLast(t *testing.T) {
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}

	// (1) native has depth — the loop must short-circuit before the SAC.
	deep := &aliasAwareReader{byBase: map[string]int64{
		"native":                   50,
		canonical.XLMSacContractID: 9_999,
	}}
	res, err := ComputeGlobalPrice(context.Background(), canonical.NativeAsset(), quote, deep, DefaultGlobalPriceOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice (deep native): %v", err)
	}
	if res.TradeCount != 50 {
		t.Errorf("trade_count = %d, want 50 (the native VWAP, not the SAC pool)", res.TradeCount)
	}
	if len(deep.vwapCalls) != 1 || deep.vwapCalls[0] != "native" {
		t.Errorf("vwap queries = %v, want just [native] — the SAC form must not be reached when SDEX has depth", deep.vwapCalls)
	}

	// (2) both established forms miss — the SAC form is the last resort
	// and must be tried, in third position.
	sacOnly := &aliasAwareReader{byBase: map[string]int64{canonical.XLMSacContractID: 12}}
	res, err = ComputeGlobalPrice(context.Background(), canonical.NativeAsset(), quote, sacOnly, DefaultGlobalPriceOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice (SAC only): %v", err)
	}
	if res.Authority != AuthorityVWAPNative || res.TradeCount != 12 {
		t.Errorf("authority/trade_count = %q/%d, want vwap_native/12", res.Authority, res.TradeCount)
	}
	wantOrder := []string{"native", "crypto:XLM", canonical.XLMSacContractID}
	if len(sacOnly.vwapCalls) != len(wantOrder) {
		t.Fatalf("vwap query order = %v, want %v", sacOnly.vwapCalls, wantOrder)
	}
	for i := range wantOrder {
		if sacOnly.vwapCalls[i] != wantOrder[i] {
			t.Errorf("vwap query[%d] = %q, want %q", i, sacOnly.vwapCalls[i], wantOrder[i])
		}
	}
}

// A VWAP that clears the trade floor but is days old must not beat a
// minute-old aggregator reading: tier order ranks trust between current
// prices, it does not make a stale tier-1 price current.
func TestComputeGlobalPrice_StaleVWAPYieldsToFreshAggregator(t *testing.T) {
	reader := &stubGlobalReader{}
	reader.vwap.price = "0.50000000000000"
	reader.vwap.asOf = time.Now().UTC().Add(-9 * 24 * time.Hour)
	reader.vwap.tradeCount = 40
	reader.vwap.ok = true
	price, _ := new(big.Int).SetString("100000000", 10) // 1.00 @ 8dp
	reader.agg.rows = []canonical.OracleUpdate{{
		Source: "coingecko", Timestamp: time.Now().UTC().Add(-time.Minute),
		Price: canonical.NewAmount(price), Decimals: 8,
	}}

	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko"}
	res, err := ComputeGlobalPrice(context.Background(), base, quote, reader, opts)
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != AuthorityAggregatorAvg || res.Price != "1.00000000000000" {
		t.Errorf("got %s %q, want aggregator_avg 1.00000000000000 over the 9-day-old VWAP", res.Authority, res.Price)
	}
}

// With no fresher tier the stale VWAP is still the best price there is:
// it is served with its own observation time rather than dropped.
func TestComputeGlobalPrice_StaleVWAPIsLastResort(t *testing.T) {
	reader := &stubGlobalReader{}
	asOf := time.Now().UTC().Add(-9 * 24 * time.Hour)
	reader.vwap.price = "0.50000000000000"
	reader.vwap.asOf = asOf
	reader.vwap.tradeCount = 40
	reader.vwap.ok = true

	base, quote := usdcUSDPair(t)
	res, err := ComputeGlobalPrice(context.Background(), base, quote, reader, DefaultGlobalPriceOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != AuthorityVWAPNative || res.Price != "0.50000000000000" || !res.AsOf.Equal(asOf) {
		t.Errorf("got %s %q as of %v, want the stale VWAP as of %v", res.Authority, res.Price, res.AsOf, asOf)
	}
	if reader.triCalls == 0 {
		t.Error("the triangulated tier must be tried before a stale VWAP is served")
	}
}

const (
	thinPoolAquaIssuer  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	thinPoolAquaClassic = "AQUA-" + thinPoolAquaIssuer
	thinPoolAquaSAC     = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
)

func installThinPoolRegistry(t *testing.T) canonical.Asset {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{
		thinPoolAquaSAC: "AQUA:" + thinPoolAquaIssuer,
	})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })
	classic, err := canonical.NewClassicAsset("AQUA", thinPoolAquaIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	return classic
}

func assertVWAPCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("vwap query order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vwap query[%d] = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
}

// TestComputeGlobalPrice_VWAPTierConfiguredWrapperSACLast: a configured
// classic↔SAC family walks classic FIRST. When the classic book clears
// the trade-count floor the SAC pool is never queried — the tier has no
// freshness preference, so a quiet-but-deep classic bucket beats a fresh
// thin pool by ORDER alone — and when the classic misses the SAC form is
// reached, second, so a Soroban-only market still prices.
func TestComputeGlobalPrice_VWAPTierConfiguredWrapperSACLast(t *testing.T) {
	classic := installThinPoolRegistry(t)
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}

	// (1) the classic book has depth — the pool must not be consulted,
	// however many trades it claims.
	deep := &aliasAwareReader{byBase: map[string]int64{
		thinPoolAquaClassic: 50,
		thinPoolAquaSAC:     9_999,
	}}
	res, err := ComputeGlobalPrice(context.Background(), classic, quote, deep, DefaultGlobalPriceOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice (deep classic): %v", err)
	}
	if res.Authority != AuthorityVWAPNative || res.TradeCount != 50 {
		t.Errorf("authority/trade_count = %q/%d, want vwap_native/50 (the classic book, not the SAC pool)", res.Authority, res.TradeCount)
	}
	assertVWAPCalls(t, deep.vwapCalls, []string{thinPoolAquaClassic})

	// (2) the classic form misses — the SAC form is the last resort and
	// is reached, in second position.
	sacOnly := &aliasAwareReader{byBase: map[string]int64{thinPoolAquaSAC: 12}}
	res, err = ComputeGlobalPrice(context.Background(), classic, quote, sacOnly, DefaultGlobalPriceOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice (SAC only): %v", err)
	}
	if res.Authority != AuthorityVWAPNative || res.TradeCount != 12 {
		t.Errorf("authority/trade_count = %q/%d, want vwap_native/12", res.Authority, res.TradeCount)
	}
	assertVWAPCalls(t, sacOnly.vwapCalls, []string{thinPoolAquaClassic, thinPoolAquaSAC})
}

// TestComputeGlobalPrice_VWAPTierBelowFloorSACDoesNotRescue: a classic
// bucket UNDER the trade-count floor falls through to the SAC form — the
// floor is per-form, and the SAC pool can then answer. That is the one
// arrangement in which a thin pool prices a wrapped classic on this tier,
// and it is bounded by the same floor: the pool must itself clear
// VWAPMinTradeCount, and the reader behind it (globalPriceReader in the
// API binary) withholds the pair when the alias-union market is below the
// substance floor. Pinned so the boundary is explicit rather than
// implied.
func TestComputeGlobalPrice_VWAPTierBelowFloorSACDoesNotRescue(t *testing.T) {
	classic := installThinPoolRegistry(t)
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}
	opts := DefaultGlobalPriceOptions() // VWAPMinTradeCount = 5

	// Classic under the floor, SAC also under the floor: NO tier-1 price.
	both := &aliasAwareReader{byBase: map[string]int64{
		thinPoolAquaClassic: 2,
		thinPoolAquaSAC:     3,
	}}
	if _, err := ComputeGlobalPrice(context.Background(), classic, quote, both, opts); err == nil {
		t.Errorf("ComputeGlobalPrice served a tier-1 price from two sub-floor buckets; want ErrNoPrice")
	}
	assertVWAPCalls(t, both.vwapCalls, []string{thinPoolAquaClassic, thinPoolAquaSAC})
}

func TestComputeGlobalPrice_VWAPTierAcceptsExactlyMinTradeCount(t *testing.T) {
	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	r := &stubGlobalReader{}
	r.vwap.price, r.vwap.ok, r.vwap.tradeCount = "1.0", true, opts.VWAPMinTradeCount
	got, err := ComputeGlobalPrice(context.Background(), base, quote, r, opts)
	if err != nil || got.Authority != AuthorityVWAPNative {
		t.Fatalf("tradeCount == floor: (%+v, %v), want the VWAP tier", got, err)
	}
}

// TestComputeGlobalPrice_AggregatorRejectsDivergentSource is the
// divergent-source proof: averaging the global Tier-2 headline's aggregator
// sources with a PLAIN MEAN lets a single 2x-off print drag the
// served price ~33%. A median+MAD filter drops the divergent
// print and serves the consensus of the two agreeing sources.
func TestComputeGlobalPrice_AggregatorRejectsDivergentSource(t *testing.T) {
	reader := &stubGlobalReader{}
	reader.vwap.ok = false // force the aggregator tier
	reader.agg.rows = []canonical.OracleUpdate{
		mkAggRow("coingecko", 10000, 2),     // 100.00
		mkAggRow("coinmarketcap", 10020, 2), // 100.20 (agrees)
		mkAggRow("cryptocompare", 20000, 2), // 200.00 (2x-off outlier)
	}
	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko", "coinmarketcap", "cryptocompare"}

	res, err := ComputeGlobalPrice(context.Background(), base, quote, reader, opts)
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	// Plain mean = (100.00 + 100.20 + 200.00)/3 = 133.40 — one
	// bad print moving the headline ~33%. Robust =
	// (100.00 + 100.20)/2 = 100.10.
	if res.Price != "100.10000000000000" {
		t.Fatalf("served price = %q, want 100.10000000000000 (consensus of the two agreeing sources, not the 133.40 inflated mean)", res.Price)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("contributing sources = %v, want 2 (the 200.00 outlier dropped)", res.Sources)
	}
}

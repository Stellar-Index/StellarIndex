// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"slices"
	"sort"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A chart series resolved first-hit once per response hides every bucket a
// lower-priority source holds as soon as a higher-priority source holds one
// bucket anywhere in the window, leaving a `points` array with no marker for
// the hole. These fixtures are PARTIALLY populated series (an empty one only
// certifies the path that already worked); the merge must be per bucket.

// holedDay is a UTC midnight `n` days ago, inside `timeframe=1y`.
func holedDay(n int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -n)
}

const (
	holedCEXPrice  = "0.1600000000" // CEX-fed series, at both ends of the window
	holedPoolPrice = "0.2500000000" // Soroban pool, in the days between
	// r1-measured thin-pool mark: one $0.60 print at 13.09 beside a 660-print
	// book. It must never displace a bucket the book answered.
	holedThinPoolPrice = "13.0995677490335234"
)

// chartOHLCStore is one fixture database read by both /v1/chart
// (HistoryPointsInRange) and /v1/ohlc (OHLCSeries), so any disagreement
// between the two surfaces is a disagreement between their reads.
type chartOHLCStore struct {
	byPair     map[string]map[time.Time]string
	pointCalls []string
	ohlcCalls  []string
}

func newChartOHLCStore(byPair map[string]map[time.Time]string) *chartOHLCStore {
	return &chartOHLCStore{byPair: byPair}
}

func (s *chartOHLCStore) rows(pair canonical.Pair, from, to time.Time) []struct {
	bucket time.Time
	price  string
} {
	got := s.byPair[pair.Base.String()+"/"+pair.Quote.String()]
	out := make([]struct {
		bucket time.Time
		price  string
	}, 0, len(got))
	for bucket, price := range got {
		if !from.IsZero() && bucket.Before(from) {
			continue
		}
		if !to.IsZero() && !bucket.Before(to) {
			continue
		}
		out = append(out, struct {
			bucket time.Time
			price  string
		}{bucket, price})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].bucket.Before(out[j].bucket) })
	return out
}

func (s *chartOHLCStore) HistoryPointsInRange(
	_ context.Context, pair canonical.Pair, _ string, from, to time.Time, _ int,
) ([]v1.HistoryPoint, error) {
	s.pointCalls = append(s.pointCalls, pair.Base.String()+"/"+pair.Quote.String())
	rows := s.rows(pair, from, to)
	out := make([]v1.HistoryPoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, v1.HistoryPoint{Bucket: r.bucket, VWAP: r.price})
	}
	return out, nil
}

// HistoryPoints is the since-inception read: the same rows, no window bound.
func (s *chartOHLCStore) HistoryPoints(
	ctx context.Context, pair canonical.Pair, gran string, limit int,
) ([]v1.HistoryPoint, error) {
	return s.HistoryPointsInRange(ctx, pair, gran, time.Time{}, time.Time{}, limit)
}

// TWAPPointsInRange reads the same store: the twap CAGGs hold the same buckets.
func (s *chartOHLCStore) TWAPPointsInRange(
	ctx context.Context, pair canonical.Pair, gran string, from, to time.Time, limit int,
) ([]v1.HistoryPoint, error) {
	return s.HistoryPointsInRange(ctx, pair, gran, from, to, limit)
}

// OHLCSeries serves each stored bucket as a bar with o=h=l=c=price.
func (s *chartOHLCStore) OHLCSeries(
	_ context.Context, pair canonical.Pair, _ string, from, to time.Time, _ int,
) ([]v1.OHLCSeriesBar, error) {
	s.ohlcCalls = append(s.ohlcCalls, pair.Base.String()+"/"+pair.Quote.String())
	rows := s.rows(pair, from, to)
	out := make([]v1.OHLCSeriesBar, 0, len(rows))
	for _, r := range rows {
		out = append(out, v1.OHLCSeriesBar{
			T: v1.WireTime(r.bucket), O: r.price, H: r.price, L: r.price, C: r.price,
			VBase: "1000000000", VQuote: "160000000", N: 12,
			Sources: []string{"sdex"},
		})
	}
	return out, nil
}

func (s *chartOHLCStore) TradesInRange(
	context.Context, canonical.Pair, time.Time, time.Time, int,
) ([]canonical.Trade, error) {
	return nil, nil
}

func (s *chartOHLCStore) TradesInRangeAfter(
	context.Context, canonical.Pair, time.Time, time.Time, time.Time,
	uint32, string, string, uint32, int,
) ([]canonical.Trade, error) {
	return nil, nil
}

func (s *chartOHLCStore) LatestTradePerSource(
	context.Context, canonical.Pair, string,
) ([]canonical.Trade, error) {
	return nil, nil
}

// holedFlagshipStore is the production shape scaled to 40 days: the CEX
// `crypto:XLM/fiat:USD` series holds the first ten and last five, the Soroban
// `<XLM SAC>/<USDC SAC>` pool the 25 between.
func holedFlagshipStore() *chartOHLCStore {
	cex := map[time.Time]string{}
	for _, n := range holedCEXDays() {
		cex[holedDay(n)] = holedCEXPrice
	}
	pool := map[time.Time]string{}
	for _, n := range holedPoolDays() {
		pool[holedDay(n)] = holedPoolPrice
	}
	return newChartOHLCStore(map[string]map[time.Time]string{
		"crypto:XLM/fiat:USD":                              cex,
		canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: pool,
	})
}

// holedCEXDays / holedPoolDays are disjoint days-ago offsets covering 40..1.
func holedCEXDays() []int {
	return []int{40, 39, 38, 37, 36, 35, 34, 33, 32, 31, 5, 4, 3, 2, 1}
}

func holedPoolDays() []int {
	out := make([]int, 0, 25)
	for n := 30; n >= 6; n-- {
		out = append(out, n)
	}
	return out
}

// holedDays builds a one-price series over days-ago offsets from..to inclusive
// (from >= to), skipping any in skip.
func holedDays(price string, from, to int, skip ...int) map[time.Time]string {
	out := map[time.Time]string{}
	for n := from; n >= to; n-- {
		if !slices.Contains(skip, n) {
			out[holedDay(n)] = price
		}
	}
	return out
}

// holedServer wires a history reader behind a server carrying the declared USD
// peg and its SAC wrapper.
func holedServer(t *testing.T, store v1.HistoryReader) *testServer {
	t.Helper()
	usdc := installUSDCSACRegistry(t)
	return httpTestServer(t, v1.New(v1.Options{
		History:           store,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))
}

func chartBucketDays(t *testing.T, env chartEnvelope) []time.Time {
	t.Helper()
	out := make([]time.Time, 0, len(env.Data.Points))
	for _, p := range env.Data.Points {
		out = append(out, p.T.Time().UTC())
	}
	return out
}

func chartPriceByDay(env chartEnvelope) map[time.Time]string {
	out := map[time.Time]string{}
	for _, p := range env.Data.Points {
		out[p.T.Time().UTC()] = p.P
	}
	return out
}

const holedChartQ = "/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d"

// A holed alias series must still reach the proxy walk: the walk was gated on
// the series being empty. Without the fix: 15 points and a 25-day gap.
func TestChart_HoledAliasSeriesStillReachesTheProxyWalk(t *testing.T) {
	store := holedFlagshipStore()
	ts := holedServer(t, store)

	env := getChart(t, ts.URL+holedChartQ)
	if got := len(env.Data.Points); got != 40 {
		t.Fatalf("points = %d, want 40 — 15 from the CEX spelling and the 25 the pool holds "+
			"between them; a holed series is not an empty one", got)
	}
	days := chartBucketDays(t, env)
	for i := 1; i < len(days); i++ {
		if gap := days[i].Sub(days[i-1]); gap != 24*time.Hour {
			t.Fatalf("gap of %s between %s and %s — the merged series must be contiguous",
				gap, days[i-1].Format(time.RFC3339), days[i].Format(time.RFC3339))
		}
	}
	// Each bucket carries its own source's mark; the pool displaces none of
	// the days the CEX spelling answered.
	byDay := chartPriceByDay(env)
	for _, n := range holedCEXDays() {
		if got := byDay[holedDay(n)]; got != holedCEXPrice {
			t.Errorf("day -%d = %q, want the CEX spelling's own mark %q", n, got, holedCEXPrice)
		}
	}
	for _, n := range holedPoolDays() {
		if got := byDay[holedDay(n)]; got != holedPoolPrice {
			t.Errorf("day -%d = %q, want the pool's own mark %q", n, got, holedPoolPrice)
		}
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; 25 of the 40 buckets were served through the peg's SAC form")
	}
	// A filled hole reports a continuous series.
	if env.Data.Discontinuous || env.Data.GapStartsAt != nil || env.Data.GapEndsAt != nil {
		t.Errorf("discontinuous = %v (gap %v → %v) over 40 contiguous daily points",
			env.Data.Discontinuous, env.Data.GapStartsAt, env.Data.GapEndsAt)
	}
	// A holed window must keep walking the proxies.
	if len(store.pointCalls) < 4 {
		t.Errorf("reader saw only %d calls: %v — the proxy walk must run on a holed window",
			len(store.pointCalls), store.pointCalls)
	}
}

// The chart and /v1/ohlc read one store and must serve the same buckets, in
// both directions: converging the chart onto /v1/ohlc's read would silently
// empty assets only the chart reaches (the declared peg's own dollar series).
func TestChart_AgreesWithOHLCOnThePopulationItServes(t *testing.T) {
	ts := holedServer(t, holedFlagshipStore())

	chartDays := chartBucketDays(t, getChart(t, ts.URL+holedChartQ))

	from := holedDay(41).Format(time.RFC3339)
	to := holedDay(0).Format(time.RFC3339)
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1d&from="+from+"&to="+to)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ohlc status = %d, want 200", resp.StatusCode)
	}
	var ohlc struct {
		Data struct {
			Intervals []v1.OHLCSeriesBar `json:"intervals"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ohlc); err != nil {
		t.Fatal(err)
	}
	ohlcDays := make([]time.Time, 0, len(ohlc.Data.Intervals))
	for _, b := range ohlc.Data.Intervals {
		ohlcDays = append(ohlcDays, b.T.Time().UTC())
	}

	for _, d := range chartDays {
		if !slices.ContainsFunc(ohlcDays, d.Equal) {
			t.Errorf("/v1/chart serves %s and /v1/ohlc does not", d.Format(time.RFC3339))
		}
	}
	for _, d := range ohlcDays {
		if !slices.ContainsFunc(chartDays, d.Equal) {
			t.Errorf("/v1/ohlc serves %s and /v1/chart does not — the chart is hiding a bucket "+
				"its sibling reads from the same rows", d.Format(time.RFC3339))
		}
	}
	if len(chartDays) != 40 || len(ohlcDays) != 40 {
		t.Fatalf("chart=%d bars, ohlc=%d bars, want 40 each", len(chartDays), len(ohlcDays))
	}
}

// A held-back SAC spelling fills an unanswered bucket and may not re-price an
// answered one. RED if the merge takes the last writer or blends: the served
// mark becomes the pool's 13.09 instead of the CEX spelling's 0.16.
func TestChart_ThinPoolNeverDisplacesAnAnsweredBucket(t *testing.T) {
	day := holedDay(3)
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{
		"crypto:XLM/fiat:USD":                              {day: holedCEXPrice},
		canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: {day: holedThinPoolPrice},
	}))

	env := getChart(t, ts.URL+holedChartQ)
	if got := len(env.Data.Points); got != 1 {
		t.Fatalf("points = %d, want 1 — both sources hold the same single bucket", got)
	}
	if got := env.Data.Points[0].P; got != holedCEXPrice {
		t.Errorf("served %q, want the established spelling's own mark %q", got, holedCEXPrice)
	}
	if env.Flags.Triangulated {
		t.Error("flags.triangulated = true; no served bucket came from a proxy quote")
	}
}

// A bucket depends only on itself, never on what else the window contains.
// The sources sit on opposite sides of the 1mo boundary: over 1y the CEX
// spelling answers somewhere, over 1mo nowhere, so a per-response first-hit
// would serve the pool's days in the narrow window and suppress them in the
// wide one.
func TestChart_ABucketRendersTheSameInEveryWindow(t *testing.T) {
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{
		"crypto:XLM/fiat:USD":                              holedDays(holedCEXPrice, 40, 31),
		canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: holedDays(holedPoolPrice, 5, 1),
	}))

	wide := chartPriceByDay(getChart(t, ts.URL+holedChartQ))
	narrow := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1mo&granularity=1d")

	if len(narrow.Data.Points) == 0 {
		t.Fatal("the 1mo window served nothing at all")
	}
	for _, p := range narrow.Data.Points {
		day := p.T.Time().UTC().Format(time.RFC3339)
		want, ok := wide[p.T.Time().UTC()]
		if !ok {
			t.Errorf("%s served in the 1mo window and absent from the 1y window", day)
		} else if p.P != want {
			t.Errorf("%s = %q in the 1mo window and %q in the 1y window", day, p.P, want)
		}
	}
}

// A series with a hole says so on the wire. `timeframe=all` never raises
// `truncated`, so without this signal a five-year hole has no marker.
func TestChart_DiscontinuousSignal(t *testing.T) {
	cex := map[time.Time]string{}
	for _, n := range holedCEXDays() {
		cex[holedDay(n)] = holedCEXPrice
	}
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": cex}))

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1d")
	if !env.Data.Discontinuous {
		t.Fatalf("discontinuous = false over %d points spanning a 25-day break", len(env.Data.Points))
	}
	if env.Data.GapStartsAt == nil || env.Data.GapEndsAt == nil {
		t.Fatal("gap_starts_at / gap_ends_at not populated on a discontinuous series")
	}
	if got, want := env.Data.GapStartsAt.Time().UTC(), holedDay(31); !got.Equal(want) {
		t.Errorf("gap_starts_at = %s, want %s (last bucket before the break)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got, want := env.Data.GapEndsAt.Time().UTC(), holedDay(5); !got.Equal(want) {
		t.Errorf("gap_ends_at = %s, want %s (first bucket after it)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if env.Data.Truncated {
		t.Error("truncated = true; `timeframe=all` never raises it, which is why the gap signal exists")
	}
}

// price_type=twap reads the twap CAGGs through the same chain.
func TestChartTWAP_HoledSeriesReachesTheProxyWalk(t *testing.T) {
	ts := holedServer(t, holedFlagshipStore())

	env := getChart(t, ts.URL+holedChartQ+"&price_type=twap")
	if env.Data.PriceType != "twap" {
		t.Fatalf("price_type = %q, want twap", env.Data.PriceType)
	}
	if got := len(env.Data.Points); got != 40 {
		t.Fatalf("points = %d, want 40 — the twap path merges per bucket too", got)
	}
	if env.Data.Discontinuous {
		t.Error("discontinuous = true on a merged, contiguous twap series")
	}
}

// market_cap multiplies the same price series by supply, so a holed price leg
// is a holed cap series. Supply is a constant 1000 XLM every day.
func TestChartMarketCap_HoledPriceLegReachesTheProxyWalk(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	sup := &stubSupplyLooker{}
	for n := 41; n >= 0; n-- {
		sup.daily = append(sup.daily, timescale.SupplyDayPoint{
			Bucket: holedDay(n), Circulating: big.NewInt(1_000_0000000), // 1000 XLM
		})
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:            holedFlagshipStore(),
		Supply:             sup,
		USDPeggedClassics:  []canonical.Asset{usdc},
		VerifiedCurrencies: newTestCatalogue(t),
	}))

	env := getChart(t, ts.URL+holedChartQ+"&price_type=market_cap")
	if got := len(env.Data.Points); got != 40 {
		t.Fatalf("points = %d, want 40 — the market-cap price leg runs the same chain", got)
	}
	if got := env.Data.Points[0].P; got != "160.00" {
		t.Errorf("first cap = %q, want 160.00 (0.16 × 1000 XLM)", got)
	}
}

// /v1/history/since-inception shares the chart's read chain, so it serves the
// merged series.
func TestHistorySinceInception_HoledSeriesReachesTheProxyWalk(t *testing.T) {
	ts := holedServer(t, holedFlagshipStore())

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote=fiat:USD&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Points        []struct{} `json:"points"`
			Discontinuous bool       `json:"discontinuous"`
		} `json:"data"`
		Flags v1.Flags `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if got := len(env.Data.Points); got != 40 {
		t.Fatalf("points = %d, want 40 — since-inception must serve the pool's buckets too", got)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; 25 buckets came through the peg's SAC form")
	}
	if env.Data.Discontinuous {
		t.Error("discontinuous = true on a merged, contiguous since-inception series")
	}
}

// since-inception carries the same gap signal rather than asserting a
// continuity it does not have.
func TestHistorySinceInception_DeclaresItsOwnHole(t *testing.T) {
	cex := map[time.Time]string{}
	for _, n := range holedCEXDays() {
		cex[holedDay(n)] = holedCEXPrice
	}
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": cex}))

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote=fiat:USD&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Points        []struct{} `json:"points"`
			Discontinuous bool       `json:"discontinuous"`
			GapStartsAt   *time.Time `json:"gap_starts_at"`
			GapEndsAt     *time.Time `json:"gap_ends_at"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if !env.Data.Discontinuous {
		t.Fatalf("discontinuous = false over %d points spanning a 25-day break", len(env.Data.Points))
	}
	if got, want := env.Data.GapStartsAt.UTC(), holedDay(31); !got.Equal(want) {
		t.Errorf("gap_starts_at = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got, want := env.Data.GapEndsAt.UTC(), holedDay(5); !got.Equal(want) {
		t.Errorf("gap_ends_at = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The declared peg's own dollar series has no observed market under any
// spelling, so the chart derives it through XLM (/v1/ohlc serves nothing for
// it). The merge must leave that route last, whole-series, and reached only
// when nothing observed answered.
func TestChart_DerivedXLMCrossSurvivesTheMerge(t *testing.T) {
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{
		pegAliasUSDCClassic + "/native": {holedDay(3): "6.2500000000", holedDay(2): "6.2500000000"},
		"crypto:XLM/fiat:USD":           {holedDay(3): holedCEXPrice, holedDay(2): holedCEXPrice},
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset="+pegAliasUSDCClassic+"&quote=fiat:USD&timeframe=1y&granularity=1d")
	if got := len(env.Data.Points); got != 2 {
		t.Fatalf("points = %d, want 2 — the XLM cross is the only route to this series and must survive", got)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false on a derived series")
	}
	// 6.25 XLM per USDC × $0.16 per XLM = $1.00.
	for _, p := range env.Data.Points {
		if p.P != "1.0000000000" {
			t.Errorf("derived point = %q, want 1.0000000000", p.P)
		}
	}
}

// An asset whose whole series comes from a proxy quote (AQUA, yXLM on
// production) keeps every bucket.
func TestChart_ProxyOnlyAssetKeepsItsSeries(t *testing.T) {
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{
		pegAliasAquaClassic + "/" + pegAliasUSDCSAC: holedDays("0.0041000000", 5, 1),
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset="+pegAliasAquaClassic+"&quote=fiat:USD&timeframe=1y&granularity=1d")
	if got := len(env.Data.Points); got != 5 {
		t.Fatalf("points = %d, want 5 — every bucket of a proxy-only asset stays served", got)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false on a series served entirely through the peg")
	}
}

// latencyStore wraps the fixture store with a per-pair read latency and an
// optional per-pair error.
type latencyStore struct {
	*chartOHLCStore
	delay map[string]time.Duration
	fail  map[string]error
}

func (l *latencyStore) HistoryPointsInRange(
	ctx context.Context, pair canonical.Pair, gran string, from, to time.Time, limit int,
) ([]v1.HistoryPoint, error) {
	key := pair.Base.String() + "/" + pair.Quote.String()
	if err, bad := l.fail[key]; bad {
		l.chartOHLCStore.pointCalls = append(l.chartOHLCStore.pointCalls, key)
		return nil, err
	}
	if d, slow := l.delay[key]; slow {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			l.chartOHLCStore.pointCalls = append(l.chartOHLCStore.pointCalls, key)
			return nil, ctx.Err()
		}
	}
	return l.chartOHLCStore.HistoryPointsInRange(ctx, pair, gran, from, to, limit)
}

func (l *latencyStore) TWAPPointsInRange(
	ctx context.Context, pair canonical.Pair, gran string, from, to time.Time, limit int,
) ([]v1.HistoryPoint, error) {
	return l.HistoryPointsInRange(ctx, pair, gran, from, to, limit)
}

// Measured regression: at `timeframe=1y&granularity=1m` one constituent takes
// 8.112s and three others sum to 5.867s. A walk reading all 24 inside the 8s
// ceiling turned the 200 into a 503 with every merged bucket discarded.
func TestChart_SlowProxyCannotCostTheSeries(t *testing.T) {
	slow := &latencyStore{
		chartOHLCStore: holedFlagshipStore(),
		delay: map[string]time.Duration{
			"crypto:XLM/fiat:USD":                              300 * time.Millisecond,
			"native/" + pegAliasUSDCClassic:                    8112 * time.Millisecond,
			"crypto:XLM/" + pegAliasUSDCClassic:                2 * time.Second,
			canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: 2 * time.Second,
			"crypto:XLM/crypto:USDT":                           2 * time.Second,
		},
	}
	ts := holedServer(t, slow)

	started := time.Now()
	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1m")
	elapsed := time.Since(started)

	if got := len(env.Data.Points); got < len(holedCEXDays()) {
		t.Fatalf("points = %d, want at least the %d the alias spelling holds — a slow proxy may not "+
			"cost the series it was consulted to complete", got, len(holedCEXDays()))
	}
	if elapsed > 6*time.Second {
		t.Errorf("handler took %s — the walk must stop on its own budget, well inside the 8s ceiling", elapsed)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false; a walk that stopped short must say the series may be incomplete")
	}
}

// A source consulted to fill a series may never destroy one: a proxy error
// degrades (stale) rather than discarding 40 merged buckets as a 503.
func TestChart_ProxyReadErrorDegradesRatherThanFails(t *testing.T) {
	ts := holedServer(t, &latencyStore{
		chartOHLCStore: holedFlagshipStore(),
		fail:           map[string]error{"native/" + pegAliasUSDCClassic: context.DeadlineExceeded},
	})

	env := getChart(t, ts.URL+holedChartQ)
	if got := len(env.Data.Points); got < len(holedCEXDays()) {
		t.Fatalf("points = %d, want at least the %d the alias spelling holds", got, len(holedCEXDays()))
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false on a walk cut short by a read error")
	}
}

// An alias spelling's read error is the response: a `?granularity=` the store
// rejects stays a 400, not an empty 200 (and nothing merged means nothing to
// degrade to).
func TestChart_AliasReadErrorIsStillTheAnswer(t *testing.T) {
	ts := holedServer(t, &latencyStore{
		chartOHLCStore: newChartOHLCStore(map[string]map[time.Time]string{}),
		fail:           map[string]error{"native/fiat:USD": v1.ErrUnknownGranularity},
	})

	resp := mustGet(t, ts.URL+holedChartQ)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an alias spelling's error is the answer", resp.StatusCode)
	}
}

// `timeframe=all` on `prices_1mo`: buckets are calendar months, so a fixed
// 30-day threshold reports a hole in a contiguous year while a skipped month
// must still be reported.
func TestChart_MonthlyGranularityGapSignal(t *testing.T) {
	month := func(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }
	var year []time.Time
	for m := time.January; m <= time.December; m++ {
		year = append(year, month(2025, m))
	}
	for _, tc := range []struct {
		name       string
		months     []time.Time
		wantPoints int
		wantGap    bool
		gapStart   time.Time
		gapEnd     time.Time
	}{
		{"contiguous calendar months", year, 12, false, time.Time{}, time.Time{}},
		{"skipped March to May", []time.Time{
			month(2025, time.January), month(2025, time.February), month(2025, time.June), month(2025, time.July),
		}, 4, true, month(2025, time.February), month(2025, time.June)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series := map[time.Time]string{}
			for _, m := range tc.months {
				series[m] = holedCEXPrice
			}
			ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": series}))

			env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1mo")
			if got := len(env.Data.Points); got != tc.wantPoints {
				t.Fatalf("points = %d, want %d", got, tc.wantPoints)
			}
			if env.Data.Discontinuous != tc.wantGap {
				t.Fatalf("discontinuous = %v, want %v (gap %v → %v) — a 31-day month is not a hole",
					env.Data.Discontinuous, tc.wantGap, env.Data.GapStartsAt, env.Data.GapEndsAt)
			}
			if !tc.wantGap {
				return
			}
			if got := env.Data.GapStartsAt.Time().UTC(); !got.Equal(tc.gapStart) {
				t.Errorf("gap_starts_at = %s, want %s", got.Format("2006-01-02"), tc.gapStart.Format("2006-01-02"))
			}
			if got := env.Data.GapEndsAt.Time().UTC(); !got.Equal(tc.gapEnd) {
				t.Errorf("gap_ends_at = %s, want %s", got.Format("2006-01-02"), tc.gapEnd.Format("2006-01-02"))
			}
		})
	}
}

// Common path, not an edge: 59 of the 60 largest assets report
// flags.triangulated=true, so every proxy read runs on an empty merge. A proxy
// that fails before the series is found must be skipped, not answered with
// (RED with the error split on merge emptiness: 503, zero points).
func TestChart_FailingEarlyProxyStillFindsTheSeries(t *testing.T) {
	pool := holedDays(holedPoolPrice, 5, 1)
	for _, tc := range []struct {
		name string
		fail map[string]error
	}{
		{"first proxy pair errors", map[string]error{
			"native/" + pegAliasUSDCClassic: errors.New("prices_1d unavailable"),
		}},
		{"first proxy pair times out", map[string]error{
			"native/" + pegAliasUSDCClassic: context.DeadlineExceeded,
		}},
		{"a backer pair times out", map[string]error{
			"native/crypto:USDT": context.DeadlineExceeded,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := holedServer(t, &latencyStore{
				chartOHLCStore: newChartOHLCStore(map[string]map[time.Time]string{
					canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: pool,
				}),
				fail: tc.fail,
			})

			env := getChart(t, ts.URL+holedChartQ)
			if got := len(env.Data.Points); got != 5 {
				t.Fatalf("points = %d, want 5 — a failing proxy must be skipped", got)
			}
			if !env.Flags.Triangulated {
				t.Error("flags.triangulated = false on a series carried entirely by a proxy quote")
			}
			if !env.Flags.Stale {
				t.Error("flags.stale = false; a source was skipped, so the series may be short")
			}
		})
	}
}

// [chartWindow.covered] has two conditions: no readable grid point before the
// earliest hit, and as many distinct buckets as the grid has points. A wrong
// `true` silently truncates a money series with no stale flag, so both branches
// of the count are pinned: a covered window stops after one read without
// flags.stale; one interior bucket missing (held by a proxy) keeps walking.
// RED under a bare `return ok`: the missing day is never filled.
func TestChart_CoveredWindowStopsTheWalkOnlyWhenCovered(t *testing.T) {
	const missing = 100
	for _, tc := range []struct {
		name      string
		book      map[time.Time]string // under native/fiat:USD
		pool      map[time.Time]string // under the peg's SAC form
		wantCalls int                  // exact when >0; otherwise at least 2
	}{
		{"window covered after one read", holedDays(holedCEXPrice, 366, 0), nil, 1},
		{"every readable bucket held", holedDays(holedCEXPrice, 365, 1), nil, 1},
		{
			"one interior bucket missing", holedDays(holedCEXPrice, 365, 1, missing),
			map[time.Time]string{holedDay(missing): holedPoolPrice},
			0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series := map[string]map[time.Time]string{"native/fiat:USD": tc.book}
			if tc.pool != nil {
				series[canonical.XLMSacContractID+"/"+pegAliasUSDCSAC] = tc.pool
			}
			store := newChartOHLCStore(series)
			ts := holedServer(t, store)

			env := getChart(t, ts.URL+holedChartQ)
			if env.Data.Discontinuous {
				t.Fatalf("series is not contiguous (gap %v → %v)", env.Data.GapStartsAt, env.Data.GapEndsAt)
			}
			if tc.wantCalls > 0 {
				if got := len(store.pointCalls); got != tc.wantCalls {
					t.Errorf("reader saw %d calls, want %d — a fully covered window has nothing left to fill: %v",
						got, tc.wantCalls, store.pointCalls)
				}
				if env.Flags.Stale {
					t.Error("flags.stale = true; stopping because the window is COVERED is not a degradation")
				}
				return
			}
			byDay := chartPriceByDay(env)
			if got, ok := byDay[holedDay(missing)]; !ok || got != holedPoolPrice {
				t.Fatalf("day -%d = %q (present=%v), want the proxy's %q — the window is not covered "+
					"while a bucket inside it is missing (calls=%d)",
					missing, got, ok, holedPoolPrice, len(store.pointCalls))
			}
			if len(store.pointCalls) < 2 {
				t.Errorf("reader saw %d call(s): %v — a covered-window stop fired on an UNCOVERED window",
					len(store.pointCalls), store.pointCalls)
			}
		})
	}
}

// spanCostStore prices a read by the range it scans: once the merge holds a
// series, a source whose reads scan more than twice the window's missing
// width, or read it unbounded, times out.
type spanCostStore struct {
	*chartOHLCStore
	missing   time.Duration
	reads     int
	laterRows int
	perSource map[string]*spanCost
}

type spanCost struct {
	reads int
	width time.Duration
}

func (s *spanCostStore) HistoryPointsInRange(
	ctx context.Context, pair canonical.Pair, gran string, from, to time.Time, limit int,
) ([]v1.HistoryPoint, error) {
	s.reads++
	if s.reads == 1 {
		return s.chartOHLCStore.HistoryPointsInRange(ctx, pair, gran, from, to, limit)
	}
	c := s.perSource[pair.String()]
	if c == nil {
		c = &spanCost{}
		s.perSource[pair.String()] = c
	}
	c.reads++
	if to.IsZero() {
		c.width = time.Duration(math.MaxInt64)
		return nil, context.DeadlineExceeded
	}
	// An index-ordered read stops at its limit, so it scans no more buckets.
	if c.width += min(to.Sub(from), time.Duration(limit)*24*time.Hour); c.width > 2*s.missing {
		return nil, context.DeadlineExceeded
	}
	pts, err := s.chartOHLCStore.HistoryPointsInRange(ctx, pair, gran, from, to, limit)
	pts = pts[:min(len(pts), limit)]
	s.laterRows += len(pts)
	return pts, err
}

// A window with a bucket no source traded in can never be covered, so every
// remaining source is read. Each read must cost only the buckets still missing,
// so the walk ends by exhausting its list without flags.stale, and a later
// source that holds a missing bucket is still merged. The scattered case is
// RED while reads are capped by range count rather than missing width.
func TestChart_UnfillableHoleDoesNotRunTheWalkToBudget(t *testing.T) {
	var scattered []int
	for n := 12; n <= 360; n += 12 {
		scattered = append(scattered, n)
	}
	for _, tc := range []struct {
		name   string
		holes  []int
		filled []int // holes the last proxy (the peg's SAC pool) holds
		// Rows returned by every read after the first; with more than one
		// span, the dense proxy and the pool each also return probe rows.
		laterRows int
	}{
		{"one hole no source holds", []int{100}, nil, 0},
		{"three holes, a later source fills one", []int{40, 100, 300}, []int{100}, 1 + 2},
		// The two one-day gaps fit in the six missing days and are bridged,
		// so the dense proxy also returns days 21 and 61, which the book holds.
		{"adjacent hole runs", []int{20, 22, 60, 62, 100, 200}, []int{60, 200}, 2 + 2 + 2},
		// Eleven-day gaps: only two fit in the 30 (then 28) missing days,
		// so the dense proxy returns those 22 held days and the pool its two.
		{"thirty holes spread across the year", scattered, []int{120, 240}, 22 + 2 + 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := map[time.Time]string{}
			for _, n := range tc.filled {
				pool[holedDay(n)] = holedPoolPrice
			}
			series := map[string]map[time.Time]string{
				"native/fiat:USD":                                  holedDays(holedCEXPrice, 365, 1, tc.holes...),
				"native/" + pegAliasUSDCClassic:                    holedDays(holedThinPoolPrice, 365, 1, tc.holes...),
				canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: pool,
			}
			store := &spanCostStore{
				chartOHLCStore: newChartOHLCStore(series),
				missing:        time.Duration(len(tc.holes)) * 24 * time.Hour,
				perSource:      map[string]*spanCost{},
			}
			ts := holedServer(t, store)

			env := getChart(t, ts.URL+holedChartQ)
			if env.Flags.Stale {
				t.Errorf("flags.stale = true after %d reads; nothing cut the walk short — a hole "+
					"no source holds is not a degradation", store.reads)
			}
			byDay := chartPriceByDay(env)
			for _, n := range tc.holes {
				got, served := byDay[holedDay(n)]
				switch {
				case slices.Contains(tc.filled, n) && got != holedPoolPrice:
					t.Errorf("day -%d = %q, want the later source's %q", n, got, holedPoolPrice)
				case !slices.Contains(tc.filled, n) && served:
					t.Errorf("day -%d served, but no source holds it", n)
				}
			}
			if store.laterRows != tc.laterRows {
				t.Errorf("reads after the first returned %d rows, want %d — a later source must be "+
					"read over the missing buckets only", store.laterRows, tc.laterRows)
			}
			for src, c := range store.perSource {
				if c.reads > 1+len(tc.holes) || c.width > 2*store.missing {
					t.Errorf("%s: %d reads over %v, want at most %d reads over %v — a later "+
						"source's cost must be bounded by the missing buckets",
						src, c.reads, c.width, 1+len(tc.holes), 2*store.missing)
				}
				if _, holds := series[src]; !holds && c.reads > 1 {
					t.Errorf("%s holds nothing but was read %d times, want 1", src, c.reads)
				}
			}
		})
	}
}

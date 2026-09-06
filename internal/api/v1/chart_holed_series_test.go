// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"sort"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A chart series resolved FIRST-HIT ONCE PER RESPONSE hides every bucket
// a lower-priority source holds, for as long as any higher-priority
// source holds one bucket anywhere in the window — and it hides them
// inside a `points` array with no hole in it, so the consumer draws a
// straight line across the missing years.
//
// Measured on production 2026-09-06, one host, one minute:
//
//	/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1d
//	  → 1,070 points, ONE interior gap of 1,919 days
//	    (2021-01-31 → 2026-05-05), truncated=false
//	/v1/ohlc?asset=native&quote=fiat:USD&interval=1d&limit=1000
//	  → 887 bars, 2024-03-12 → 2026-09-05, of which 763 fall strictly
//	    inside the chart's gap
//
// `crypto:XLM/fiat:USD` answered the chart first and won the whole
// response; the 763 days live in `<XLM SAC>/<USDC SAC>`, a pair the
// proxy walk enumerates and — because the walk was gated on the series
// being EMPTY, which a series with a hole in it is not — never read.
//
// These fixtures are built at the shape that reaches that branch: a
// PARTIALLY populated series. An empty one is easier to construct and
// certifies only the path that already worked.

// holedDay is a UTC midnight `n` days ago — recent enough that
// `timeframe=1y` covers it, so the chart's window and an explicit OHLC
// window can be made to span exactly the same buckets.
func holedDay(n int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -n)
}

const (
	// The CEX-fed series' own marks, at the two ends of the window.
	holedCEXPrice = "0.1600000000"
	// The Soroban pool's marks, in the days between them.
	holedPoolPrice = "0.2500000000"
	// The r1-measured thin-pool mark: one $0.60 print at 13.09 beside a
	// 660-print book. It must never displace a bucket the book answered.
	holedThinPoolPrice = "13.0995677490335234"
)

// chartOHLCStore is ONE fixture database, keyed by pair and bucket, read
// by both /v1/chart (HistoryPointsInRange) and /v1/ohlc (OHLCSeries).
//
// One store rather than one fixture per endpoint is what makes a
// cross-surface parity test mean anything: with two fixtures the test
// asserts that two hand-written answers agree, which they will whatever
// the code does. Here the population is a property of the store, so any
// disagreement between the two surfaces is a disagreement between the
// two READS.
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

// HistoryPoints is the since-inception read: the same rows with no
// window bound.
func (s *chartOHLCStore) HistoryPoints(
	ctx context.Context, pair canonical.Pair, gran string, limit int,
) ([]v1.HistoryPoint, error) {
	return s.HistoryPointsInRange(ctx, pair, gran, time.Time{}, time.Time{}, limit)
}

// TWAPPointsInRange reads the same store: the twap CAGGs hold the same
// buckets for the same markets, and the chart's twap path runs the same
// chain over them.
func (s *chartOHLCStore) TWAPPointsInRange(
	ctx context.Context, pair canonical.Pair, gran string, from, to time.Time, limit int,
) ([]v1.HistoryPoint, error) {
	return s.HistoryPointsInRange(ctx, pair, gran, from, to, limit)
}

// OHLCSeries expresses the same stored buckets as bars. A bucket that
// traded at one price is a bar whose open, high, low and close are that
// price — so the two surfaces differ in SHAPE and can only differ in
// POPULATION if their reads do.
func (s *chartOHLCStore) OHLCSeries(
	_ context.Context, pair canonical.Pair, _ string, from, to time.Time, _ int,
) ([]v1.OHLCSeriesBar, error) {
	s.ohlcCalls = append(s.ohlcCalls, pair.Base.String()+"/"+pair.Quote.String())
	rows := s.rows(pair, from, to)
	out := make([]v1.OHLCSeriesBar, 0, len(rows))
	for _, r := range rows {
		out = append(out, v1.OHLCSeriesBar{
			T: r.bucket, O: r.price, H: r.price, L: r.price, C: r.price,
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

// holedFlagshipStore is the production shape, scaled to 40 days: the
// CEX-fed `crypto:XLM/fiat:USD` series holds the first ten days and the
// last five, and the Soroban `<XLM SAC>/<USDC SAC>` pool holds the
// twenty-five between them.
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

// holedCEXDays / holedPoolDays are days-ago offsets; the two sets are
// disjoint and together cover 40..1 with no hole.
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

// holedServer wires the store behind a server carrying the deployment's
// declared USD peg and its SAC wrapper.
func holedServer(t *testing.T, store *chartOHLCStore) *testServer {
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
		out = append(out, p.T.UTC())
	}
	return out
}

// ── the defect itself ─────────────────────────────────────────────────

// TestChart_HoledAliasSeriesStillReachesTheProxyWalk is the branch the
// regression turns on: the alias-spelling series is NOT empty, it is
// holed, and the fallback that can fill the hole was gated on emptiness.
//
// RED before the fix: 15 points, all at the CEX mark, with a 25-day
// interior gap — the pool's spelling is enumerated by the proxy walk and
// never read, because `len(points) == 0` is false.
func TestChart_HoledAliasSeriesStillReachesTheProxyWalk(t *testing.T) {
	store := holedFlagshipStore()
	ts := holedServer(t, store)

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
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
	// Each bucket carries its OWN source's mark: the pool fills the days
	// the CEX spelling left empty and displaces none of the days it
	// answered.
	byDay := map[time.Time]string{}
	for _, p := range env.Data.Points {
		byDay[p.T.UTC()] = p.P
	}
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
}

// TestChart_AgreesWithOHLCOnThePopulationItServes is the test that would
// have caught this: the two surfaces read one store and must answer the
// same question about the same pair with the same BUCKETS.
//
// It asserts agreement in BOTH directions. Convergence in one direction
// only is how a fix that puts the chart onto /v1/ohlc's read would pass
// while silently emptying the assets the chart reaches and /v1/ohlc does
// not — measured live on 2026-09-06, `/v1/ohlc` returns nothing at all
// for the declared peg's own dollar series while `/v1/chart` serves 124
// days of it through the XLM cross.
func TestChart_AgreesWithOHLCOnThePopulationItServes(t *testing.T) {
	store := holedFlagshipStore()
	ts := holedServer(t, store)

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	chartDays := chartBucketDays(t, env)

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
		ohlcDays = append(ohlcDays, b.T.UTC())
	}

	inOHLC := map[time.Time]bool{}
	for _, d := range ohlcDays {
		inOHLC[d] = true
	}
	inChart := map[time.Time]bool{}
	for _, d := range chartDays {
		inChart[d] = true
	}
	for _, d := range chartDays {
		if !inOHLC[d] {
			t.Errorf("/v1/chart serves %s and /v1/ohlc does not", d.Format(time.RFC3339))
		}
	}
	for _, d := range ohlcDays {
		if !inChart[d] {
			t.Errorf("/v1/ohlc serves %s and /v1/chart does not — the chart is hiding a bucket "+
				"its sibling reads from the same rows", d.Format(time.RFC3339))
		}
	}
	if len(chartDays) != 40 || len(ohlcDays) != 40 {
		t.Fatalf("chart=%d bars, ohlc=%d bars, want 40 each", len(chartDays), len(ohlcDays))
	}
}

// TestChart_ThinPoolNeverDisplacesAnAnsweredBucket is the other half of
// the per-bucket rule, and the reason the walk can run unconditionally.
// A held-back SAC spelling fills a bucket nothing established answered
// and NO other bucket — so the r1-measured $0.60 pool print that moves a
// real bar's high by +37.32% cannot reach a day the book traded.
//
// RED if the merge takes the last writer, or blends: the served mark
// becomes the pool's 13.09 instead of the CEX spelling's 0.16.
func TestChart_ThinPoolNeverDisplacesAnAnsweredBucket(t *testing.T) {
	day := holedDay(3)
	store := newChartOHLCStore(map[string]map[time.Time]string{
		"crypto:XLM/fiat:USD":                              {day: holedCEXPrice},
		canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: {day: holedThinPoolPrice},
	})
	ts := holedServer(t, store)

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	if got := len(env.Data.Points); got != 1 {
		t.Fatalf("points = %d, want 1 — both sources hold the same single bucket", got)
	}
	if got := env.Data.Points[0].P; got != holedCEXPrice {
		t.Errorf("served %q, want the established spelling's own mark %q — a held-back spelling "+
			"may fill an unanswered bucket and may not re-price an answered one", got, holedCEXPrice)
	}
	if env.Flags.Triangulated {
		t.Error("flags.triangulated = true; no served bucket came from a proxy quote")
	}
}

// TestChart_ABucketRendersTheSameInEveryWindow pins the property that
// made per-bucket the right rule rather than a per-response first-hit
// (aggregate-alias-folding.md §7.5): a bucket must depend only on
// itself, never on what else the requested window happens to contain.
//
// RED under a per-response first-hit: asked over 1y the pool's days are
// suppressed (the CEX spelling answered somewhere in the window); asked
// over the 1-month window that holds only pool days they are served —
// one unchanged database, two answers.
func TestChart_ABucketRendersTheSameInEveryWindow(t *testing.T) {
	// The two sources are placed on opposite sides of the 1mo boundary
	// on purpose: over 1y the CEX spelling answers SOMEWHERE, and over
	// 1mo it answers nowhere. A per-response first-hit therefore serves
	// the pool's recent days in the narrow window and suppresses the
	// same days in the wide one. Sharing the flagship fixture would not
	// reach that: both windows would hold a CEX day and the first-hit
	// would pick the same source twice, which is a fixture shaped by
	// the fix rather than by the input space.
	cex := map[time.Time]string{}
	for n := 40; n >= 31; n-- {
		cex[holedDay(n)] = holedCEXPrice
	}
	pool := map[time.Time]string{}
	for n := 5; n >= 1; n-- {
		pool[holedDay(n)] = holedPoolPrice
	}
	store := newChartOHLCStore(map[string]map[time.Time]string{
		"crypto:XLM/fiat:USD":                              cex,
		canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: pool,
	})
	ts := holedServer(t, store)

	wide := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	narrow := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1mo&granularity=1d")

	byDayWide := map[time.Time]string{}
	for _, p := range wide.Data.Points {
		byDayWide[p.T.UTC()] = p.P
	}
	if len(narrow.Data.Points) == 0 {
		t.Fatal("the 1mo window served nothing at all")
	}
	for _, p := range narrow.Data.Points {
		want, ok := byDayWide[p.T.UTC()]
		if !ok {
			t.Errorf("%s served in the 1mo window and absent from the 1y window",
				p.T.UTC().Format(time.RFC3339))
			continue
		}
		if p.P != want {
			t.Errorf("%s = %q in the 1mo window and %q in the 1y window — a bucket must not depend "+
				"on the window that contains it", p.T.UTC().Format(time.RFC3339), p.P, want)
		}
	}
}

// ── the wire signal ───────────────────────────────────────────────────

// TestChart_DiscontinuousSignal pins that a series with a hole SAYS so.
// The 1,070-point production series carried its 1,919-day break with
// `truncated: false` and no other marker, so a consumer drew a straight
// line across five years and could not have known.
func TestChart_DiscontinuousSignal(t *testing.T) {
	// Only the CEX spelling has rows, so 25 days stay missing.
	cex := map[time.Time]string{}
	for _, n := range holedCEXDays() {
		cex[holedDay(n)] = holedCEXPrice
	}
	store := newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": cex})
	ts := holedServer(t, store)

	// `timeframe=all` is the production shape and the one with no other
	// account of itself: `truncated` is deliberately never raised for it
	// ("everything you have" cannot be short), so before this signal a
	// five-year hole had nothing on the wire at all.
	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1d")
	if !env.Data.Discontinuous {
		t.Fatalf("discontinuous = false over %d points spanning a 25-day break", len(env.Data.Points))
	}
	if env.Data.GapStartsAt == nil || env.Data.GapEndsAt == nil {
		t.Fatal("gap_starts_at / gap_ends_at not populated on a discontinuous series")
	}
	if got, want := env.Data.GapStartsAt.UTC(), holedDay(31); !got.Equal(want) {
		t.Errorf("gap_starts_at = %s, want %s (last bucket before the break)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got, want := env.Data.GapEndsAt.UTC(), holedDay(5); !got.Equal(want) {
		t.Errorf("gap_ends_at = %s, want %s (first bucket after it)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if env.Data.Truncated {
		t.Error("truncated = true; `timeframe=all` never raises it, which is why the gap signal exists")
	}
}

// TestChart_ContiguousSeriesIsNotDiscontinuous keeps the signal from
// being a constant: once the pool fills the hole the same request must
// report a continuous series, so a consumer can act on the flag.
func TestChart_ContiguousSeriesIsNotDiscontinuous(t *testing.T) {
	ts := holedServer(t, holedFlagshipStore())

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	if env.Data.Discontinuous {
		t.Errorf("discontinuous = true over %d contiguous daily points (gap %v → %v)",
			len(env.Data.Points), env.Data.GapStartsAt, env.Data.GapEndsAt)
	}
	if env.Data.GapStartsAt != nil || env.Data.GapEndsAt != nil {
		t.Error("gap bounds populated on a continuous series")
	}
}

// ── the sibling surfaces on the same chain ────────────────────────────

// TestChartTWAP_HoledSeriesReachesTheProxyWalk: price_type=twap reads
// the twap CAGGs through the same chain and inherited the same defect.
// Probed live 2026-09-06: it served the identical 1,070 points with the
// identical 1,919-day break.
func TestChartTWAP_HoledSeriesReachesTheProxyWalk(t *testing.T) {
	ts := holedServer(t, holedFlagshipStore())

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d&price_type=twap")
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

// TestChartMarketCap_HoledPriceLegReachesTheProxyWalk: market_cap
// multiplies the same daily price series by circulating supply, so a
// holed price leg is a holed market-cap series. Supply is a single
// snapshot, forward-filled, so every priced day gets a cap.
func TestChartMarketCap_HoledPriceLegReachesTheProxyWalk(t *testing.T) {
	store := holedFlagshipStore()
	usdc := installUSDCSACRegistry(t)
	sup := &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
		{Bucket: holedDay(41), Circulating: big.NewInt(1_000_0000000)}, // 1000 XLM
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:            store,
		Supply:             sup,
		USDPeggedClassics:  []canonical.Asset{usdc},
		VerifiedCurrencies: newTestCatalogue(t),
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
	if got := len(env.Data.Points); got != 40 {
		t.Fatalf("points = %d, want 40 — the market-cap price leg runs the same chain", got)
	}
	if got := env.Data.Points[0].P; got != "160.00" {
		t.Errorf("first cap = %q, want 160.00 (0.16 × 1000 XLM)", got)
	}
}

// TestHistorySinceInception_HoledSeriesReachesTheProxyWalk closes the
// third copy: /v1/history/since-inception ran a hand-maintained twin of
// the same chain, with the same per-response first-hit gate, and served
// the identical holed series — 1,070 points and the identical 1,919-day
// break, probed live 2026-09-06. It now shares the chain rather than
// mirroring it.
func TestHistorySinceInception_HoledSeriesReachesTheProxyWalk(t *testing.T) {
	ts := holedServer(t, holedFlagshipStore())

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote=fiat:USD&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Points []struct {
				T time.Time `json:"t"`
				P string    `json:"p"`
			} `json:"points"`
			Discontinuous bool `json:"discontinuous"`
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

// TestHistorySinceInception_DeclaresItsOwnHole: this surface shares the
// chart's read chain and can therefore serve the same holed series — on
// the flagship pair it gains 763 buckets and still stops ~1,156 days
// short. It carries the same gap signal rather than asserting a
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

// ── no reach is lost ──────────────────────────────────────────────────

// TestChart_DerivedXLMCrossSurvivesTheMerge pins the direction a
// convergence onto /v1/ohlc's read would have broken. The declared peg's
// own dollar series has no observed market under ANY spelling — its
// depth is the USDC/XLM book — so the chart derives it through XLM, and
// /v1/ohlc, which has no derivation route, serves nothing for it at all
// (measured live 2026-09-06: 124 chart points against `intervals: []`).
//
// The per-bucket merge must leave that route exactly where it was: last,
// whole-series, and reached only when nothing observed answered.
func TestChart_DerivedXLMCrossSurvivesTheMerge(t *testing.T) {
	store := newChartOHLCStore(map[string]map[time.Time]string{
		// The peg's actual depth: its XLM book.
		pegAliasUSDCClassic + "/native": {
			holedDay(3): "6.2500000000",
			holedDay(2): "6.2500000000",
		},
		// XLM's own dollar series, the pivot leg.
		"crypto:XLM/fiat:USD": {
			holedDay(3): holedCEXPrice,
			holedDay(2): holedCEXPrice,
		},
	})
	ts := holedServer(t, store)

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

// TestChart_ProxyOnlyAssetKeepsItsSeries pins the other no-loss
// direction with a live-measured shape: an asset whose whole series
// comes from a proxy quote (AQUA and yXLM on production serve 178 chart
// points each this way). The merge must return the same buckets it
// always did, not fewer.
func TestChart_ProxyOnlyAssetKeepsItsSeries(t *testing.T) {
	pool := map[time.Time]string{}
	for n := 5; n >= 1; n-- {
		pool[holedDay(n)] = "0.0041000000"
	}
	store := newChartOHLCStore(map[string]map[time.Time]string{
		pegAliasAquaClassic + "/" + pegAliasUSDCSAC: pool,
	})
	ts := holedServer(t, store)

	env := getChart(t, ts.URL+"/v1/chart?asset="+pegAliasAquaClassic+"&quote=fiat:USD&timeframe=1y&granularity=1d")
	if got := len(env.Data.Points); got != 5 {
		t.Fatalf("points = %d, want 5 — every bucket of a proxy-only asset stays served", got)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false on a series served entirely through the peg")
	}
}

// ── the walk must not cost the series it exists to complete ───────────

// latencyStore wraps the fixture store with a per-pair read latency and
// an optional per-pair error, so the handler can be driven at the
// timings production actually shows.
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

// TestChart_SlowProxyCannotCostTheSeries reproduces the measured
// production regression: at `timeframe=1y&granularity=1m` one
// constituent (`native/USDC-GA5Z…`) takes 8.112s on its own and three
// others sum to 5.867s, while the flagship itself serves in 1.09-1.39s.
// A walk that reads all 24 inside one 8s ceiling turns that 200 into a
// `503 chart-timeout`.
//
// RED before the budget: the request spends the whole ceiling and
// answers 503, with every merged bucket discarded.
func TestChart_SlowProxyCannotCostTheSeries(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	store := holedFlagshipStore()
	slow := &latencyStore{
		chartOHLCStore: store,
		delay: map[string]time.Duration{
			// The alias spelling that answers today, at its measured cost.
			"crypto:XLM/fiat:USD": 300 * time.Millisecond,
			// The constituent measured at 8.112s in production.
			"native/" + pegAliasUSDCClassic: 8112 * time.Millisecond,
			// Three more at ~2s each.
			"crypto:XLM/" + pegAliasUSDCClassic:                2 * time.Second,
			canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: 2 * time.Second,
			"crypto:XLM/crypto:USDT":                           2 * time.Second,
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           slow,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	started := time.Now()
	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1m")
	elapsed := time.Since(started)

	// The series the surface serves today must still be served — every
	// bucket of it, not a 503 and not a truncation.
	if got := len(env.Data.Points); got < len(holedCEXDays()) {
		t.Fatalf("points = %d, want at least the %d the alias spelling holds — a slow proxy may not "+
			"cost the series it was consulted to complete", got, len(holedCEXDays()))
	}
	// The 8s handler ceiling must not be reached: the walk abandons the
	// slow read at chartWalkBudget and serves what merged.
	if elapsed > 6*time.Second {
		t.Errorf("handler took %s — the walk must stop on its own budget, well inside the 8s ceiling", elapsed)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false; a walk that stopped short must say the series may be incomplete")
	}
	t.Logf("served %d points in %s (flags.stale=%v)", len(env.Data.Points), elapsed.Round(time.Millisecond), env.Flags.Stale)
}

// TestChart_ProxyReadErrorDegradesRatherThanFails is the same rule at
// the error edge rather than the latency one: a source consulted to
// FILL a series may never destroy one.
//
// RED before the fix: the proxy's error propagated, every merged bucket
// was discarded, and a complete 40-point series answered 503.
func TestChart_ProxyReadErrorDegradesRatherThanFails(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	failing := &latencyStore{
		chartOHLCStore: holedFlagshipStore(),
		fail: map[string]error{
			"native/" + pegAliasUSDCClassic: context.DeadlineExceeded,
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           failing,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	if got := len(env.Data.Points); got < len(holedCEXDays()) {
		t.Fatalf("points = %d, want at least the %d the alias spelling holds", got, len(holedCEXDays()))
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false on a walk cut short by a read error")
	}
}

// TestChart_FirstReadErrorStillFails pins the other side of that
// asymmetry: when NOTHING has merged there is no answer to degrade, so
// the store's error must still reach the handler's problem writer
// exactly as it did before the walk existed.
func TestChart_FirstReadErrorStillFails(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	dead := &latencyStore{
		chartOHLCStore: newChartOHLCStore(map[string]map[time.Time]string{}),
		fail:           map[string]error{"native/fiat:USD": v1.ErrUnknownGranularity},
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           dead,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an error with nothing merged must still be the answer",
			resp.StatusCode)
	}
}

// TestChart_CoveredWindowStopsTheWalk pins the short-circuit: when the
// requested window is fully claimed, no remaining source can add a
// bucket, so the walk must stop rather than pay for 23 more CAGG scans.
// Reads are not cached — CachedHistoryReader wraps LatestTradePerSource
// only — so each one is a fresh scan.
//
// RED before the short-circuit: all 24 source pairs are read.
func TestChart_CoveredWindowStopsTheWalk(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	// Every day of the window, under the first alias spelling that is
	// tried — so the window is covered after ONE read.
	dense := map[time.Time]string{}
	for n := 366; n >= 0; n-- {
		dense[holedDay(n)] = holedCEXPrice
	}
	store := newChartOHLCStore(map[string]map[time.Time]string{"native/fiat:USD": dense})
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           store,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	if env.Data.Discontinuous {
		t.Fatalf("fixture is not contiguous (gap %v → %v)", env.Data.GapStartsAt, env.Data.GapEndsAt)
	}
	if got := len(store.pointCalls); got != 1 {
		t.Errorf("reader saw %d calls, want 1 — a fully covered window has nothing left to fill: %v",
			got, store.pointCalls)
	}
	if env.Flags.Stale {
		t.Error("flags.stale = true; stopping because the window is COVERED is not a degradation")
	}
}

// TestChart_UncoveredWindowStillWalks keeps the short-circuit from
// being the old per-response first-hit wearing a new name: the moment a
// bucket in the window is missing, the walk must continue.
func TestChart_UncoveredWindowStillWalks(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	store := holedFlagshipStore()
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           store,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	if got := len(env.Data.Points); got != 40 {
		t.Fatalf("points = %d, want 40 — a holed window must keep walking", got)
	}
	if len(store.pointCalls) < 4 {
		t.Errorf("reader saw only %d calls: %v — the proxy walk must run on a holed window",
			len(store.pointCalls), store.pointCalls)
	}
}

// ── the gap signal on a calendar-month grid ───────────────────────────

// TestChart_MonthlyGranularityIsNotDiscontinuous: `prices_1mo` is built
// with time_bucket('1 month', ts, 'UTC'), so its buckets are CALENDAR
// months. Measured against a fixed 30-day threshold, 7 of the 12
// adjacencies in a contiguous year exceed it and the series reports a
// hole it does not have — the field's own documented meaning inverted,
// on the wire, in the OpenAPI description.
//
// RED before the fix: discontinuous=true, gap_starts_at=2025-01-01,
// gap_ends_at=2025-02-01.
func TestChart_MonthlyGranularityIsNotDiscontinuous(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	months := map[time.Time]string{}
	b := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		months[b] = holedCEXPrice
		b = b.AddDate(0, 1, 0)
	}
	store := newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": months})
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           store,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1mo")
	if got := len(env.Data.Points); got != 12 {
		t.Fatalf("points = %d, want 12 contiguous calendar months", got)
	}
	if env.Data.Discontinuous {
		t.Errorf("discontinuous = true over 12 contiguous calendar months, naming %v → %v as a gap — "+
			"a 31-day month is not a hole",
			env.Data.GapStartsAt.UTC().Format("2006-01-02"), env.Data.GapEndsAt.UTC().Format("2006-01-02"))
	}
}

// TestChart_MonthlyGranularityReportsARealGap keeps the 1mo arm from
// going silent: skipping a month IS a hole and must still be reported.
func TestChart_MonthlyGranularityReportsARealGap(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	months := map[time.Time]string{}
	for _, m := range []time.Month{time.January, time.February, time.June, time.July} {
		months[time.Date(2025, m, 1, 0, 0, 0, 0, time.UTC)] = holedCEXPrice
	}
	store := newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": months})
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           store,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1mo")
	if !env.Data.Discontinuous {
		t.Fatal("discontinuous = false over a series that skips March, April and May")
	}
	if got := env.Data.GapStartsAt.UTC(); !got.Equal(time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("gap_starts_at = %s, want 2025-02-01", got.Format("2006-01-02"))
	}
	if got := env.Data.GapEndsAt.UTC(); !got.Equal(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("gap_ends_at = %s, want 2025-06-01", got.Format("2006-01-02"))
	}
}

// ── a proxy failure is never the answer ───────────────────────────────

// TestChart_FailingEarlyProxyStillFindsTheSeries is the case the
// merge-state split inverted, and it is the COMMON path rather than an
// edge: on this deployment 59 of the 60 largest assets report
// flags.triangulated=true, meaning their alias spellings hold nothing
// and a PROXY read carries the whole series. Every proxy read for those
// assets therefore runs on an EMPTY merge.
//
// Base's proxy walk is `if err != nil || len(pp) == 0 { continue }` —
// skip the failing pair, try the next. Splitting error handling on
// merge emptiness instead propagated it, so a proxy that failed before
// the series was found turned base's 200 into a 503 with nothing in it.
//
// RED with the error split on merge state: 503, zero points, for a
// series that is sitting in the very next pair.
func TestChart_FailingEarlyProxyStillFindsTheSeries(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	// Nothing under any alias spelling of native/fiat:USD; the series
	// lives in the peg's SAC form, which the walk reaches LAST.
	pool := map[time.Time]string{}
	for n := 5; n >= 1; n-- {
		pool[holedDay(n)] = holedPoolPrice
	}
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
			store := &latencyStore{
				chartOHLCStore: newChartOHLCStore(map[string]map[time.Time]string{
					canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: pool,
				}),
				fail: tc.fail,
			}
			ts := httpTestServer(t, v1.New(v1.Options{
				History:           store,
				USDPeggedClassics: []canonical.Asset{usdc},
			}))

			env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
			if got := len(env.Data.Points); got != 5 {
				t.Fatalf("points = %d, want 5 — a proxy that fails before the series is found must be "+
					"skipped, not answered with", got)
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

// TestChart_AliasReadErrorIsStillTheAnswer is the other half of the
// same split, and the reason it is drawn on source CLASS rather than on
// nothing at all: an ALIAS spelling is the read this surface has always
// made, so its failure remains the response. A `?granularity=` the
// store rejects must still be a 400 and not an empty 200.
func TestChart_AliasReadErrorIsStillTheAnswer(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	dead := &latencyStore{
		chartOHLCStore: newChartOHLCStore(map[string]map[time.Time]string{}),
		fail:           map[string]error{"native/fiat:USD": v1.ErrUnknownGranularity},
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           dead,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an alias spelling's error is the answer", resp.StatusCode)
	}
}

// ── the short-circuit's own regression barrier ────────────────────────

// TestChart_InteriorHoleInsideTheWindowStillWalks is the counting arm of
// [chartWindow.covered], and it exists because nothing else in the suite
// reaches it. The predicate has two conditions — no readable grid point
// before the earliest hit, AND as many distinct buckets held as the grid
// has points — and every other fixture here starts its data deep inside
// the window, so condition one alone decides and the count is never
// consulted. Replacing the count with a bare `return ok` passes the
// whole package.
//
// That would be the worst failure this patch could have: `covered` is
// what decides when to STOP reading, a wrong `true` silently truncates a
// money series, and a clean coverage stop deliberately does NOT set
// flags.stale — so there would be nothing on the wire to show for it.
//
// The window here starts inside the alias series' own span (so condition
// one passes) with exactly one interior bucket missing, held by a proxy.
//
// RED under `return ok`: the walk stops after the first read and the
// missing day is never filled.
func TestChart_InteriorHoleInsideTheWindowStillWalks(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	const missing = 100
	book := map[time.Time]string{}
	for n := 365; n >= 1; n-- {
		if n == missing {
			continue
		}
		book[holedDay(n)] = holedCEXPrice
	}
	store := newChartOHLCStore(map[string]map[time.Time]string{
		"native/fiat:USD": book,
		canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: {holedDay(missing): holedPoolPrice},
	})
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           store,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	byDay := map[time.Time]string{}
	for _, p := range env.Data.Points {
		byDay[p.T.UTC()] = p.P
	}
	if got, ok := byDay[holedDay(missing)]; !ok || got != holedPoolPrice {
		t.Fatalf("day -%d = %q (present=%v), want the proxy's %q — the window is NOT covered while a "+
			"bucket inside it is missing, so the walk must keep reading (calls=%d)",
			missing, got, ok, holedPoolPrice, len(store.pointCalls))
	}
	if env.Data.Discontinuous {
		t.Errorf("discontinuous = true; the hole was filled (gap %v → %v)",
			env.Data.GapStartsAt, env.Data.GapEndsAt)
	}
	if len(store.pointCalls) < 2 {
		t.Errorf("reader saw %d call(s): %v — a covered-window stop fired on an UNCOVERED window",
			len(store.pointCalls), store.pointCalls)
	}
}

// TestChart_CoveredWindowStopsAtTheCountingCondition is the mirror: the
// same fixture with the hole FILLED by the alias spelling is genuinely
// covered, so the walk must stop after one read. Together with the test
// above this pins both branches of the predicate — one where the count
// says "keep going", one where it says "stop" — rather than only the
// condition that short-circuits first.
func TestChart_CoveredWindowStopsAtTheCountingCondition(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	book := map[time.Time]string{}
	for n := 365; n >= 1; n-- {
		book[holedDay(n)] = holedCEXPrice
	}
	store := newChartOHLCStore(map[string]map[time.Time]string{"native/fiat:USD": book})
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           store,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d")
	if env.Data.Discontinuous {
		t.Fatalf("fixture is not contiguous (gap %v → %v)", env.Data.GapStartsAt, env.Data.GapEndsAt)
	}
	if got := len(store.pointCalls); got != 1 {
		t.Errorf("reader saw %d calls, want 1 — every readable bucket is held: %v", got, store.pointCalls)
	}
	if env.Flags.Stale {
		t.Error("flags.stale = true; stopping because the window is COVERED is not a degradation")
	}
}

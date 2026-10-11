// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
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

// holedToday is read once so a fixture built before UTC midnight and an
// assertion made after it name the same day.
var holedToday = time.Now().UTC().Truncate(24 * time.Hour)

// holedDay is a UTC midnight `n` days ago, inside `timeframe=1y`.
func holedDay(n int) time.Time {
	return holedToday.AddDate(0, 0, -n)
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
	return newChartOHLCStore(map[string]map[time.Time]string{
		"crypto:XLM/fiat:USD":                              holedAt(holedCEXPrice, holedCEXDays()),
		canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: holedAt(holedPoolPrice, holedPoolDays()),
	})
}

// holedCEXOnlyStore holds just the CEX spelling, leaving the 25-day break.
func holedCEXOnlyStore() *chartOHLCStore {
	return newChartOHLCStore(map[string]map[time.Time]string{
		"crypto:XLM/fiat:USD": holedAt(holedCEXPrice, holedCEXDays()),
	})
}

// holedAt prices each days-ago offset in days.
func holedAt(price string, days []int) map[time.Time]string {
	out := map[time.Time]string{}
	for _, n := range days {
		out[holedDay(n)] = price
	}
	return out
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

type sinceInceptionBody struct {
	Data struct {
		Points        []struct{}   `json:"points"`
		Discontinuous bool         `json:"discontinuous"`
		GapStartsAt   *v1.WireTime `json:"gap_starts_at"`
		GapEndsAt     *v1.WireTime `json:"gap_ends_at"`
	} `json:"data"`
	Flags v1.Flags `json:"flags"`
}

func getSinceInception(t *testing.T, ts *testServer) sinceInceptionBody {
	t.Helper()
	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote=fiat:USD&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body sinceInceptionBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

const holedChartQ = "/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d"

// requireHoleDeclared asserts a series over the CEX-only store names its
// 25-day break.
func requireHoleDeclared(t *testing.T, points int, discontinuous bool, start, end *v1.WireTime) {
	t.Helper()
	if !discontinuous {
		t.Fatalf("discontinuous = false over %d points spanning a 25-day break", points)
	}
	if start == nil || end == nil {
		t.Fatal("gap_starts_at / gap_ends_at not populated on a discontinuous series")
	}
	if got, want := start.Time().UTC(), holedDay(31); !got.Equal(want) {
		t.Errorf("gap_starts_at = %s, want %s (last bucket before the break)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got, want := end.Time().UTC(), holedDay(5); !got.Equal(want) {
		t.Errorf("gap_ends_at = %s, want %s (first bucket after it)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
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

// TestChartMarketCap_ValuationGuards pins /v1/chart?price_type=market_cap to
// the valuation guards populateMarketCap applies on /v1/assets/{id}: a series
// the detail page would refuse as a headline cap (ticker collision, single
// venue with sub-floor volume, cap beyond the turnover ceiling) is withheld,
// and a liquidity refusal says so rather than reading as "no data".
func TestChartMarketCap_ValuationGuards(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name         string
		asset        string
		sources      []string
		volume       string
		floor, ratio float64
		wantPoints   int
		wantLowLiq   bool
	}{
		{"single venue, liquid: served", dustGuardAssetID, []string{"sdex"}, "100000", 1000, 0, 2, false},
		{"single venue, sub-floor volume: withheld", dustGuardAssetID, []string{"sdex"}, "10", 1000, 0, 0, true},
		{"multi-venue, cap beyond turnover ceiling: withheld", dustGuardAssetID, []string{"sdex", "kraken"}, "10", 0, 50000, 0, true},
		{"verified-ticker collision: withheld", chartGuardLookalike, []string{"sdex", "kraken"}, "100000", 1000, 50000, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			priceKey := tc.asset + "/fiat:USD"
			srv := v1.New(v1.Options{
				History: &stubHistoryReader{points: []v1.HistoryPoint{
					{Bucket: d(1), VWAP: "0.50"},
					{Bucket: d(2), VWAP: "0.50"},
				}},
				Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
					{Bucket: d(1), Circulating: mustBigInt("100000000000000000")}, // 10^10 tokens: $5B at $0.50
				}},
				Prices: &stubPriceReader{
					snapshots: map[string]v1.PriceSnapshot{priceKey: {Price: "0.50", PriceType: "vwap"}},
					sources:   map[string][]string{priceKey: tc.sources},
				},
				Volume:                  &stubVolumeReader{volume: tc.volume},
				VerifiedCurrencies:      newTestCatalogue(t),
				MinMarketCapVolumeUSD:   tc.floor,
				MaxMarketCapVolumeRatio: tc.ratio,
			})
			ts := httpTestServer(t, srv)
			resp := mustGet(t, ts.URL+"/v1/chart?asset="+tc.asset+"&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var env struct {
				Data struct {
					Points []struct {
						P string `json:"p"`
					} `json:"points"`
					MarketCapLowLiquidity bool `json:"market_cap_low_liquidity"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := len(env.Data.Points); got != tc.wantPoints {
				t.Errorf("points = %d, want %d: %+v", got, tc.wantPoints, env.Data.Points)
			}
			if tc.wantPoints > 0 && env.Data.Points[0].P != "5000000000.00" {
				t.Errorf("first cap = %q, want 5000000000.00", env.Data.Points[0].P)
			}
			if env.Data.MarketCapLowLiquidity != tc.wantLowLiq {
				t.Errorf("market_cap_low_liquidity = %v, want %v", env.Data.MarketCapLowLiquidity, tc.wantLowLiq)
			}
		})
	}
}

// /v1/chart?price_type=market_cap forward-fills daily supply onto price days,
// but not indefinitely: a supply series that stops is not carried to today
// with stale:false.
func TestChartMarketCap_DeadSupplyIsNotForwardFilledForever(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	var price []v1.HistoryPoint
	for day := 1; day <= 10; day++ {
		price = append(price, v1.HistoryPoint{Bucket: d(day), VWAP: "0.50"})
	}
	priceKey := dustGuardAssetID + "/fiat:USD"
	srv := v1.New(v1.Options{
		History: &stubHistoryReader{points: price},
		Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
			{Bucket: d(1), Circulating: mustBigInt("100000000000000000")}, // last observed June 1
		}},
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{priceKey: {Price: "0.50", PriceType: "vwap"}},
		},
	})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?asset="+dustGuardAssetID+"&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Points []struct {
				T time.Time `json:"t"`
			} `json:"points"`
		} `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// June 1 supply covers June 1-3 (<= 48 h carry); June 4-10 are cut.
	if n := len(env.Data.Points); n != 3 || !env.Data.Points[n-1].T.Equal(d(3)) {
		t.Errorf("points = %+v, want June 1-3 only", env.Data.Points)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false on a series whose supply stopped a week before its last price day")
	}
}

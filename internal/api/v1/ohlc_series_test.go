package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// mkSeriesBar builds a fake OHLCSeriesBar for stub fixtures. Volume
// + price strings are formatted exactly as the storage NUMERIC
// passthrough would produce them.
func mkSeriesBar(t time.Time, o, h, l, c, vb, vq string, n int64) v1.OHLCSeriesBar {
	return v1.OHLCSeriesBar{T: v1.WireTime(t), O: o, H: h, L: l, C: c, VBase: vb, VQuote: vq, N: n}
}

// TestOHLCSeries_ReturnsIntervalsArray — the multi-bar mode wires
// the OHLCSeries reader call and renders the canonical
// {intervals: [...]} wire shape. Pre-fix, /v1/ohlc?interval=...
// returned a single OHLCBar and ignored the param entirely (F-0071).
func TestOHLCSeries_ReturnsIntervalsArray(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	bars := []v1.OHLCSeriesBar{
		mkSeriesBar(t0, "0.16", "0.17", "0.15", "0.165", "1000", "165", 4),
		mkSeriesBar(t0.Add(time.Hour), "0.165", "0.18", "0.16", "0.175", "1200", "200", 5),
	}
	reader := &stubHistoryReader{ohlcBars: bars}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	// Non-fiat quote → first-hit alias path (exact NUMERIC passthrough,
	// no combine reformat). The fiat:USD combine path is covered by
	// TestOHLCSeries_FiatCombinesUSDPeggedConstituents.
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=crypto:BTC&interval=1h&limit=24")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data v1.OHLCSeriesResponse `json:"data"`
	}
	mustDecode(t, resp, &body)
	if got := len(body.Data.Intervals); got != 2 {
		t.Fatalf("len(intervals) = %d, want 2", got)
	}
	if body.Data.Interval != "1h" {
		t.Errorf("interval = %q, want 1h", body.Data.Interval)
	}
	if body.Data.Base != "native" || body.Data.Quote != "crypto:BTC" {
		t.Errorf("base/quote = %q/%q, want native/crypto:BTC", body.Data.Base, body.Data.Quote)
	}
	if body.Data.Intervals[0].O != "0.16" || body.Data.Intervals[1].C != "0.175" {
		t.Errorf("OHLC values wrong: %+v", body.Data.Intervals)
	}
	if reader.LastInterval() != "1h" {
		t.Errorf("storage call interval = %q, want 1h", reader.LastInterval())
	}
	// limit+1, exactly: the handler reads ONE row past the requested cap
	// so it can tell a window that was cut from one that merely filled
	// (RLT-453, see capOHLCSeriesNewest). Anything else forwarded here is
	// a bug — `limit` loses the truncated signal, more over-reads.
	if reader.LastLimit() != 25 {
		t.Errorf("storage call limit = %d, want 25 (limit=24 + the one-row truncation probe)", reader.LastLimit())
	}
}

// TestOHLCSeries_StatesVolumeScaleOnWire pins F015's narrowed remainder
// for the non-combined path: a series bar must state the smallest-unit
// scale its v_base/v_quote are expressed in (v_base_decimals /
// v_quote_decimals), resolved from the CAGG's own `sources` column —
// mirroring [OHLCBar.QuoteVolumeDecimals] (F096) for the single-bar
// path. A bar whose sources are unknown states null, never a guessed
// scale and never the internal -1 sentinel.
func TestOHLCSeries_StatesVolumeScaleOnWire(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cex := mkSeriesBar(t0, "0.16", "0.17", "0.15", "0.165", "1000", "165", 4)
	cex.Sources = []string{"binance"} // CEX venue — 8dp, not the 7dp on-chain default
	unknown := mkSeriesBar(t0.Add(time.Hour), "0.165", "0.18", "0.16", "0.175", "1200", "200", 5)
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{cex, unknown}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=crypto:BTC&interval=1h&limit=24")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data struct {
			Intervals []map[string]json.RawMessage `json:"intervals"`
		} `json:"data"`
	}
	mustDecode(t, resp, &body)
	if got := len(body.Data.Intervals); got != 2 {
		t.Fatalf("len(intervals) = %d, want 2", got)
	}
	// 8 is binance's amountScaleDecimalsFor scale.
	for i, want := range []string{"8", "null"} {
		for _, key := range []string{"v_base_decimals", "v_quote_decimals"} {
			got, ok := body.Data.Intervals[i][key]
			if !ok || string(got) != want {
				t.Errorf("intervals[%d].%s = %q (present=%v), want %s — a consumer "+
					"dividing by 10^%s must land on the served integer's actual "+
					"scale, or be told it is unknown", i, key, got, ok, want, key)
			}
		}
	}
}

// TestOHLCSeries_InvalidInterval400 — unsupported interval values
// 400 with the canonical errors/invalid-interval problem+json.
func TestOHLCSeries_InvalidInterval400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)
	// "2h" was here as the invalid example until 2h/12h/3d/2w were
	// added to the ladder; "7h" replaces it as a plausible-looking
	// interval with no cagg or re-bucket route.
	for _, raw := range []string{"foo", "7h", "10s", " 1h", "2M"} {
		resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval="+raw)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("interval=%q: status = %d, want 400", raw, resp.StatusCode)
		}
	}
}

// TestOHLCSeries_LimitTooLarge400 — limit > 1000 / < 1 / non-int
// 400 with errors/limit-too-large.
func TestOHLCSeries_LimitTooLarge400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)
	for _, raw := range []string{"5000", "0", "-1", "abc", "10001"} {
		resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h&limit="+raw)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("limit=%q: status = %d, want 400", raw, resp.StatusCode)
		}
	}
}

// TestOHLCSeries_EmptyReturns200WithEmptyIntervals — when the CAGG
// has no closed buckets in the requested window, the series shape
// is still emitted (200, {intervals: []}). Distinct from the
// single-bar 404-on-empty contract.
func TestOHLCSeries_EmptyReturns200WithEmptyIntervals(t *testing.T) {
	reader := &stubHistoryReader{ohlcBars: nil} // zero bars
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty series)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"intervals":[]`) {
		t.Errorf("body missing empty intervals array: %s", body)
	}
}

// TestOHLCSeries_BucketTimestampsAlignedUTC — the handler's
// implicit-`to` snap rounds DOWN to the requested interval's UTC
// boundary so two requests landing in the same closed window
// across regions resolve to identical [from, to). Storage call's
// `to` arg is the snapped value.
func TestOHLCSeries_BucketTimestampsAlignedUTC(t *testing.T) {
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	gotTo := reader.LastTo()
	// `to` must be top-of-hour UTC. The handler snapped now → top-of-hour.
	if gotTo.Minute() != 0 || gotTo.Second() != 0 || gotTo.Nanosecond() != 0 {
		t.Errorf("to not aligned to 1h boundary: %v", gotTo)
	}
	if gotTo.Location() != time.UTC {
		t.Errorf("to not UTC: %v", gotTo.Location())
	}
	// Default limit = 100, so from = to - 100h.
	gotFrom := reader.LastFrom()
	if want := gotTo.Add(-100 * time.Hour); !gotFrom.Equal(want) {
		t.Errorf("from = %v, want %v (to - 100h default limit)", gotFrom, want)
	}
}

// TestOHLCSeries_DailyBoundaryAlignment — same alignment property
// at 1d granularity: `to` snaps to 00:00 UTC.
func TestOHLCSeries_DailyBoundaryAlignment(t *testing.T) {
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1d&limit=7")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	gotTo := reader.LastTo()
	if gotTo.Hour() != 0 || gotTo.Minute() != 0 || gotTo.Second() != 0 {
		t.Errorf("to not aligned to 1d boundary: %v", gotTo)
	}
}

// pgTimeBucketOrigin mirrors timescale's time_bucket() default
// origin (2000-01-03 00:00 UTC, a Monday) — see
// internal/storage/timescale/aggregates.go's OHLCSeriesReBucketed
// doc comment. Server-side folded intervals (3d/2w/2h, RLT-258) grid
// off this origin; the tests below pin that the API's default `to`
// does too.
var pgTimeBucketOrigin = time.Date(2000, 1, 3, 0, 0, 0, 0, time.UTC)

// TestOHLCSeries_ThreeDayBoundaryMatchesTimeBucketOrigin — the
// default `to` for interval=3d must land on the same grid
// [timescale.Store.OHLCSeriesReBucketed]'s `time_bucket(INTERVAL '3
// days', …)` folds prices_1d onto, i.e. a multiple of 72h measured
// from pgTimeBucketOrigin. [time.Time.Truncate] floors from Go's
// zero time instead, whose offset from pgTimeBucketOrigin is not a
// multiple of 72h, so the un-fixed handler's default `to` is
// provably off that grid for every possible "now" (RLT-258).
func TestOHLCSeries_ThreeDayBoundaryMatchesTimeBucketOrigin(t *testing.T) {
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=3d&limit=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	gotTo := reader.LastTo()
	width := 3 * 24 * time.Hour
	if rem := gotTo.Sub(pgTimeBucketOrigin) % width; rem != 0 {
		t.Errorf("to = %v is %v off the 3d time_bucket grid (origin %v) — client truncation disagrees with the server's default time_bucket origin",
			gotTo, rem, pgTimeBucketOrigin)
	}
}

// TestOHLCSeries_TwoWeekBoundaryMatchesTimeBucketOrigin — same
// property at the 2w fold (14 days folding prices_1w). See
// TestOHLCSeries_ThreeDayBoundaryMatchesTimeBucketOrigin.
func TestOHLCSeries_TwoWeekBoundaryMatchesTimeBucketOrigin(t *testing.T) {
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=2w&limit=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	gotTo := reader.LastTo()
	width := 14 * 24 * time.Hour
	if rem := gotTo.Sub(pgTimeBucketOrigin) % width; rem != 0 {
		t.Errorf("to = %v is %v off the 2w time_bucket grid (origin %v) — client truncation disagrees with the server's default time_bucket origin",
			gotTo, rem, pgTimeBucketOrigin)
	}
}

// TestOHLCSeries_ExplicitFromTo — explicit RFC3339 from/to flow
// through verbatim (no clamping); interval validation still
// applies.
func TestOHLCSeries_ExplicitFromTo(t *testing.T) {
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	from := "2026-01-01T00:00:00Z"
	to := "2026-01-02T00:00:00Z"
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h&from="+from+"&to="+to)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	gotFrom, _ := time.Parse(time.RFC3339, from)
	gotTo, _ := time.Parse(time.RFC3339, to)
	if !reader.LastFrom().Equal(gotFrom) {
		t.Errorf("from = %v, want %v", reader.LastFrom(), gotFrom)
	}
	if !reader.LastTo().Equal(gotTo) {
		t.Errorf("to = %v, want %v", reader.LastTo(), gotTo)
	}
}

// TestOHLCSeries_StorageError500 — propagated upstream errors
// (other than ErrUnknownGranularity) render the canonical 500.
func TestOHLCSeries_StorageError500(t *testing.T) {
	reader := &stubHistoryReader{
		ohlcSeriesFn: func(_ context.Context, _ canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.OHLCSeriesBar, error) {
			return nil, errors.New("storage exploded")
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// TestOHLCSeries_PreservesSingleBarBackcompat — the single-bar mode
// is reached only when `interval` is absent. With interval set the
// stub's TradesInRange is NEVER called (the series reader fires
// instead). Mirrors the F-0071 back-compat contract: clients that
// haven't migrated still get the single-bar shape, clients passing
// interval get the new series.
func TestOHLCSeries_PreservesSingleBarBackcompat(t *testing.T) {
	// Two readers, two responses:
	//   1. no interval → single-bar mode → TradesInRange called
	//   2. interval=1h → series mode → OHLCSeries called, TradesInRange NOT called
	t0 := time.Unix(1_772_000_000, 0).UTC()
	tradeFixture := []canonical.Trade{mkOHLCTrade(1, 100, t0)}
	reader := &stubHistoryReader{
		trades:   tradeFixture,
		ohlcBars: []v1.OHLCSeriesBar{mkSeriesBar(t0, "0.16", "0.17", "0.15", "0.165", "1000", "165", 4)},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	// 1. No interval → single-bar wire shape.
	resp1 := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("single-bar status = %d", resp1.StatusCode)
	}
	body1, _ := readAll(resp1)
	if !strings.Contains(body1, `"open"`) || strings.Contains(body1, `"intervals"`) {
		t.Errorf("single-bar mode returned wrong shape: %s", body1)
	}

	// 2. With interval → series wire shape.
	resp2 := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("series status = %d", resp2.StatusCode)
	}
	body2, _ := readAll(resp2)
	if !strings.Contains(body2, `"intervals"`) || strings.Contains(body2, `"trade_count"`) {
		t.Errorf("series mode returned wrong shape: %s", body2)
	}
}

// TestOHLCSeries_AllSupportedIntervals — every valid interval
// reaches the storage layer with the canonical string. Pins the
// enum allow-list against drift.
func TestOHLCSeries_AllSupportedIntervals(t *testing.T) {
	for _, interval := range []string{
		"1m", "5m", "15m", "30m", "1h", "2h", "4h", "12h",
		"1d", "3d", "1w", "2w", "1mo",
	} {
		reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{}}
		srv := v1.New(v1.Options{History: reader})
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval="+interval)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("interval=%s: status = %d", interval, resp.StatusCode)
		}
		if reader.LastInterval() != interval {
			t.Errorf("interval=%s: reader saw %q", interval, reader.LastInterval())
		}
	}
}

// TestOHLCSeries_FiatCombinesUSDPeggedConstituents — a fiat:USD series
// must COMBINE the USD-pegged constituent pairs per bucket (not first-hit
// a single one), so the deep history under the stablecoin pairs surfaces.
// Verifies the combine math: exact volume sum + max-high/min-low, and
// base-volume-weighted open/close.
func TestOHLCSeries_FiatCombinesUSDPeggedConstituents(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{
		ohlcSeriesFn: func(_ context.Context, pair canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.OHLCSeriesBar, error) {
			b, q := pair.Base.String(), pair.Quote.String()
			switch {
			case b == "native" && q == "crypto:USDT":
				return []v1.OHLCSeriesBar{mkSeriesBar(t0, "1.0", "1.1", "0.9", "1.05", "100", "100", 2)}, nil
			case b == "crypto:XLM" && q == "fiat:USD":
				return []v1.OHLCSeriesBar{mkSeriesBar(t0, "1.2", "1.3", "1.0", "1.25", "300", "360", 5)}, nil
			}
			return nil, nil
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	var env struct {
		Data v1.OHLCSeriesResponse `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(env.Data.Intervals) != 1 {
		t.Fatalf("want 1 combined bucket, got %d: %s", len(env.Data.Intervals), body)
	}
	bar := env.Data.Intervals[0]
	// Exact: n = 2+5, base = 100+300; high = max, low = min.
	if bar.N != 7 {
		t.Errorf("N = %d, want 7 (2+5)", bar.N)
	}
	if bar.VBase != "400" {
		t.Errorf("v_base = %q, want 400 (100+300)", bar.VBase)
	}
	// base-volume-weighted open = (1.0*100 + 1.2*300)/400 = 1.15
	if got := mustFloat(t, bar.O); !approxEq(got, 1.15) {
		t.Errorf("open = %v, want ~1.15 (vol-weighted)", got)
	}
	// close = (1.05*100 + 1.25*300)/400 = 1.20
	if got := mustFloat(t, bar.C); !approxEq(got, 1.20) {
		t.Errorf("close = %v, want ~1.20 (vol-weighted)", got)
	}
	if got := mustFloat(t, bar.H); !approxEq(got, 1.3) {
		t.Errorf("high = %v, want 1.3 (max)", got)
	}
	if got := mustFloat(t, bar.L); !approxEq(got, 0.9) {
		t.Errorf("low = %v, want 0.9 (min)", got)
	}
}

// TestOHLCSeries_TriangulatedFalseForDirectlyQuotedFiatSeries — GH-1081
// finding 3: a fiat-quoted series served ENTIRELY by the directly-quoted
// market (crypto:XLM/fiat:USD itself, not a stablecoin/SAC proxy) must
// not be flagged triangulated just because the quote asset is fiat.
func TestOHLCSeries_TriangulatedFalseForDirectlyQuotedFiatSeries(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{
		ohlcSeriesFn: func(_ context.Context, pair canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.OHLCSeriesBar, error) {
			if pair.Base.String() == "crypto:XLM" && pair.Quote.String() == "fiat:USD" {
				return []v1.OHLCSeriesBar{mkSeriesBar(t0, "1.2", "1.3", "1.0", "1.25", "300", "360", 5)}, nil
			}
			return nil, nil
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	var env struct {
		Data  v1.OHLCSeriesResponse `json:"data"`
		Flags v1.Flags              `json:"flags"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(env.Data.Intervals) != 1 {
		t.Fatalf("want 1 bucket, got %d: %s", len(env.Data.Intervals), body)
	}
	if env.Flags.Triangulated {
		t.Error("flags.triangulated = true, want false — every contributing bar came from " +
			"crypto:XLM/fiat:USD itself, no proxy/peg constituent was involved")
	}
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return f
}

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}

// TestOHLCSeries_WireShapeFields — exhaustive field check for the
// wire envelope so subtle JSON-tag changes get flagged.
func TestOHLCSeries_WireShapeFields(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// fiat:USD now COMBINES the USD-pegged constituents (the deep series
	// lives under the stablecoin pairs). Make the stub pair-aware so only
	// the direct native/fiat:USD pair carries the fixture — exercising the
	// combine's single-source passthrough (exact O/H/L/C/v/n) so the wire
	// shape assertion still holds.
	reader := &stubHistoryReader{
		ohlcSeriesFn: func(_ context.Context, pair canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.OHLCSeriesBar, error) {
			if pair.Base.String() == "native" && pair.Quote.String() == "fiat:USD" {
				return []v1.OHLCSeriesBar{mkSeriesBar(t0, "1.0", "2.0", "0.5", "1.5", "100", "150", 3)}, nil
			}
			return nil, nil
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h")
	body, _ := readAll(resp)
	// Series-mode wire field names — CG/CMC parity (`t,o,h,l,c,v_base,v_quote,n`).
	// fiat:USD goes through the combine, which normalises prices to fixed
	// decimals (ohlcPriceDigits) like the single-bar /v1/ohlc; v_base and
	// v_quote are integer smallest-unit text on every path.
	// Single-constituent bucket → values pass through exactly.
	for _, want := range []string{`"t":"`, `"o":"1.0000000000"`, `"h":"2.0000000000"`, `"l":"0.5000000000"`, `"c":"1.5000000000"`, `"v_base":"100"`, `"v_quote":"150"`, `"n":3`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}

	// Confirm the envelope JSON parses cleanly into the typed
	// response struct.
	var env struct {
		Data v1.OHLCSeriesResponse `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Data.Intervals[0].N != 3 {
		t.Errorf("N decoded = %d, want 3", env.Data.Intervals[0].N)
	}
}

// newestNSeriesFn is a [stubHistoryReader.ohlcSeriesFn] with the STORE's
// read semantics rather than a fixture's: bars outside [from, to) are
// not returned, and a positive `limit` keeps the NEWEST `limit` of what
// remains, ascending — what [timescale.Store.OHLCSeries] does with its
// `ORDER BY bucket DESC LIMIT n` + reverse (RLT-453). A stub that
// ignores `limit` cannot see any of the defects below, because every one
// of them is about which rows a capped read leaves behind.
func newestNSeriesFn(byPair map[string][]v1.OHLCSeriesBar) func(
	context.Context, canonical.Pair, string, time.Time, time.Time, int,
) ([]v1.OHLCSeriesBar, error) {
	return func(_ context.Context, pair canonical.Pair, _ string, from, to time.Time, limit int) ([]v1.OHLCSeriesBar, error) {
		var out []v1.OHLCSeriesBar
		for _, b := range byPair[pair.String()] {
			if !b.T.Time().Before(from) && b.T.Time().Before(to) {
				out = append(out, b)
			}
		}
		if limit > 0 && len(out) > limit {
			out = out[len(out)-limit:]
		}
		return out, nil
	}
}

// hourlyBars builds one ascending bar per listed hour offset from t0,
// every bar sharing one shape.
func hourlyBars(t0 time.Time, hours []int, h, l string, n int64) []v1.OHLCSeriesBar {
	out := make([]v1.OHLCSeriesBar, 0, len(hours))
	for _, hr := range hours {
		out = append(out, mkSeriesBar(t0.Add(time.Duration(hr)*time.Hour), "1.00", h, l, "1.00", "1000", "1000", n))
	}
	return out
}

func seriesWindowURL(base, quote string, t0 time.Time, hours, limit int) string {
	return "/v1/ohlc?base=" + base + "&quote=" + quote + "&interval=1h" +
		"&from=" + t0.Format(time.RFC3339) +
		"&to=" + t0.Add(time.Duration(hours)*time.Hour).Format(time.RFC3339) +
		"&limit=" + strconv.Itoa(limit)
}

// TestOHLCSeries_TruncatedSetOnlyWhenBucketsWereDropped pins the second
// half of RLT-453. OHLCSeriesBar.Truncated was declared on the wire
// (OpenAPI: "Reserved for future row-cap signalling; absent today") and
// assigned nowhere, so a capped response gave a caller no way to tell
// its window was cut.
//
// It must be set when — and ONLY when — the window held buckets the
// response does not carry. The first attempt at this set it on
// `len(bars) == limit`, which is also the shape of every default
// request on a liquid pair: no from/to sizes the window to exactly
// `limit` intervals, a dense market fills every one, and nothing was
// dropped. That version flagged every such chart as cut.
func TestOHLCSeries_TruncatedSetOnlyWhenBucketsWereDropped(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dense := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	for _, tc := range []struct {
		name      string
		hours     []int // populated buckets in the 12h window
		limit     int
		wantHours []int
		truncated bool
	}{
		{"wide dense window is cut to the newest limit", dense, 2, []int{10, 11}, true},
		{"one bucket over the cap is still a cut", dense, 11, dense[1:], true},
		{"window holding exactly limit buckets is whole", dense, 12, dense, false},
		{"wide sparse window holding exactly limit buckets is whole", []int{1, 7}, 2, []int{1, 7}, false},
		{"window below the cap is whole", []int{3}, 24, []int{3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubHistoryReader{ohlcSeriesFn: newestNSeriesFn(map[string][]v1.OHLCSeriesBar{
				"native/crypto:BTC": hourlyBars(t0, tc.hours, "1.10", "0.90", 4),
			})}
			ts := httpTestServer(t, v1.New(v1.Options{History: reader}))

			resp := mustGet(t, ts.URL+seriesWindowURL("native", "crypto:BTC", t0, 12, tc.limit))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var body struct {
				Data v1.OHLCSeriesResponse `json:"data"`
			}
			mustDecode(t, resp, &body)
			if len(body.Data.Intervals) != len(tc.wantHours) {
				t.Fatalf("len(intervals) = %d, want %d", len(body.Data.Intervals), len(tc.wantHours))
			}
			for i, bar := range body.Data.Intervals {
				if want := t0.Add(time.Duration(tc.wantHours[i]) * time.Hour); !bar.T.Time().Equal(want) {
					t.Errorf("Intervals[%d].t = %s, want %s — a cut keeps the NEWEST buckets",
						i, bar.T.Time().Format(time.RFC3339), want.Format(time.RFC3339))
				}
				if bar.Truncated != tc.truncated {
					t.Errorf("Intervals[%d].truncated = %v, want %v (RLT-453)", i, bar.Truncated, tc.truncated)
				}
			}
		})
	}
}

// TestOHLCSeries_DefaultWindowFullOfBarsIsNotTruncated is the twin the
// first attempt's test got backwards: it asserted truncated=true for a
// request with no from/to whose reader returned exactly `limit` bars.
// That is the ordinary default request on a dense pair — the handler
// sized the window to `limit` intervals itself, so `limit` bars is the
// WHOLE window and nothing was dropped.
func TestOHLCSeries_DefaultWindowFullOfBarsIsNotTruncated(t *testing.T) {
	// Every hour for the last two days, so whatever two-hour window
	// "now" snaps to is fully populated.
	start := time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	hours := make([]int, 48)
	for i := range hours {
		hours[i] = i
	}
	reader := &stubHistoryReader{ohlcSeriesFn: newestNSeriesFn(map[string][]v1.OHLCSeriesBar{
		"native/crypto:BTC": hourlyBars(start, hours, "1.10", "0.90", 4),
	})}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=crypto:BTC&interval=1h&limit=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data v1.OHLCSeriesResponse `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data.Intervals) != 2 {
		t.Fatalf("len(intervals) = %d, want 2 — the default window is `limit` intervals wide", len(body.Data.Intervals))
	}
	for i, bar := range body.Data.Intervals {
		if bar.Truncated {
			t.Errorf("Intervals[%d].truncated = true, want false — the default window holds exactly "+
				"`limit` intervals, all served; nothing was dropped", i)
		}
	}
}

// TestOHLCSeries_FiatCombineLimitServesNewestCompleteBuckets is RLT-453
// through the consumer the store-level fix alone made WORSE.
//
// A fiat quote is answered by combining several constituent series, and
// each constituent read carries the request's `limit`. With the store
// keeping the NEWEST rows of a capped read, a dense constituent is cut
// to its last few hours while a sparse one still reaches far back — so
// the merged set's OLD end is made of buckets the dense constituent
// really traded in but was not asked for. Trimming that merged set to
// its earliest `limit` (what the combine did, to match the store's old
// ASC order) served exactly those: 04:00 and 08:00 as n=1 bars where the
// market printed 101, beside a 09:00 that is not among the newest three
// either. Wrong values, not merely stale ones.
//
// The newest `limit` buckets of the merged set are inside every
// constituent's own newest `limit`, so they are complete — and for the
// same reason the held-back pass cannot mistake one of them for a
// bucket the book left unanswered. The pool below trades in hours the
// book answered (07 and 09–11) at a mark far outside the book's range;
// it must appear in no served bar.
func TestOHLCSeries_FiatCombineLimitServesNewestCompleteBuckets(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sparseHours := map[int]bool{0: true, 4: true, 8: true, 11: true}
	const (
		denseN, sparseN       = int64(100), int64(1)
		denseHigh, denseLow   = "1.10", "0.90"
		sparseHigh, sparseLow = "1.20", "0.80"
		poolMark              = "5.00"
	)
	byPair := map[string][]v1.OHLCSeriesBar{
		// Established, dense: the book, every hour.
		pegAliasAquaClassic + "/" + usdcClassicID: hourlyBars(t0,
			[]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, denseHigh, denseLow, denseN),
		// Established, sparse: one print in four of the twelve hours.
		pegAliasAquaClassic + "/crypto:USDT": hourlyBars(t0, []int{0, 4, 8, 11}, sparseHigh, sparseLow, sparseN),
		// Held back: a thin pool, only in hours the book answered.
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC: hourlyBars(t0, []int{7, 9, 10, 11}, poolMark, poolMark, 1),
	}

	for _, tc := range []struct {
		limit     int
		truncated bool
	}{
		{limit: 3, truncated: true},   // the demonstrated red case
		{limit: 5, truncated: true},   // reaches 07:00, where only the book and the pool trade
		{limit: 12, truncated: false}, // the whole window: nothing dropped
	} {
		t.Run("limit="+strconv.Itoa(tc.limit), func(t *testing.T) {
			reader := &stubHistoryReader{ohlcSeriesFn: newestNSeriesFn(byPair)}
			ts := httpTestServer(t, v1.New(v1.Options{
				History:           reader,
				USDPeggedClassics: []canonical.Asset{usdc},
			}))
			resp := mustGet(t, ts.URL+seriesWindowURL(pegAliasAquaClassic, "fiat:USD", t0, 12, tc.limit))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var env fiatSeriesEnvelope
			mustDecode(t, resp, &env)
			if len(env.Data.Intervals) != tc.limit {
				t.Fatalf("len(intervals) = %d, want %d (reads=%v)", len(env.Data.Intervals), tc.limit, reader.ohlcPairs)
			}
			for i, bar := range env.Data.Intervals {
				if want := t0.Add(time.Duration(12-tc.limit+i) * time.Hour); !bar.T.Time().Equal(want) {
					t.Errorf("Intervals[%d].t = %s, want %s — a capped fiat series serves the NEWEST "+
						"`limit` buckets of the window", i, bar.T.Time().Format(time.RFC3339), want.Format(time.RFC3339))
				}
				// Values are judged against the hour the bar CLAIMS, so a
				// wrong bucket that is also wrong-valued says so.
				hr := int(bar.T.Time().Sub(t0) / time.Hour)
				wantN, wantH, wantL := denseN, denseHigh, denseLow
				if sparseHours[hr] {
					wantN, wantH, wantL = denseN+sparseN, sparseHigh, sparseLow
				}
				if bar.N != wantN {
					t.Errorf("%02d:00 n = %d, want %d — a served bar must carry EVERY established "+
						"constituent's prints, never the remainder a capped read left behind, and never the pool's",
						hr, bar.N, wantN)
				}
				if g, w := mustFloat(t, bar.H), mustFloat(t, wantH); !approxEq(g, w) {
					t.Errorf("%02d:00 h = %s, want %s", hr, bar.H, wantH)
				}
				if g, w := mustFloat(t, bar.L), mustFloat(t, wantL); !approxEq(g, w) {
					t.Errorf("%02d:00 l = %s, want %s", hr, bar.L, wantL)
				}
				if bar.Truncated != tc.truncated {
					t.Errorf("%02d:00 truncated = %v, want %v", hr, bar.Truncated, tc.truncated)
				}
			}
		})
	}
}

// TestOHLCSeries_StatesPerBarVolumeScale pins GH-1152 on the direct
// (non-fiat) series path: v_base/v_quote are smallest-unit sums at the
// scale of the venues in the bucket, so each bar must state that scale.
// Without it a chart reading v_quote had nothing to divide by and plotted
// raw integers. A bar whose reader named no venue carries no scale at all
// rather than a guessed one.
func TestOHLCSeries_StatesPerBarVolumeScale(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	onchain := mkSeriesBar(t0, "0.16", "0.17", "0.15", "0.165", "10000000000", "1650000000", 4)
	onchain.Sources = []string{"sdex"}
	cex := mkSeriesBar(t0.Add(time.Hour), "0.165", "0.18", "0.16", "0.175", "100000000000", "17500000000", 5)
	cex.Sources = []string{"binance"}
	unknown := mkSeriesBar(t0.Add(2*time.Hour), "0.175", "0.18", "0.17", "0.18", "1000", "180", 1)
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{onchain, cex, unknown}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=crypto:BTC&interval=1h&limit=24")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"v_base_decimals":7`, `"v_quote_decimals":7`, `"v_base_decimals":8`, `"v_quote_decimals":8`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
	var env struct {
		Data v1.OHLCSeriesResponse `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Intervals) != 3 {
		t.Fatalf("len(intervals) = %d, want 3", len(env.Data.Intervals))
	}
	for i, want := range []int{7, 8} {
		b := env.Data.Intervals[i]
		if b.VBaseDecimals == nil || *b.VBaseDecimals != want || b.VQuoteDecimals == nil || *b.VQuoteDecimals != want {
			t.Fatalf("bar %d decimals = %v/%v, want %d/%d", i, b.VBaseDecimals, b.VQuoteDecimals, want, want)
		}
	}
	// 1000 XLM in both hours: only the stated scale makes them equal.
	for i, b := range env.Data.Intervals[:2] {
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(*b.VBaseDecimals)), nil)
		if units := new(big.Rat).SetFrac(mustBigInt(b.VBase), scale); units.Cmp(big.NewRat(1000, 1)) != 0 {
			t.Errorf("bar %d v_base / 10^decimals = %s, want 1000", i, units.FloatString(7))
		}
	}
	if u := env.Data.Intervals[2]; u.VBaseDecimals != nil || u.VQuoteDecimals != nil {
		t.Errorf("bar with no named venue states decimals %v/%v, want none", u.VBaseDecimals, u.VQuoteDecimals)
	}
}

// TestOHLCSeries_FiatCombinedStatesLiftTarget pins GH-1152 on the
// fiat-combine path: a bucket merging a 7dp on-chain leg with an 8dp CEX
// leg is summed at 8dp, and the bar must say 8 — the combine's own lift
// target, which finalize used to drop.
func TestOHLCSeries_FiatCombinedStatesLiftTarget(t *testing.T) {
	ts := httpTestServer(t, mixedScaleFiatServer(t))
	bar := fetchMixedScaleSeriesBar(t, ts.URL)
	if bar.VBaseDecimals == nil || *bar.VBaseDecimals != 8 || bar.VQuoteDecimals == nil || *bar.VQuoteDecimals != 8 {
		t.Fatalf("combined bar decimals = %v/%v, want 8/8 (the bucket's common scale)", bar.VBaseDecimals, bar.VQuoteDecimals)
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(8), nil))
	if got := new(big.Rat).Quo(mustRat(t, bar.VQuote), scale); got.Cmp(big.NewRat(220, 1)) != 0 {
		t.Errorf("v_quote / 10^v_quote_decimals = %s USD, want 220", got.FloatString(10))
	}
}

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
// {intervals: [...]} wire shape. Without it, /v1/ohlc?interval=...
// would return a single OHLCBar and ignore the param entirely.
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
	// (see capOHLCSeriesNewest). Anything else forwarded here is
	// a bug — `limit` loses the truncated signal, more over-reads.
	if reader.LastLimit() != 25 {
		t.Errorf("storage call limit = %d, want 25 (limit=24 + the one-row truncation probe)", reader.LastLimit())
	}
}

// TestOHLCSeries_StatesVolumeScaleOnWire pins the scale statement
// for the non-combined path: a series bar must state the smallest-unit
// scale its v_base/v_quote are expressed in (v_base_decimals /
// v_quote_decimals), resolved from the CAGG's own `sources` column —
// mirroring [OHLCBar.QuoteVolumeDecimals] for the single-bar
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
// doc comment. Server-side folded intervals (3d/2w/2h) grid
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
// provably off that grid for every possible "now".
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
// instead). Mirrors the back-compat contract: clients that
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

// TestOHLCSeries_TriangulatedFalseForDirectlyQuotedFiatSeries:
// a fiat-quoted series served ENTIRELY by the directly-quoted
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
// `ORDER BY bucket DESC LIMIT n` + reverse. A stub that
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
// half of the newest-buckets cap. OHLCSeriesBar.Truncated was declared on the wire
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

// TestOHLCSeries_FiatCombineLimitServesNewestCompleteBuckets covers the newest-buckets cap
// through the consumer that a store-level cap alone would make WORSE.
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

// TestOHLCSeries_StatesPerBarVolumeScale pins, on the direct
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

// TestOHLCSeries_FiatCombinedStatesLiftTarget pins, on the
// fiat-combine path: a bucket merging a 7dp on-chain leg with an 8dp CEX
// leg is summed at 8dp, and the bar must say 8 — the combine's own lift
// target, which finalize must not drop.
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

// A bar names the venues behind it on both read paths, so a derived
// series (poloniex_via_btc) is distinguishable from fill-derived bars.
func TestOHLCSeries_ServesPerBarSources(t *testing.T) {
	t0 := time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)
	derived := mkSeriesBar(t0, "0.0019", "0.0019", "0.0019", "0.0019", "100", "1", 1)
	derived.Sources = []string{"poloniex_via_btc"}
	reader := &stubHistoryReader{
		ohlcSeriesFn: func(_ context.Context, pair canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.OHLCSeriesBar, error) {
			if pair.Base.String() == "crypto:XLM" && (pair.Quote.String() == "fiat:USD" || pair.Quote.String() == "crypto:BTC") {
				return []v1.OHLCSeriesBar{derived}, nil
			}
			return nil, nil
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))

	for _, quote := range []string{"fiat:USD", "crypto:BTC"} {
		resp := mustGet(t, ts.URL+"/v1/ohlc?base=crypto:XLM&quote="+quote+"&interval=1d")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("quote=%s: status = %d", quote, resp.StatusCode)
		}
		var body struct {
			Data struct {
				Intervals []struct {
					Sources []string `json:"sources"`
				} `json:"intervals"`
			} `json:"data"`
		}
		mustDecode(t, resp, &body)
		if len(body.Data.Intervals) != 1 || len(body.Data.Intervals[0].Sources) != 1 || body.Data.Intervals[0].Sources[0] != "poloniex_via_btc" {
			t.Errorf("quote=%s: intervals = %+v, want one bar with sources [poloniex_via_btc]", quote, body.Data.Intervals)
		}
	}
}

// The annotation is one-way: it fires only for a window entirely below a
// KNOWN floor. A straddling or in-coverage window is a genuine market
// answer, and a failed or empty probe leaves the response unannotated
// rather than guessing from a database hiccup or a pair too new to have a
// closed daily bucket.
func TestOHLCSeries_CoverageFloorAnnotation(t *testing.T) {
	const below1, below2 = "2016-01-01T00:00:00Z", "2016-03-01T00:00:00Z"
	for _, tc := range []struct {
		name        string
		probe       *coverageFloorProbe
		from, to    string
		wantOutside bool
		wantFloor   bool // coverage_from echoed, even when the flag is off
	}{
		{"below the floor is flagged", &coverageFloorProbe{floor: xlmCoverageFloor, found: true}, below1, below2, true, true},
		{"straddling window is not flagged", &coverageFloorProbe{floor: xlmCoverageFloor, found: true}, "2018-01-01T00:00:00Z", "2019-01-01T00:00:00Z", false, true},
		{"quiet window inside coverage is not flagged", &coverageFloorProbe{floor: xlmCoverageFloor, found: true}, "2023-01-01T00:00:00Z", "2023-02-01T00:00:00Z", false, true},
		{"probe error yields no signal", &coverageFloorProbe{err: errors.New("prices_1d unavailable")}, below1, below2, false, false},
		{"unknown floor yields no signal", &coverageFloorProbe{found: false}, below1, below2, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := ohlcCoverageGet(t, ohlcCoverageServer(t, tc.probe), tc.from, tc.to)
			if env.Flags.OutsideCoverage != tc.wantOutside {
				t.Errorf("flags.outside_coverage = %v, want %v", env.Flags.OutsideCoverage, tc.wantOutside)
			}
			if !tc.wantFloor {
				if env.CoverageFrom != nil {
					t.Errorf("coverage_from = %v, want absent", env.CoverageFrom)
				}
				return
			}
			if env.CoverageFrom == nil || !env.CoverageFrom.Equal(xlmCoverageFloor) {
				t.Errorf("coverage_from = %v, want %s", env.CoverageFrom, xlmCoverageFloor)
			}
		})
	}
}

// TestOHLCSeries_FiatQuoteFloorIsTheConstituentSet pins the fold on
// /v1/ohlc for AQUA/fiat:USD, whose combined series draws on AQUA/USDC
// among others. Each case names the window under test and what the
// literal-pair probe would have said instead.
func TestOHLCSeries_FiatQuoteFloorIsTheConstituentSet(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)
	usd := mustParseAsset(t, "fiat:USD")
	const pairQS = "base=" + aquaClassicID + "&quote=fiat:USD"

	cases := []struct {
		name        string
		byPair      map[string]time.Time
		failPairs   map[string]bool
		from, to    string
		wantFrom    *time.Time
		wantOutside bool
	}{
		{
			// The direct pair's buckets begin 2024 (a late CEX feed); the
			// USDC constituent's begin 2021. A 2022 window is inside the
			// served history — the literal-pair probe called it uncovered.
			name: "earlier constituent lifts the floor",
			byPair: map[string]time.Time{
				probeKey(aqua, usd):  directFloor2024,
				probeKey(aqua, usdc): pegFloor2021,
			},
			from: "2022-01-01T00:00:00Z", to: "2022-02-01T00:00:00Z",
			wantFrom: &pegFloor2021, wantOutside: false,
		},
		{
			// The direct pair's buckets begin 2018, a constituent's 2021.
			// The set's floor is the EARLIEST — a later constituent never
			// drags it — so a 2019 window is quiet, not uncovered.
			name: "later constituent does not drag the floor",
			byPair: map[string]time.Time{
				probeKey(aqua, usd):  xlmCoverageFloor,
				probeKey(aqua, usdc): pegFloor2021,
			},
			from: "2019-01-01T00:00:00Z", to: "2019-02-01T00:00:00Z",
			wantFrom: &xlmCoverageFloor, wantOutside: false,
		},
		{
			// No direct bucket at all; only the USDC constituent, from
			// 2021. The literal-pair probe found nothing and stayed
			// silent; the set says the 2019 window is below the floor.
			name: "constituent-only floor flags a window below it",
			byPair: map[string]time.Time{
				probeKey(aqua, usdc): pegFloor2021,
			},
			from: "2019-01-01T00:00:00Z", to: "2019-02-01T00:00:00Z",
			wantFrom: &pegFloor2021, wantOutside: true,
		},
		{
			name: "constituent-only floor reads a later window as quiet",
			byPair: map[string]time.Time{
				probeKey(aqua, usdc): pegFloor2021,
			},
			from: "2022-01-01T00:00:00Z", to: "2022-02-01T00:00:00Z",
			wantFrom: &pegFloor2021, wantOutside: false,
		},
		{
			name:   "no constituent holds a bucket",
			byPair: map[string]time.Time{},
			from:   "2019-01-01T00:00:00Z", to: "2019-02-01T00:00:00Z",
			wantFrom: nil, wantOutside: false,
		},
		{
			// The direct pair answered 2024 but the USDC constituent's
			// probe FAILED. The constituent might hold the earliest
			// bucket, so a floor computed without it could be too late —
			// the set is unknown, and the response carries no claim.
			name: "a failed constituent silences the set",
			byPair: map[string]time.Time{
				probeKey(aqua, usd): directFloor2024,
			},
			failPairs: map[string]bool{probeKey(aqua, usdc): true},
			from:      "2022-01-01T00:00:00Z", to: "2022-02-01T00:00:00Z",
			wantFrom: nil, wantOutside: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &coverageFloorProbe{byPair: tc.byPair, failPairs: tc.failPairs}
			ts := fiatCoverageServer(t, probe)
			env := ohlcCoverageGetPair(t, ts, pairQS, tc.from, tc.to)
			assertCoverage(t, env.coverageMeta, tc.wantFrom, tc.wantOutside)
		})
	}
}

// TestOHLCSeries_NativeUSDIsNotQuietSinceAFloorItIsNotServedFrom is the
// shape that motivated the constituent-set fold: `native/fiat:USD`
// holds no daily bucket under any of XLM's spellings, and its combined
// series is served from the USDC constituent, whose buckets begin 2021.
// A 2019 window on it is BELOW the served floor. It must not come back
// as a quiet window with a floor the served set does not hold — the
// only floor that can appear is the constituent's, and the window must
// be flagged against it.
func TestOHLCSeries_NativeUSDIsNotQuietSinceAFloorItIsNotServedFrom(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	native := canonical.NativeAsset()
	pegFloor := time.Date(2021, 2, 1, 0, 0, 0, 0, time.UTC)
	probe := &coverageFloorProbe{byPair: map[string]time.Time{
		// Only the peg constituent holds buckets; native/fiat:USD and
		// crypto:XLM/fiat:USD (one probe key) hold none.
		probeKey(native, usdc): pegFloor,
	}}
	ts := fiatCoverageServer(t, probe)

	env := ohlcCoverageGetPair(t, ts, "base=native&quote=fiat:USD", "2019-01-01T00:00:00Z", "2019-02-01T00:00:00Z")
	if env.CoverageFrom != nil && env.CoverageFrom.Equal(xlmCoverageFloor) {
		t.Fatalf("coverage_from = %s — a floor the served constituent set does not hold", xlmCoverageFloor.Format(time.RFC3339))
	}
	assertCoverage(t, env.coverageMeta, &pegFloor, true)
	if calls, _, _, _ := probe.snapshot(); calls < 2 {
		t.Errorf("probe calls = %d, want the constituents probed, not the literal pair alone", calls)
	}
}

// TestOHLCSeries_FiatQuoteBookOutranksSACQuotedPool — AQUA under r1's
// registry (the USDC and AQUA wrappers both declared): the book
// `AQUA/USDC-GA5Z…` holds one bar on day 1 (n=1, 100 base); the pool
// `<AQUA SAC>/<USDC SAC>` holds bars on day 1 (n=50, high 0.50, low
// 0.01) and on day 2.
//
// Served: TWO bars. Day 1 is the book's
// alone — the pool is dropped from that bucket entirely, so its two
// prints at 0.50 and 0.01 cannot become the bar's high and low, which is
// what "outranks" means here — and day 2 is the pool's, because the
// book cannot answer it and the alternative is reporting a day the
// market traded as quiet.
//
// The per-bucket guarantee is the day-1 assertions.
func TestOHLCSeries_FiatQuoteBookOutranksSACQuotedPool(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	day1 := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	book := mkSeriesBar(day1, "0.0041", "0.0042", "0.0040", "0.0041", "100000", "410", 1)
	poolDay2 := mkSeriesBar(day2, "0.0035", "0.0036", "0.0034", "0.0035", "10000", "35", 3)
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		pegAliasAquaClassic + "/" + usdcClassicID: {book},
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC: {
			mkSeriesBar(day1, "0.0030", "0.5000", "0.0100", "0.0035", "20", "0.07", 50),
			poolDay2,
		},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := fiatSeriesGet(t, ts, pegAliasAquaClassic)
	if len(env.Data.Intervals) != 2 {
		t.Fatalf("intervals = %d, want the book's day-1 bar and the pool's day-2 bar: %+v (reads=%v)",
			len(env.Data.Intervals), env.Data.Intervals, reader.ohlcPairs)
	}
	// Day 1: the book's own bar, print for print. The pool's 50 prints
	// and its 0.50/0.01 extremes are not blended in and not weighted
	// down — they are not in this bucket at all.
	assertBookBar(t, env.Data.Intervals[0], book)
	// Day 2: the pool's, where the book has nothing to say.
	assertBookBar(t, env.Data.Intervals[1], poolDay2)
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; the series was served through the peg")
	}
	assertSACQuotedSeriesReadLast(t, reader.ohlcPairs)
	// The population is the classic peg under every base spelling.
	for _, spelling := range []string{
		pegAliasAquaClassic + "/" + usdcClassicID,
		pegAliasAquaSAC + "/" + usdcClassicID,
	} {
		if callIndex(reader.ohlcPairs, spelling) < 0 {
			t.Errorf("%s never read (reads=%v)", spelling, reader.ohlcPairs)
		}
	}
}

// TestOHLCSeries_XLMBookOutranksSACQuotedPool — XLM under r1's
// registry: `native/USDC-GA5Z…` holds a bar of 100 trades over
// 6,000,000 units; `<XLM SAC>/<USDC SAC>` holds a two-trade bar with
// high 0.50 and low 0.01 in the same bucket.
//
// The served bucket carries n=100 and the book's own high 0.20 and low
// 0.18 under every XLM spelling of the request — the fiat combine folds
// the base spellings, so which one was named does not change the answer.
// The measured shape this pins out is n=102 with high 0.50 and low 0.01:
// two prints setting a bar's extremes beside six million units of book
// volume.
//
// The SAC-quoted spelling IS read — that is
// how a bucket the book cannot answer gets served at all — so what holds
// the answer still is the per-bucket gate, not an absent read. The read
// order is asserted instead: every established spelling first.
func TestOHLCSeries_XLMBookOutranksSACQuotedPool(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	day := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	book := mkSeriesBar(day, "0.19", "0.20", "0.18", "0.195", "6000000", "1140000", 100)
	pool := mkSeriesBar(day, "0.19", "0.50", "0.01", "0.19", "20", "4", 2)
	for _, base := range []string{"native", "crypto:XLM", canonical.XLMSacContractID} {
		t.Run(base, func(t *testing.T) {
			reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
				"native/" + usdcClassicID:                          {book},
				canonical.XLMSacContractID + "/" + pegAliasUSDCSAC: {pool},
			}}
			ts := httpTestServer(t, v1.New(v1.Options{
				History:           reader,
				USDPeggedClassics: []canonical.Asset{usdc},
			}))
			env := fiatSeriesGet(t, ts, base)
			if len(env.Data.Intervals) != 1 {
				t.Fatalf("intervals = %d, want 1: %+v (reads=%v)", len(env.Data.Intervals), env.Data.Intervals, reader.ohlcPairs)
			}
			assertBookBar(t, env.Data.Intervals[0], book)
			assertSACQuotedSeriesReadLast(t, reader.ohlcPairs)
			if callIndex(reader.ohlcPairs, canonical.XLMSacContractID+"/"+pegAliasUSDCSAC) < 0 {
				t.Errorf("the pool was never read (reads=%v) — the held-back set must be read, "+
					"or a bucket only it can answer is served as quiet", reader.ohlcPairs)
			}
		})
	}
}

// TestOHLCSeries_SACQuotedOnlyDepthIsServed — SAC-quoted-only depth is served,
// pinned from the other side.
//
// One market, AQUA quoted in the USDC SAC, with a daily bar inside the
// window; the declared peg is classic USDC. No established spelling
// holds a bucket, so every bucket is unanswered and the held-back
// spelling fills them: the series serves the pool's bar.
//
// A populated answer carries no coverage annotation at all — the floor
// exists to explain an empty one — so the probe must not run here. The
// annotation that would otherwise sit SILENT on an empty answer is the
// thing this replaces: the surface need not describe a market it
// cannot serve, because it serves it.
func TestOHLCSeries_SACQuotedOnlyDepthIsServed(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	poolFloor := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	pool := mkSeriesBar(poolFloor, "0.0041", "0.0042", "0.0040", "0.0041", "1000000", "4100", 3)
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		aquaClassicID + "/" + pegAliasUSDCSAC: {pool},
	}}
	probe := &coverageFloorProbe{byPair: map[string]time.Time{}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		CoverageFloor:     probe,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := fiatSeriesGet(t, ts, aquaClassicID)
	if len(env.Data.Intervals) != 1 {
		t.Fatalf("intervals = %d, want the pool's bar (reads=%v)", len(env.Data.Intervals), reader.ohlcPairs)
	}
	assertBookBar(t, env.Data.Intervals[0], pool)
	assertSACQuotedSeriesReadLast(t, reader.ohlcPairs)
	if env.CoverageFrom != nil || env.Flags.OutsideCoverage {
		t.Errorf("populated answer carries coverage_from=%v outside_coverage=%v; the floor annotates empties only",
			env.CoverageFrom, env.Flags.OutsideCoverage)
	}
	if n := len(probe.probed()); n != 0 {
		t.Errorf("%d coverage probes issued for a populated series", n)
	}
}

// TestOHLCSeries_SACQuotedDepthOutsideTheWindowStillCarriesItsFloor —
// the annotation half of the same change. The pool's only bucket sits
// BEFORE the requested window, so the series is genuinely empty, and the
// floor must now name the pool's first bucket: the combine reads that
// market, so a floor measured over it is a claim this surface can keep.
func TestOHLCSeries_SACQuotedDepthOutsideTheWindowStillCarriesItsFloor(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	poolFloor := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		aquaClassicID + "/" + pegAliasUSDCSAC: {
			mkSeriesBar(poolFloor, "0.0041", "0.0042", "0.0040", "0.0041", "1000000", "4100", 3),
		},
	}}
	probe := &coverageFloorProbe{byPair: map[string]time.Time{
		probeKey(aqua, usdcSAC): poolFloor,
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		CoverageFloor:     probe,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))
	const pairQS = "base=" + aquaClassicID + "&quote=fiat:USD"

	env := ohlcCoverageGetPair(t, ts, pairQS, "2024-01-01T00:00:00Z", "2024-02-01T00:00:00Z")
	assertCoverage(t, env.coverageMeta, &poolFloor, true)
	if probe.literal() == 0 {
		t.Error("no quote-literal probe was issued; the fiat series must not be measured with the quote leg alias-folded")
	}
}

// TestOHLCSeries_FiatProbeSpansWhatTheCombineReads pins the equality
// the annotation rests on, so the two sides cannot come apart by
// editing one: on an empty answer the set of literal pairs the combine
// REQUESTED must equal the set of markets the floor probes SPAN.
//
// A quote-literal probe spans its base leg's alias family crossed with
// the one quote spelling it was given, in both stored directions — so
// its span is computed here the way the SQL computes it, and the memo's
// collapse of repeated quote spellings is why the probe list is the
// shorter of the two. Pinned for a SAC-declared base and for XLM's
// three-form base, under the registry shape r1 runs.
//
// The SAC-quoted market is on BOTH sides of
// the equality: the combine reads a declared peg's SAC wrapper, so the
// floor measures it. The equality is what keeps the two honest — a probe
// wider than the read reports a served-and-empty window as quiet, and a
// probe narrower than the read leaves a market it can serve unmeasured.
// An empty answer is the only one a floor annotates, and an empty answer
// is one where the held-back set was read too, so the set the probe must
// span is the whole constituent list.
func TestOHLCSeries_FiatProbeSpansWhatTheCombineReads(t *testing.T) {
	cases := []struct {
		name     string
		registry func(*testing.T) canonical.Asset
		base     string
	}{
		{"AQUA under the USDC wrapper alone", installUSDCSACRegistry, aquaClassicID},
		{"AQUA under r1's registry", installPegAliasRegistry, aquaClassicID},
		{"native under r1's registry", installPegAliasRegistry, "native"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usdc := tc.registry(t)
			if forms := canonical.AssetAliases(usdc); len(forms) < 2 {
				t.Fatalf("fixture puts no second quote spelling in play: %v", forms)
			}
			reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{}}
			probe := &coverageFloorProbe{byPair: map[string]time.Time{}}
			ts := httpTestServer(t, v1.New(v1.Options{
				History:           reader,
				CoverageFloor:     probe,
				USDPeggedClassics: []canonical.Asset{usdc},
			}))
			ohlcCoverageGetPair(t, ts, "base="+tc.base+"&quote=fiat:USD", "2024-01-01T00:00:00Z", "2024-02-01T00:00:00Z")

			requested := map[string]bool{}
			for _, raw := range reader.ohlcPairs {
				p, err := canonical.ParsePair(raw)
				if err != nil {
					t.Fatalf("ParsePair(%q): %v", raw, err)
				}
				requested[coverageMarketKey(p.Base, p.Quote)] = true
			}
			span := map[string]bool{}
			for _, rd := range probe.probedReads() {
				for _, k := range probeSpanKeys(rd.pair, rd.span) {
					sp, err := canonical.ParsePair(k)
					if err != nil {
						continue // one asset against itself: NewPair rejects it, so the combine never requests it
					}
					span[coverageMarketKey(sp.Base, sp.Quote)] = true
				}
			}
			if len(span) == 0 {
				t.Fatalf("no probe spanned anything; reads=%v", reader.ohlcPairs)
			}
			if missing := coverageSetDiff(span, requested); len(missing) > 0 {
				t.Errorf("the floor spans markets the combine never requested: %v (span=%v)", missing, coverageKeys(span))
			}
			if extra := coverageSetDiff(requested, span); len(extra) > 0 {
				t.Errorf("the combine requested pairs the floor does not measure: %v", extra)
			}
			sacQuoted := coverageMarketKey(mustParseAsset(t, tc.base), mustParseAsset(t, pegAliasUSDCSAC))
			if !requested[sacQuoted] {
				t.Errorf("%s was not requested — a declared peg's SAC wrapper is where a Soroban pool's "+
					"USD leg lives, and an empty answer means every established spelling missed", sacQuoted)
			}
			if !span[sacQuoted] {
				t.Errorf("%s is not in the probed span — the combine reads it, so the floor must measure it "+
					"or an empty window it could serve carries no explanation", sacQuoted)
			}
		})
	}
}

// TestOHLCSeries_NonstandardDecimals_NormalizesBarsNotVolumes pins the
// series-mode contract:
//
//   - o/h/l/c: the raw prices_<n> CAGG ratio × K (K = 10^(9−7) = 100
//     for a 9dp base vs a 7dp classic quote) — the same factor the
//     single-bar path applies, since every bar shares the pair.
//   - v_base/v_quote: UNCHANGED. The CAGG's volume columns are raw
//     smallest-unit sums in each asset's OWN declared decimals
//     (migration 0002: volume = Σ(base_amount)), and the wire contract
//     (OHLCBar/VWAPResult doc: "in the asset's smallest unit") promises
//     exactly that — same precedent as /v1/history, which serves raw
//     base_amount/quote_amount plus base_decimals/quote_decimals
//     metadata and never rescales amounts. Scaling volumes by 10^(7−dec)
//     would silently break the smallest-unit contract.
func TestOHLCSeries_NonstandardDecimals_NormalizesBarsNotVolumes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{{
		T: v1.WireTime(t0),
		// Raw CAGG ratios (quote_amount/base_amount on smallest units):
		// true price is 100x these for a 9dp base vs 7dp quote.
		O: "2.5", H: "3", L: "2", C: "2.5",
		// Raw smallest-unit sums: 10^11 base units at 9dp = 100 tokens;
		// 2.5*10^9 quote units at 7dp = 250 USDC.
		VBase: "100000000000", VQuote: "2500000000",
		N: 4,
	}}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/ohlc?base="+flaggedAsset+"&quote="+classicUSDC+"&interval=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("series mode: status = %d, want 200 (CAGG read is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"o":"250.0000000000"`,
		`"h":"300.0000000000"`,
		`"l":"200.0000000000"`,
		`"c":"250.0000000000"`,
		// Volumes byte-identical to the raw CAGG values.
		`"v_base":"100000000000"`,
		`"v_quote":"2500000000"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("series body missing %q: %s", want, body)
		}
	}
}

// TestOHLCSeries_NonstandardDecimals_7dpByteIdentical proves wiring the
// cache does NOT reformat an unflagged (7dp/7dp) pair's bars — the CAGG's
// NUMERIC::text strings must pass through byte-identical (AdjustPrice's
// no-op contract), not get re-rendered at 10 fixed digits.
func TestOHLCSeries_NonstandardDecimals_7dpByteIdentical(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9) // flagged asset NOT in this pair
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{{
		T: v1.WireTime(t0), O: "0.16", H: "0.17", L: "0.15", C: "0.165",
		VBase: "1000", VQuote: "165", N: 4,
	}}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote="+classicUSDC+"&interval=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"o":"0.16"`, `"h":"0.17"`, `"l":"0.15"`, `"c":"0.165"`,
		`"v_base":"1000"`, `"v_quote":"165"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("7dp bars must be byte-identical; body missing %q: %s", want, body)
		}
	}
}

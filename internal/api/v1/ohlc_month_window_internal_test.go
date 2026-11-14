package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestOHLCMonthWindow_CoversWholeCalendarMonths: on every day of a year the default window starts
// and ends on month starts and holds exactly `limit` closed month buckets.
func TestOHLCMonthWindow_CoversWholeCalendarMonths(t *testing.T) {
	for day := time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC); day.Year() == 2026; day = day.AddDate(0, 0, 1) {
		for _, limit := range []int{1, 12} {
			from, to := ohlcMonthWindow(day, false, limit)
			wantTo := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
			if !to.Equal(wantTo) || !from.Equal(wantTo.AddDate(0, -limit, 0)) {
				t.Fatalf("%s limit=%d: window [%s, %s), want %d months ending %s", day.Format(time.DateOnly), limit, from, to, limit, wantTo)
			}
		}
	}
}

// TestOHLCMonthWindow_ExplicitTo keeps `to` and counts month buckets back from it.
func TestOHLCMonthWindow_ExplicitTo(t *testing.T) {
	mid := time.Date(2026, 3, 31, 9, 0, 0, 0, time.UTC)
	from, to := ohlcMonthWindow(mid, true, 2)
	if !to.Equal(mid) || !from.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("mid-month to: [%s, %s), want Feb 1 .. Mar 31 (the Feb and Mar buckets)", from, to)
	}
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	from, to = ohlcMonthWindow(start, true, 2)
	if !to.Equal(start) || !from.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month-start to: [%s, %s), want Jan 1 .. Mar 1", from, to)
	}
}

// TestChartServedGranularity_ChargeFollowsTheRead: a since-inception read is priced by the grain
// it walks after the specialised paths snap the request.
func TestChartServedGranularity_ChargeFollowsTheRead(t *testing.T) {
	usd, eur := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}, canonical.Asset{Type: canonical.AssetFiat, Code: "EUR"}
	xlmUSD := canonical.Pair{Base: canonical.NativeAsset(), Quote: usd}
	for _, tc := range []struct {
		pair            canonical.Pair
		priceType, gran string
		want            string
	}{
		{xlmUSD, "twap", "4h", "1h"},
		{xlmUSD, "twap", "1m", "1h"},
		{xlmUSD, "twap", "x7", "1h"},
		{xlmUSD, "twap", "1w", "1d"},
		{xlmUSD, "market_cap", "1h", "1d"},
		{canonical.Pair{Base: eur, Quote: usd}, "vwap", "1h", "1d"},
		{xlmUSD, "vwap", "4h", "4h"},
	} {
		if got := chartServedGranularity(tc.pair, tc.priceType, tc.gran); got != tc.want {
			t.Errorf("%s %s: served grain = %q, want %q", tc.priceType, tc.gran, got, tc.want)
		}
	}
	if twap4h, oneH := sinceInceptionCost(chartServedGranularity(xlmUSD, "twap", "4h")), sinceInceptionCost("1h"); twap4h != oneH {
		t.Errorf("twap 4h charged %d tokens, want the 1h price %d", twap4h, oneH)
	}
}

// TestChartWindowCost_PricedByTheGridWalked: every default chart costs one token; a client-chosen fine grain pays for its grid.
func TestChartWindowCost_PricedByTheGridWalked(t *testing.T) {
	usd, eur := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}, canonical.Asset{Type: canonical.AssetFiat, Code: "EUR"}
	xlmUSD := canonical.Pair{Base: canonical.NativeAsset(), Quote: usd}
	for tf, spec := range chartTimeframes {
		if spec.Duration == 0 {
			continue
		}
		if got := chartWindowCost(xlmUSD, "vwap", spec.DefaultGranule, spec.Duration); got != 1 {
			t.Errorf("default %s at %s costs %d tokens, want 1", tf, spec.DefaultGranule, got)
		}
	}
	day := 24 * time.Hour
	for _, tc := range []struct {
		pair            canonical.Pair
		priceType, gran string
		window          time.Duration
		want            int
	}{
		{xlmUSD, "vwap", "1m", 30 * day, 9},  // 43 200 points
		{xlmUSD, "vwap", "1m", 365 * day, 8}, // served at 15m: 35 040 points
		{xlmUSD, "vwap", "1m", 7 * day, 3},   // 10 080 points
		{xlmUSD, "vwap", "1h", 7 * day, 1},   // the explorer's own chart
		{xlmUSD, "twap", "1m", 365 * day, 2}, // snapped to 1h: 8 760 points
		{xlmUSD, "market_cap", "1m", 365 * day, 1},
		{canonical.Pair{Base: eur, Quote: usd}, "vwap", "1m", 365 * day, 1},
		{xlmUSD, "vwap", "nonsense", 365 * day, 1},
	} {
		if got := chartWindowCost(tc.pair, tc.priceType, tc.gran, tc.window); got != tc.want {
			t.Errorf("%s %s over %s: %d tokens, want %d", tc.priceType, tc.gran, tc.window, got, tc.want)
		}
	}
}

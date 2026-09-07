package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The grain a chart is SERVED at has to be one whose grid fits in one
// response: the reader caps at historyMaxPoints and drops the NEWEST
// buckets, so an over-fine pair is answered with its oldest slice and
// the rest of the requested window silently missing (measured on
// production 2026-09-07 at 1y/1m: 50,000 points covering 36 of 365
// days, ending three months before the request).
//
// This walks the whole prescribed timeframe table against the whole
// served granularity set, so the expectation is the SERVED VALUE for
// every cell rather than "1y/1m changed": a future rung or timeframe
// that shifts a cell has to move a line here and say which.
func TestChartFitGranularity_EveryTimeframeAndGrain(t *testing.T) {
	// want[timeframe][requested granularity] = served granularity.
	// 1y/1m is the one cell that coarsens today: 365 days of minutes
	// is 525,600 grid points against a 50,000-point cap, and 15m is
	// the finest rung that fits (35,040).
	want := map[string]map[string]string{
		"1h":  {"1m": "1m", "15m": "15m", "1h": "1h", "4h": "4h", "1d": "1d", "1w": "1w", "1mo": "1mo"},
		"24h": {"1m": "1m", "15m": "15m", "1h": "1h", "4h": "4h", "1d": "1d", "1w": "1w", "1mo": "1mo"},
		"1w":  {"1m": "1m", "15m": "15m", "1h": "1h", "4h": "4h", "1d": "1d", "1w": "1w", "1mo": "1mo"},
		"1mo": {"1m": "1m", "15m": "15m", "1h": "1h", "4h": "4h", "1d": "1d", "1w": "1w", "1mo": "1mo"},
		"1y":  {"1m": "15m", "15m": "15m", "1h": "1h", "4h": "4h", "1d": "1d", "1w": "1w", "1mo": "1mo"},
		// `all` has no requested width, so nothing is decidable from
		// the request and every grain is served as asked. Measured the
		// same day: ?timeframe=all&granularity=1h serves 47,823 points
		// spanning 2017-01-17 to now — complete, under the cap, and a
		// genesis-derived grid would have counted 96,600 and wrongly
		// coarsened it.
		"all": {"1m": "1m", "15m": "15m", "1h": "1h", "4h": "4h", "1d": "1d", "1w": "1w", "1mo": "1mo"},
	}

	if len(want) != len(chartTimeframes) {
		t.Fatalf("matrix covers %d timeframes, chartTimeframes holds %d — "+
			"a new timeframe needs a row here, not a wider assertion", len(want), len(chartTimeframes))
	}
	for tf, spec := range chartTimeframes {
		row, ok := want[tf]
		if !ok {
			t.Fatalf("timeframe %q has no row in the matrix", tf)
		}
		if len(row) != len(timescale.AllHistoryGranularities) {
			t.Fatalf("timeframe %q covers %d grains, the served set holds %d",
				tf, len(row), len(timescale.AllHistoryGranularities))
		}
		for _, g := range timescale.AllHistoryGranularities {
			req := string(g)
			expect, ok := row[req]
			if !ok {
				t.Fatalf("timeframe %q has no cell for granularity %q", tf, req)
			}
			got := chartFitGranularity(chartVWAPGranularityLadder, req, spec.Duration)
			if got != expect {
				t.Errorf("timeframe=%s granularity=%s → served %q, want %q", tf, req, got, expect)
			}
			// Whatever is served must itself fit, or the coarsening
			// merely moved the truncation somewhere else.
			if !chartGranularityFits(got, spec.Duration) {
				t.Errorf("timeframe=%s granularity=%s → served %q, whose grid still exceeds the %d-point cap",
					tf, req, got, historyMaxPoints)
			}
		}
	}
}

// The served grain is never FINER than the requested one — coarsening
// may only lose resolution, never invent it, and a caller that asked
// for 1d must not be handed minute buckets it did not ask to pay for.
func TestChartFitGranularity_NeverRefines(t *testing.T) {
	rank := map[string]int{}
	for i, g := range timescale.AllHistoryGranularities {
		rank[string(g)] = i
	}
	for _, spec := range chartTimeframes {
		for _, g := range timescale.AllHistoryGranularities {
			got := chartFitGranularity(chartVWAPGranularityLadder, string(g), spec.Duration)
			if rank[got] < rank[string(g)] {
				t.Errorf("window=%s granularity=%s → served %q, which is FINER than requested",
					spec.Duration, g, got)
			}
		}
	}
}

// An unknown grain is handed through untouched so the reader can
// answer it with ErrUnknownGranularity and the handler can render the
// 400 that enumerates the served set. Coarsening it would turn a bad
// request into a 200 over a grain nobody asked for.
func TestChartFitGranularity_UnknownGrainUntouched(t *testing.T) {
	for _, gran := range []string{"2h", "30m", "", "1y"} {
		if got := chartFitGranularity(chartVWAPGranularityLadder, gran, 365*24*time.Hour); got != gran {
			t.Errorf("granularity=%q → %q, want it returned unchanged", gran, got)
		}
	}
}

// The TWAP ladder may only ever name a grain that has a TWAP CAGG
// behind it (twap_1h / twap_1d, migration 0081) — coarsening past `1d`
// or onto `4h` would read a view that does not exist.
func TestChartFitGranularity_TWAPLadderStaysOnItsTwoCAGGs(t *testing.T) {
	backed := map[string]bool{"1h": true, "1d": true}
	for tf, spec := range chartTimeframes {
		for _, req := range []string{"1m", "15m", "1h", "4h", "1d", "1w", "1mo"} {
			got := chartFitGranularity(chartTWAPGranularityLadder,
				twapChartGranularity(req), spec.Duration)
			if !backed[got] {
				t.Errorf("timeframe=%s granularity=%s → twap served %q, which has no TWAP CAGG",
					tf, req, got)
			}
		}
	}
	// And a window wide enough to blow the cap at 1h does step to 1d
	// rather than serving a truncated hourly series — the guard the
	// ladder exists for, exercised at a width no timeframe reaches yet.
	if got := chartFitGranularity(chartTWAPGranularityLadder, "1h", 10*365*24*time.Hour); got != "1d" {
		t.Errorf("10y at 1h → twap served %q, want 1d", got)
	}
}

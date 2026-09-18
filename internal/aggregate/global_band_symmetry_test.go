package aggregate

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// MNY-22 direction-symmetry regression for the ORACLE-AGGREGATOR tier
// (finding K004, seventh site — the sibling bands in outliers.go,
// outliers_local.go and served_guard.go are covered by
// band_symmetry_test.go).
//
// [rejectAggregatorOutliers] used to score a source ADDITIVELY in price
// space — `|p − centre| > K·(1.4826·MAD)` with K = [aggregatorMADFactor]
// = 5 — which is one-sided-blind by construction: a source can only ever
// be `centre` below the centre, so once K·scale reaches the centre the
// band's lower edge is non-positive and NO downward print can be
// rejected, while the mirror-image up-move still is. That happens at a
// relative MAD of 1/(5·1.4826) = 13.5 %: ordinary disagreement between
// three vendors on a thin RWA, or on a major mid-crash.
//
// The consequence is precisely the failure M8 added this filter to
// prevent, in the one direction the filter could not see: a vendor
// publishing a decimal-shifted or stale-to-zero quote is averaged
// straight into the plain-mean headline price.

// TestRejectAggregatorOutliers_CrashAndPumpBothDropped contaminates one
// dispersed source set once with a crash quote and once with a pump
// quote, and asserts BOTH are dropped and the headline is the clean
// consensus either way.
//
// Baseline sources 70/85/100/115/130 (median 100, relative MAD 15 % —
// past the 13.5 % at which the additive lower edge went non-positive).
// With the 1.00 crash quote folded in: median 92.50, MAD 22.50, so the
// old band was [92.50 − 166.79, 92.50 + 166.79] = [−74.29, 259.29] and
// the crash quote scored INSIDE it, dragging the served mean from
// 100.00 to 83.50. The ratio-symmetric lower edge is 92.50²/259.29 =
// 33.00, so it is dropped — while 70.00, the honest low source, is
// untouched.
func TestRejectAggregatorOutliers_CrashAndPumpBothDropped(t *testing.T) {
	base := []int64{7000, 8500, 10000, 11500, 13000} // 70.00 … 130.00 at 2dp

	cases := []struct {
		name    string
		outlier int64
	}{
		{"crash_quote_below", 100},    // 1.00 — a decimal-shifted vendor quote
		{"pump_quote_above", 855_625}, // 8556.25 — its ratio mirror about 92.50
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubGlobalReader{}
			reader.vwap.ok = false // force the aggregator tier
			for i, p := range base {
				reader.agg.rows = append(reader.agg.rows, mkAggRow(string(rune('a'+i)), p, 2))
			}
			reader.agg.rows = append(reader.agg.rows, mkAggRow("bad", tc.outlier, 2))

			bq, quote := usdcUSDPair(t)
			opts := DefaultGlobalPriceOptions()
			opts.AggregatorSources = []string{"a", "b", "c", "d", "e", "bad"}

			res, err := ComputeGlobalPrice(context.Background(), bq, quote, reader, opts)
			if err != nil {
				t.Fatalf("ComputeGlobalPrice: %v", err)
			}
			if res.Authority != AuthorityAggregatorAvg {
				t.Fatalf("authority = %q, want %q", res.Authority, AuthorityAggregatorAvg)
			}
			// Clean consensus mean = (70+85+100+115+130)/5 = 100.00.
			if res.Price != "100.00000000000000" {
				t.Errorf("served price = %q, want 100.00000000000000 — the %d quote must not reach the mean",
					res.Price, tc.outlier)
			}
			if len(res.Sources) != len(base) {
				t.Errorf("contributing sources = %v, want the %d agreeing vendors (the divergent quote dropped)",
					res.Sources, len(base))
			}
			for _, s := range res.Sources {
				if s == "bad" {
					t.Errorf("divergent quote survived: sources = %v", res.Sources)
				}
			}
		})
	}
}

// TestRejectAggregatorOutliers_HonestlyDispersedSetFullyKept pins the
// other side: the ratio-symmetric band must not start trimming the low
// tail of a set that is merely dispersed. All five sources of the
// uncontaminated baseline survive, so the fix cannot degenerate into
// "reject more of everything".
func TestRejectAggregatorOutliers_HonestlyDispersedSetFullyKept(t *testing.T) {
	rows := []canonical.OracleUpdate{
		mkAggRow("a", 7000, 2), mkAggRow("b", 8500, 2), mkAggRow("c", 10000, 2),
		mkAggRow("d", 11500, 2), mkAggRow("e", 13000, 2),
	}
	kept := rejectAggregatorOutliers(rows)
	if len(kept) != len(rows) {
		t.Fatalf("kept %d/%d — an honestly dispersed vendor set must survive intact: %v",
			len(kept), len(rows), sourceSet(kept))
	}
}

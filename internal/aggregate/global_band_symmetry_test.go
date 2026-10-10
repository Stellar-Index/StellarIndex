package aggregate

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

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

// TestRejectAggregatorOutliers_BetweenOldAndNewEdgeIsDropped pins the
// boundary the other two tests straddle but never land on: a source
// sitting strictly between the old additive lower edge and the new
// ratio-symmetric one.
//
// 66.00/100.00/106.745: median 100.00, MAD 6.745, scale = 1.4826·MAD ≈
// 10.0002, so K·scale ≈ 50.0008. The old additive edge, centre −
// K·scale ≈ 49.9992, would have kept 66.00. The ratio-symmetric edge,
// centre²/(centre + K·scale) ≈ 66.6663, does not: 66.00 sits inside the
// newly-tightened gap and must be dropped, not kept.
func TestRejectAggregatorOutliers_BetweenOldAndNewEdgeIsDropped(t *testing.T) {
	rows := []canonical.OracleUpdate{
		mkAggRow("lo", 66000, 3), mkAggRow("mid", 100000, 3), mkAggRow("hi", 106745, 3),
	}
	kept := rejectAggregatorOutliers(rows)
	for _, s := range kept {
		if s.Source == "lo" {
			t.Fatalf("lo survived: sources = %v — the tightened lower edge must drop a source between the old and new edge", sourceSet(kept))
		}
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d, want 2 (mid, hi): %v", len(kept), sourceSet(kept))
	}
}

func TestRejectAggregatorOutliers(t *testing.T) {
	t.Run("drops_divergent_source_MAD_positive", func(t *testing.T) {
		// Two agreeing-but-not-identical sources + one 2x outlier. The
		// MAD is positive here (agreeing sources differ slightly), so
		// this exercises the MAD-band path (not the MAD==0 path).
		rows := []canonical.OracleUpdate{
			mkAggRow("cg", 10000, 2),  // 100.00
			mkAggRow("cmc", 10500, 2), // 105.00 (agrees, ~5%)
			mkAggRow("cc", 21000, 2),  // 210.00 (2x-off outlier)
		}
		kept := rejectAggregatorOutliers(rows)
		got := sourceSet(kept)
		if len(kept) != 2 || got["cc"] {
			t.Fatalf("kept = %v, want the 2 agreeing sources (cg, cmc), outlier cc dropped", got)
		}
	})

	t.Run("all_agree_no_op", func(t *testing.T) {
		rows := []canonical.OracleUpdate{
			mkAggRow("cg", 10000, 2), mkAggRow("cmc", 10010, 2), mkAggRow("cc", 9990, 2),
		}
		if kept := rejectAggregatorOutliers(rows); len(kept) != 3 {
			t.Fatalf("tightly-agreeing sources must all survive; kept %d/3", len(kept))
		}
	})

	t.Run("two_sources_passthrough", func(t *testing.T) {
		// Only 2 sources — no majority to define a consensus, so even a
		// 2x gap is passed through unchanged (either could be right).
		rows := []canonical.OracleUpdate{
			mkAggRow("cg", 10000, 2), mkAggRow("cmc", 20000, 2),
		}
		if kept := rejectAggregatorOutliers(rows); len(kept) != 2 {
			t.Fatalf("2-source input must pass through unchanged; kept %d/2", len(kept))
		}
	})

	t.Run("always_keeps_at_least_one", func(t *testing.T) {
		// A pathological 3-way split (1, 1000, 1000000 — a 1000x low
		// print and a 1000x high print either side of the median) still
		// yields a non-empty survivor set (the median centre is always
		// a survivor), AND the survivor set must actually
		// exclude both divergent prints, not just be non-empty: a
		// downward-blind band would let the 1000x-low print "a" survive
		// alongside the median while only trimming the high side, which
		// `len(kept) == 0` can never distinguish from the correct
		// symmetric reject.
		rows := []canonical.OracleUpdate{
			mkAggRow("a", 100, 2), mkAggRow("b", 100000, 2), mkAggRow("c", 100000000, 2),
		}
		kept := rejectAggregatorOutliers(rows)
		if len(kept) == 0 {
			t.Fatal("rejectAggregatorOutliers must never fail closed to zero survivors")
		}
		got := sourceSet(kept)
		if got["a"] {
			t.Errorf("kept = %v, want the 1000x-low print (a) dropped along with the 1000x-high print (c)", got)
		}
		if got["c"] {
			t.Errorf("kept = %v, want the 1000x-high print (c) dropped", got)
		}
		if !got["b"] {
			t.Errorf("kept = %v, want the median source (b) to survive", got)
		}
	})
}

func TestRejectAggregatorOutliers_BandEdgeIsInclusive(t *testing.T) {
	// Majority at 4000 → MAD 0 → scale = 20 → half-width 5·20 = 100.
	for _, tc := range []struct {
		probe int64
		keep  bool
	}{{4100, true}, {4101, false}} {
		rows := []canonical.OracleUpdate{
			mkAggRow("a", 4000, 2), mkAggRow("b", 4000, 2), mkAggRow("c", tc.probe, 2),
		}
		got := rejectAggregatorOutliers(rows)
		if kept := len(got) == 3; kept != tc.keep {
			t.Fatalf("probe %d: kept %d/3, want probe kept=%v", tc.probe, len(got), tc.keep)
		}
	}
}

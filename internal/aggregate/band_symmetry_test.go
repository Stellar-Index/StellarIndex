package aggregate_test

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// MNY-22 direction-symmetry regressions (findings F037, F039, K004,
// RLT-391).
//
// Every robust band in this package used to be ADDITIVE in price space
// (`|p − centre| > K·scale`), which is one-sided-blind by construction:
// a price can only be `centre` below the centre, so once `K·scale`
// reaches the centre NO downward print — a 5× crash, a decimal-shift
// fat finger (on the served guard, even an exact 0) — can exceed the threshold, while the
// mirror-image up-move still is. The additive band goes blind below at
// a relative scale of 1/K: 16.9 % relative MAD for [FilterOutliers] at
// the default σ=4, 6.75 % for the served-VWAP guard at K=10 — dispersion
// an ordinary long-tail pair reaches routinely.
//
// The bands are now symmetric in RATIO space (ADR-0046 §1's direction symmetry: a ½× and a 2×
// deviation are equally outlying), so these assert the DOWNWARD half of
// each band as tightly as the upward half, and pin the upward half
// unchanged so the fix cannot degenerate into "reject more of
// everything".

// TestFilterOutliers_DownAndUpOutliersBothDropped is the window-filter
// half. The same dispersed window is contaminated once with a crash
// print and once with its mirror-image pump; both must be rejected and
// the served VWAP must be the clean window's, in both directions.
//
// Pre-fix the crash print survived (window relative MAD 25 % ⇒ additive
// lower edge −90.9) and dragged the served VWAP from 105 to 87.5.
func TestFilterOutliers_DownAndUpOutliersBothDropped(t *testing.T) {
	// Base window: a genuinely dispersed pair (~1.25× step between
	// prints, relative MAD 25 %). Base amount 1 ⇒ price = quote.
	base := []int64{64, 80, 100, 125, 156}
	const cleanVWAPNum = 64 + 80 + 100 + 125 + 156 // over 5 base units

	cases := []struct {
		name    string
		outlier int64
	}{
		{"crash_print_below", 1},
		{"pump_print_above", 10_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trades := make([]canonical.Trade, 0, len(base)+1)
			for _, p := range base {
				trades = append(trades, mkTrade(1, p))
			}
			trades = append(trades, mkTrade(1, tc.outlier))

			got := aggregate.FilterOutliers(trades, 4.0)
			if len(got) != len(base) {
				t.Fatalf("kept %d/%d trades, want %d — the %d print is as outlying as its mirror image",
					len(got), len(trades), len(base), tc.outlier)
			}
			for _, tr := range got {
				if tr.QuoteAmount.BigInt().Int64() == tc.outlier {
					t.Fatalf("outlier %d survived the filter", tc.outlier)
				}
			}
			vwap, err := aggregate.VWAP(got)
			if err != nil {
				t.Fatalf("VWAP: %v", err)
			}
			want := big.NewRat(cleanVWAPNum, int64(len(base)))
			if vwap.Cmp(want) != 0 {
				t.Errorf("served VWAP = %s, want %s — the %d print must not reach VWAP",
					vwap.RatString(), want.RatString(), tc.outlier)
			}
		})
	}
}

// TestFilterOutliers_SymmetricWindowFullyKept pins the other side: a
// window that is merely dispersed, with no contamination, keeps every
// print. The ratio-symmetric band must not start trimming the low tail
// of an honest window.
func TestFilterOutliers_SymmetricWindowFullyKept(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(1, 64), mkTrade(1, 80), mkTrade(1, 100),
		mkTrade(1, 125), mkTrade(1, 156),
	}
	got := aggregate.FilterOutliers(trades, 4.0)
	if len(got) != len(trades) {
		t.Fatalf("kept %d/%d — an honest dispersed window must survive intact", len(got), len(trades))
	}
}

// TestFilterOutliersLocal_CrashPrintDroppedOnDispersedWindow covers the
// PUBLISHED-VWAP path (orchestrator.go's FilterOutliersLocal). Its
// reference set folds the WINDOW reference in first and keeps the
// SMALLEST score across references, so the window's wide, unclamped band
// decides a print no local reference vouches for — and pre-fix that band
// had no lower edge at all on a dispersed window, so the crash print was
// published into the VWAP.
func TestFilterOutliersLocal_CrashPrintDroppedOnDispersedWindow(t *testing.T) {
	base := []int64{64, 80, 100, 125, 156}
	trades := make([]canonical.Trade, 0, len(base)+1)
	for _, p := range base {
		trades = append(trades, mkTrade(1, p))
	}
	trades = append(trades, mkTrade(1, 1)) // crash print

	got := aggregate.FilterOutliersLocal(trades, aggregate.LocalOutlierOptions{Sigma: 4.0})
	if len(got) != len(base) {
		t.Fatalf("kept %d/%d prints, want %d — the crash print must not reach the published VWAP",
			len(got), len(trades), len(base))
	}
	vwap, err := aggregate.VWAP(got)
	if err != nil {
		t.Fatalf("VWAP: %v", err)
	}
	if want := big.NewRat(64+80+100+125+156, int64(len(base))); vwap.Cmp(want) != 0 {
		t.Errorf("published VWAP = %s, want %s", vwap.RatString(), want.RatString())
	}
}

// TestGuardServedVWAP_CrashPrintRejectedOnVolatileBaseline is the
// serving-guard half (F039/RLT-391). A trailing baseline with 10 %
// relative MAD used to push the guard's additive lower edge to −48.26,
// from where EVERY non-negative candidate — 0 included — was served
// verbatim as a confident price, while the mirror-image pump was still
// caught.
func TestGuardServedVWAP_CrashPrintRejectedOnVolatileBaseline(t *testing.T) {
	// Newest-first; median 100, MAD 10 (10 % relative — above the 6.75 %
	// at which the old additive lower edge went non-positive).
	trailing := ratsFor(t, "120", "110", "100", "90", "80", "120", "110", "100", "90", "80")

	rejected := []string{"0", "0.000001", "20", "33"} // crash prints, below centre/3
	for _, cand := range rejected {
		accept, lkg := aggregate.GuardServedVWAP(mustRat(t, cand), trailing)
		if accept {
			t.Errorf("crash candidate %s was ACCEPTED — the guard has no lower bound", cand)
			continue
		}
		if lkg != 0 {
			t.Errorf("crash candidate %s: lkg=%d, want 0 (the newest clean trailing bucket)", cand, lkg)
		}
		if got := trailing[lkg].RatString(); got != "120" {
			t.Errorf("crash candidate %s: served last-known-good %s, want 120", cand, got)
		}
	}

	// The upward half of the band is UNCHANGED: a 2× move on a pair this
	// volatile is still served, a 4× pump is still rejected.
	if accept, _ := aggregate.GuardServedVWAP(mustRat(t, "200"), trailing); !accept {
		t.Error("2x move on a volatile baseline must still be served (band over-tightened)")
	}
	if accept, _ := aggregate.GuardServedVWAP(mustRat(t, "400"), trailing); accept {
		t.Error("4x pump must still be rejected")
	}
	// And a real downward move inside the band is still served.
	if accept, _ := aggregate.GuardServedVWAP(mustRat(t, "50"), trailing); !accept {
		t.Error("a 2x drop on a volatile baseline must still be served (band over-tightened)")
	}
}

// TestGuardServedVWAP_MADArmStillWidensDownward proves the fix did not
// collapse the MAD arm into the ratio arm: on a 15 %-relative-MAD
// baseline the band's lower edge is still BELOW centre/3 (the ratio
// bound) — the volatile pair still earns latitude from its own spread —
// yet it is finite and positive, so a crash print is rejected.
func TestGuardServedVWAP_MADArmStillWidensDownward(t *testing.T) {
	// Median 100, MAD 22.5 ⇒ K·1.4826·MAD = 333.59, upper edge 433.59,
	// ratio-symmetric lower edge 100²/433.59 = 23.06 — below the ratio
	// arm's 100/3, so the MAD arm is the binding lower edge.
	trailing := ratsFor(t, "130", "130", "130", "115", "100", "100", "100", "85", "70", "70")
	if accept, _ := aggregate.GuardServedVWAP(mustRat(t, "25"), trailing); !accept {
		t.Error("25 is inside the MAD arm's widened lower edge (23.06) — must be served")
	}
	if accept, lkg := aggregate.GuardServedVWAP(mustRat(t, "1"), trailing); accept || lkg < 0 {
		t.Errorf("a 100x crash print must be rejected with a last-known-good (accept=%v lkg=%d)", accept, lkg)
	}
}

// mustRat parses a decimal test fixture into an exact *big.Rat.
func mustRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("bad rat literal %q", s)
	}
	return r
}

// ratsFor builds a newest-first trailing baseline from decimal strings.
func ratsFor(t *testing.T, ss ...string) []*big.Rat {
	t.Helper()
	out := make([]*big.Rat, len(ss))
	for i, s := range ss {
		out[i] = mustRat(t, s)
	}
	return out
}

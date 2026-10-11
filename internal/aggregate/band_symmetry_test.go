package aggregate_test

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Direction-symmetry regressions for every robust band here (FilterOutliers, FilterOutliersLocal, GuardServedVWAP, the
// oracle-aggregator band): an additive band |p - centre| > K*scale is blind below the centre once K*scale reaches it.
// The bands are ratio-symmetric (ADR-0046 §1), so these pin the downward half as tightly as the upward half.

// TestFilterOutliers_DownAndUpOutliersBothDropped pins that a crash print and its mirror-image pump are both rejected, leaving the clean window's VWAP.
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
			vwap, err := aggregate.VWAPOf(got)
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

// TestFilterOutliers_SymmetricWindowFullyKept pins that a merely dispersed, uncontaminated window keeps every print.
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

// TestFilterOutliersLocal_CrashPrintDroppedOnDispersedWindow covers the published-VWAP path: an additive window band has no lower edge on a dispersed window, so the crash print must be dropped.
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
	vwap, err := aggregate.VWAPOf(got)
	if err != nil {
		t.Fatalf("VWAP: %v", err)
	}
	if want := big.NewRat(64+80+100+125+156, int64(len(base))); vwap.Cmp(want) != 0 {
		t.Errorf("published VWAP = %s, want %s", vwap.RatString(), want.RatString())
	}
}

// TestGuardServedVWAP_CrashPrintRejectedOnVolatileBaseline is the serving-guard half: a volatile baseline must not let every non-negative candidate, 0 included, be served verbatim.
func TestGuardServedVWAP_CrashPrintRejectedOnVolatileBaseline(t *testing.T) {
	// Newest-first; median 100, MAD 10 (10 % relative — above the 6.75 %
	// at which an additive lower edge goes non-positive).
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

// TestGuardServedVWAP_MADArmStillWidensDownward proves the MAD arm still widens below the ratio bound yet stays finite and positive, so a crash print is rejected.
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

// TestRejectAggregatorOutliers_SurvivorSetSymmetric is the oracle-aggregator half: a decimal-shifted quote and its ratio mirror are both dropped, leaving the five honest vendors.
func TestRejectAggregatorOutliers_SurvivorSetSymmetric(t *testing.T) {
	honest := []canonical.OracleUpdate{
		oracleRow("a", 7000), oracleRow("b", 8500), oracleRow("c", 10000),
		oracleRow("d", 11500), oracleRow("e", 13000),
	}

	cases := []struct {
		name    string
		outlier int64
	}{
		{"crash_quote_below", 100},
		{"pump_quote_above", 855_625},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := append(append([]canonical.OracleUpdate{}, honest...), oracleRow("bad", tc.outlier))

			kept := aggregate.RejectAggregatorOutliers(rows)
			got := make(map[string]bool, len(kept))
			for _, r := range kept {
				got[r.Source] = true
			}
			if got["bad"] {
				t.Errorf("survivors = %v — the %d quote is as outlying as its mirror image", got, tc.outlier)
			}
			for _, h := range honest {
				if !got[h.Source] {
					t.Errorf("survivors = %v — honest vendor %q was dropped", got, h.Source)
				}
			}
			if len(kept) != len(honest) {
				t.Errorf("kept %d rows, want exactly the %d honest vendors: %v", len(kept), len(honest), got)
			}
		})
	}
}

// oracleRow builds an aggregator quote at 2dp fixed-point.
func oracleRow(source string, priceScaled int64) canonical.OracleUpdate {
	return canonical.OracleUpdate{
		Source:    source,
		Timestamp: time.Now().UTC(),
		Price:     canonical.NewAmount(big.NewInt(priceScaled)),
		Decimals:  2,
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

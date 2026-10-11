package diagnostics

import (
	"math/big"
	"strconv"
	"strings"
	"testing"
)

// TestDiffLedgerCounts_AllAgree: identical counts on both sides return zero diffs.
func TestDiffLedgerCounts_AllAgree(t *testing.T) {
	ours := map[uint32]int{100: 5, 101: 2, 102: 0}
	theirs := map[uint32]int{100: 5, 101: 2, 102: 0}
	if got := diffLedgerCounts(ours, theirs); len(got) != 0 {
		t.Errorf("expected zero diffs on identical maps, got %d: %+v", len(got), got)
	}
}

// TestDiffLedgerCounts_Missing is the common alert path: rows missing on a ledger Hubble has, i.e. a decoder coverage gap.
func TestDiffLedgerCounts_Missing(t *testing.T) {
	ours := map[uint32]int{100: 5}
	theirs := map[uint32]int{100: 7}
	got := diffLedgerCounts(ours, theirs)
	if len(got) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(got))
	}
	if got[0].Ours != 5 || got[0].Theirs != 7 {
		t.Errorf("got %+v, want {100, 5, 7}", got[0])
	}
}

// TestDiffLedgerCounts_Extra: emitting MORE rows than Hubble means non-trade events decoded or double-counting; either way a bug.
func TestDiffLedgerCounts_Extra(t *testing.T) {
	ours := map[uint32]int{200: 9}
	theirs := map[uint32]int{200: 4}
	got := diffLedgerCounts(ours, theirs)
	if len(got) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(got))
	}
	if got[0].Ours != 9 || got[0].Theirs != 4 {
		t.Errorf("got %+v, want {200, 9, 4}", got[0])
	}
}

// TestDiffLedgerCounts_AbsenceIsZero: a ledger absent on one side counts as 0 there, not skipped, so Hubble 3 vs us 0 (a decoder skipping a whole shape) is reported.
func TestDiffLedgerCounts_AbsenceIsZero(t *testing.T) {
	ours := map[uint32]int{}
	theirs := map[uint32]int{500: 3}
	got := diffLedgerCounts(ours, theirs)
	if len(got) != 1 {
		t.Fatalf("expected 1 diff (absence ≡ zero on missing key), got %d", len(got))
	}
	if got[0].Ledger != 500 || got[0].Ours != 0 || got[0].Theirs != 3 {
		t.Errorf("got %+v, want {500, 0, 3}", got[0])
	}

	// And the symmetric case: we have rows for a ledger Hubble doesn't.
	got = diffLedgerCounts(map[uint32]int{600: 2}, map[uint32]int{})
	if len(got) != 1 || got[0].Ledger != 600 || got[0].Ours != 2 || got[0].Theirs != 0 {
		t.Errorf("symmetric absence: got %+v, want {600, 2, 0}", got)
	}
}

// TestDiffLedgerCounts_SortedByLedger locks ascending ledger order, so reports diff across runs despite map iteration order.
func TestDiffLedgerCounts_SortedByLedger(t *testing.T) {
	ours := map[uint32]int{300: 1, 100: 1, 200: 1}
	theirs := map[uint32]int{300: 9, 100: 9, 200: 9}
	got := diffLedgerCounts(ours, theirs)
	if len(got) != 3 {
		t.Fatalf("expected 3 diffs, got %d", len(got))
	}
	for i, want := range []uint32{100, 200, 300} {
		if got[i].Ledger != want {
			t.Errorf("position %d: got ledger=%d, want %d (output must be sorted ascending)",
				i, got[i].Ledger, want)
		}
	}
}

// TestDiffLedgerCounts_BothEmpty: no data either side yields no divergence; hubbleCountVerdict refuses to call an empty comparison a pass.
func TestDiffLedgerCounts_BothEmpty(t *testing.T) {
	if got := diffLedgerCounts(map[uint32]int{}, map[uint32]int{}); len(got) != 0 {
		t.Errorf("empty inputs should yield empty output, got %+v", got)
	}
	if got := diffLedgerCounts(nil, nil); len(got) != 0 {
		t.Errorf("nil inputs should yield empty output, got %+v", got)
	}
}

// TestHubbleCheck_FlagValidation exercises the argv guards, which fail loudly before config load or BigQuery.
func TestHubbleCheck_FlagValidation(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantSubstr string
	}{
		{"missing-config", []string{"-from", "100", "-to", "200", "-bigquery-project", "p"}, "-config required"},
		{"missing-from", []string{"-config", "/dev/null", "-to", "200", "-bigquery-project", "p"}, "-from must be > 0"},
		{"to-equals-from", []string{"-config", "/dev/null", "-from", "100", "-to", "100", "-bigquery-project", "p"}, "must be > -from"},
		{"missing-project", []string{"-config", "/dev/null", "-from", "100", "-to", "200"}, "-bigquery-project required"},
		{"uncapped-bytes", []string{"-config", "/dev/null", "-from", "100", "-to", "200", "-bigquery-project", "p", "-max-bytes-billed", "-1"}, "-max-bytes-billed must be > 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := hubbleCheck(tc.args)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubstr)
			}
			if !contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ─── -with-amounts variant ────────────────────────────────────────

// statsOf is a tiny constructor so the table-driven tests below
// don't drown in big.Int boilerplate.
func statsOf(count int, sumSell, sumBuy int64) ledgerStats {
	return ledgerStats{
		Count:   count,
		SumSell: big.NewInt(sumSell),
		SumBuy:  big.NewInt(sumBuy),
	}
}

// TestDiffLedgerStats_AllAgree: identical stats yield zero diffs, with no false positives from the big.Int comparison.
func TestDiffLedgerStats_AllAgree(t *testing.T) {
	ours := map[uint32]ledgerStats{
		100: statsOf(5, 1_000_000, 12_000_000),
		101: statsOf(2, 500_000, 6_000_000),
	}
	theirs := map[uint32]ledgerStats{
		100: statsOf(5, 1_000_000, 12_000_000),
		101: statsOf(2, 500_000, 6_000_000),
	}
	if got := diffLedgerStats(ours, theirs, 0); len(got) != 0 {
		t.Errorf("expected zero diffs on identical stats, got %d", len(got))
	}
}

// TestDiffLedgerStats_AmountMismatchAtSameCount: counts agree but an amount diverged, which only the stats check (SumSell) catches.
func TestDiffLedgerStats_AmountMismatchAtSameCount(t *testing.T) {
	ours := map[uint32]ledgerStats{200: statsOf(3, 1_000_000, 12_000_000)}
	theirs := map[uint32]ledgerStats{200: statsOf(3, 1_000_001, 12_000_000)}
	got := diffLedgerStats(ours, theirs, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(got))
	}
	if got[0].Reasons[0] != "sum_sell" {
		t.Errorf("expected reason=sum_sell, got %v", got[0].Reasons)
	}
}

// TestDiffLedgerStats_ToleranceBps: a small drift passes with -tolerance-bps > 0 and fails at strict zero.
func TestDiffLedgerStats_ToleranceBps(t *testing.T) {
	// 1 bp ≈ 0.01% — at scale 1_000_000_000 that's a 100k allowance.
	ours := map[uint32]ledgerStats{300: statsOf(1, 1_000_000_000, 1_000_000_000)}
	theirs := map[uint32]ledgerStats{300: statsOf(1, 1_000_050_000, 1_000_000_000)}
	// 50_000 / 1_000_050_000 ≈ 0.5 bps — well within 1 bp tolerance.
	if got := diffLedgerStats(ours, theirs, 1); len(got) != 0 {
		t.Errorf("expected tolerance=1bps to absorb 0.5bp drift, got %d diffs", len(got))
	}
	// At strict zero, the same delta is divergence.
	if got := diffLedgerStats(ours, theirs, 0); len(got) != 1 {
		t.Errorf("expected tolerance=0 to flag any delta, got %d diffs", len(got))
	}
}

// TestSumsWithinTolerance_StrictAndLoose unit-tests the comparator: equal, off-by-one and the tolerance/larger ratio threshold.
func TestSumsWithinTolerance_StrictAndLoose(t *testing.T) {
	mustHold := func(a, b int64, bps int, want bool) {
		t.Helper()
		got := sumsWithinTolerance(big.NewInt(a), big.NewInt(b), bps)
		if got != want {
			t.Errorf("sumsWithinTolerance(%d, %d, %dbps) = %v, want %v", a, b, bps, got, want)
		}
	}
	mustHold(100, 100, 0, true)              // equal, strict
	mustHold(100, 101, 0, false)             // off-by-one, strict
	mustHold(100, 101, 100, true)            // 100bps = 1%, off-by-one (1%) absorbed
	mustHold(1_000_000, 1_000_001, 1, true)  // 0.0001% within 1 bp
	mustHold(1_000_000, 1_001_000, 1, false) // 0.1% exceeds 1 bp
}

// TestDiffLedgerStats_OneSidedAbsenceFlagsBothCountAndSums: absence is zero, so a ledger with data on one side reports count and both sum diffs (a complete decoder failure looks like this).
func TestDiffLedgerStats_OneSidedAbsenceFlagsBothCountAndSums(t *testing.T) {
	ours := map[uint32]ledgerStats{}
	theirs := map[uint32]ledgerStats{500: statsOf(3, 1_000_000, 5_000_000)}
	got := diffLedgerStats(ours, theirs, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(got))
	}
	wantReasons := []string{"count", "sum_sell", "sum_buy"}
	if len(got[0].Reasons) != 3 {
		t.Errorf("expected 3 reasons, got %v", got[0].Reasons)
	}
	for i, want := range wantReasons {
		if got[0].Reasons[i] != want {
			t.Errorf("reason %d: got %q, want %q", i, got[0].Reasons[i], want)
		}
	}
}

// TestHubbleStatsSQL_ScalesSumsToStroops pins that Hubble's amounts are whole-unit FLOAT64 while ours are stroops; the query must ROUND(SUM(...) * hubbleStroopsPerUnit) before casting to STRING so both sides share an integer stroop scale and big.Int.SetString does not abort.
func TestHubbleStatsSQL_ScalesSumsToStroops(t *testing.T) {
	sql := hubbleStatsSQL()

	wantSell := "ROUND(SUM(selling_amount) * " + hubbleStroopsPerUnitLiteral + ")"
	if !strings.Contains(sql, wantSell) {
		t.Errorf("sum_sell not scaled to stroops: query missing %q\ngot: %s", wantSell, sql)
	}
	wantBuy := "ROUND(SUM(buying_amount) * " + hubbleStroopsPerUnitLiteral + ")"
	if !strings.Contains(sql, wantBuy) {
		t.Errorf("sum_buy not scaled to stroops: query missing %q\ngot: %s", wantBuy, sql)
	}

	// The literal embedded in the SQL text must actually equal the Go-side
	// scaling constant, or the two could drift independently.
	gotLiteral, err := strconv.Atoi(hubbleStroopsPerUnitLiteral)
	if err != nil {
		t.Fatalf("hubbleStroopsPerUnitLiteral %q is not an integer: %v", hubbleStroopsPerUnitLiteral, err)
	}
	if gotLiteral != hubbleStroopsPerUnit {
		t.Errorf("hubbleStroopsPerUnitLiteral = %d, want %d (hubbleStroopsPerUnit)", gotLiteral, hubbleStroopsPerUnit)
	}

	// An unscaled whole-unit sum like "12.3456789" is not a valid base-10 integer, so scaling must be SQL-side; the scaled, rounded stroop value is valid.
	if _, ok := new(big.Int).SetString("12.3456789", 10); ok {
		t.Fatalf("test assumption broken: unscaled decimal string unexpectedly parsed as big.Int")
	}
	// Multiplying the same whole-unit value by hubbleStroopsPerUnit and
	// rounding — exactly what the fixed query does in SQL — yields the
	// integral stroop string fetchHubbleStats can actually parse.
	if _, ok := new(big.Int).SetString("123456789", 10); !ok {
		t.Fatalf("scaled stroop value should parse as big.Int")
	}
}

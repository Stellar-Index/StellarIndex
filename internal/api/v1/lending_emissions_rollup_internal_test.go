package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The emissions and revenue workers take distinct advisory locks: sharing one would let either starve the other.
func TestLendingEmissionsRollup_LockDiffersFromRevenue(t *testing.T) {
	s, store := New(Options{}), (*timescale.Store)(nil)
	if e, r := s.NewLendingEmissionsRollup(store, nil).rollup().src.lockName, s.NewLendingRevenueRollup(store, nil).rollup().src.lockName; e == r || e == "" {
		t.Fatalf("emissions lock %q, revenue lock %q; want two distinct names", e, r)
	}
}

// A sweep difference is a mismatch only when the same lines carry the same tokens at the same revision, neither side was
// refused, and the USD differs: other tokens are a rewritten source, a refusal is a verdict.
func TestEmisDayRowsUnexplained(t *testing.T) {
	usd := func(s string) *string { return &s }
	row := func(amount string, amountUSD, reason *string) []timescale.LendingEmissionsUSDDay {
		return []timescale.LendingEmissionsUSDDay{{
			Pool: "P", Day: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Stream: emisRowPoolClaims, Amount: amount,
			AmountUSD: amountUSD, USDReason: reason, BasisHours: map[string]int{}, Excluded: []string{},
			ValuationRev: lendingEmissionsValuationRev,
		}}
	}
	withheld := exclusionPriceWithheld
	for _, tc := range []struct {
		name     string
		old, new []timescale.LendingEmissionsUSDDay
		want     bool
	}{
		{"other usd, same tokens", row("10", usd("1"), nil), row("10", usd("2"), nil), true},
		{"other tokens", row("10", usd("1"), nil), row("11", usd("1.1"), nil), false},
		{"refused before", row("10", nil, &withheld), row("10", usd("1"), nil), false},
		{"same", row("10", usd("1"), nil), row("10", usd("1"), nil), false},
		{"a line left", row("10", usd("1"), nil), nil, false},
	} {
		if got := emisDayRowsUnexplained(tc.old, tc.new); got != tc.want {
			t.Errorf("%s: unexplained = %v, want %v", tc.name, got, tc.want)
		}
	}
	a, b := row("10", usd("1"), nil), row("10", usd("1"), nil)
	b[0].ValuedAt = time.Now()
	if !emisDayRowsEqual(a, b) {
		t.Error("rows differing only in valued_at are not equal")
	}
}

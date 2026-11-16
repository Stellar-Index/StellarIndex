package v1

import (
	"math/big"
	"testing"
)

func bigOf(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad int %q", s)
	}
	return n
}

// doubleValue values a token at two units of the asset.
func doubleValue(n *big.Int) (*big.Int, bool) { return new(big.Int).Lsh(n, 1), true }

// TestReconcileLeg covers the earnings invariant: accrued only when the folded balance equals the on-chain one.
func TestReconcileLeg(t *testing.T) {
	const above63 = "18446744073709551616" // 2^64
	cases := []struct {
		name                   string
		in                     legInputs
		wantAccrued, wantValue string // "" = null
		wantReason             string
	}{
		{
			"equal balances are exact above 2^63",
			legInputs{netFlow: bigOf(t, "30000000000000000000"), folded: bigOf(t, above63), onchain: bigOf(t, above63), value: doubleValue},
			"6893488147419103232", "36893488147419103232", "",
		},
		{
			"off by one base unit is withheld",
			legInputs{netFlow: bigOf(t, "10"), folded: bigOf(t, "100"), onchain: bigOf(t, "101"), value: doubleValue},
			"", "202", earningsReasonBalanceMismatch,
		},
		{
			"fully exited account keeps realised interest",
			legInputs{netFlow: bigOf(t, "-42"), folded: new(big.Int), onchain: new(big.Int), value: func(*big.Int) (*big.Int, bool) { return nil, false }},
			"42", "0", "",
		},
		{
			"auction-moved leg is withheld even when balances agree",
			legInputs{netFlow: bigOf(t, "10"), folded: bigOf(t, "5"), onchain: bigOf(t, "5"), moved: true, value: doubleValue},
			"", "10", earningsReasonAuctionMoved,
		},
		{
			"unread on-chain balance is withheld",
			legInputs{netFlow: bigOf(t, "10"), folded: bigOf(t, "5"), value: doubleValue},
			"", "", earningsReasonOnchainUnread,
		},
		{
			"inconsistent fold is withheld",
			legInputs{netFlow: bigOf(t, "10"), onchain: bigOf(t, "5"), value: doubleValue},
			"", "10", earningsReasonFoldInconsistent,
		},
		{
			"unread rate is withheld",
			legInputs{netFlow: bigOf(t, "10"), folded: bigOf(t, "5"), onchain: bigOf(t, "5"), value: func(*big.Int) (*big.Int, bool) { return nil, false }},
			"", "", earningsReasonRateUnread,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var leg LendingEarningsLeg
			reconcileLeg(&leg, c.in)
			if got := deref(leg.Accrued); got != c.wantAccrued {
				t.Errorf("accrued = %q, want %q", got, c.wantAccrued)
			}
			if got := deref(leg.CurrentValue); got != c.wantValue {
				t.Errorf("current_value = %q, want %q", got, c.wantValue)
			}
			if got := deref(leg.Reason); got != c.wantReason || leg.Reconciled != (c.wantReason == "") {
				t.Errorf("reason = %q reconciled = %v, want %q", got, leg.Reconciled, c.wantReason)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

package supply

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestValidateGenesisLedgerBoundary pins the -genesis-ledger validation that
// keeps the seed's disjoint-partition invariant SOUND (review Finding 2). The
// CH genesis sum covers `ledger < boundary` and the PG Soroban-era total covers
// `ledger >= clickhouse.SorobanGenesisLedger`; these are only non-overlapping —
// and so the seeded baseline only correct — when the boundary is at-or-below
// the true Soroban genesis. A boundary ABOVE it would sum Soroban-era flows into
// BOTH slices → double-count → inflated served supply, so it must be rejected.
//
// The default (clickhouse.SorobanGenesisLedger) must keep working: the check is
// strictly-greater-than, so boundary == genesis passes.
func TestValidateGenesisLedgerBoundary(t *testing.T) {
	genesis := uint(clickhouse.SorobanGenesisLedger)
	cases := []struct {
		name    string
		ledger  uint
		wantErr bool
	}{
		{"zero-rejected", 0, true},
		{"one-accepted", 1, false},
		{"below-genesis-accepted", genesis - 1, false},
		{"at-genesis-accepted-default", genesis, false},
		{"one-above-genesis-rejected", genesis + 1, true},
		{"far-above-genesis-rejected", genesis + 10_000_000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGenesisLedgerBoundary(tc.ledger)
			if tc.wantErr && err == nil {
				t.Errorf("validateGenesisLedgerBoundary(%d) = nil, want error", tc.ledger)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateGenesisLedgerBoundary(%d) = %v, want nil", tc.ledger, err)
			}
		})
	}
}

package supply

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestValidateGenesisLedgerBoundary: a -genesis-ledger above the true Soroban genesis double-counts flows in the CH and PG slices, so it must be rejected; equal passes.
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

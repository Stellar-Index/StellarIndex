package main

import (
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

func TestWrittenTotal(t *testing.T) {
	boom := errors.New("db down")
	cases := []struct {
		name      string
		snap      supply.Supply
		err       error
		wantTotal *big.Int
		wantOK    bool
		wantErr   error
	}{
		{"not found is absent, not an error", supply.Supply{}, timescale.ErrNotFound, nil, false, nil},
		{"wrapped not found is absent", supply.Supply{}, fmt.Errorf("lookup: %w", timescale.ErrNotFound), nil, false, nil},
		{"found row returns its total", supply.Supply{TotalSupply: big.NewInt(1_234_567)}, nil, big.NewInt(1_234_567), true, nil},
		{"row without a total is absent", supply.Supply{}, nil, nil, false, nil},
		{"other errors propagate", supply.Supply{}, boom, nil, false, boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, ok, err := writtenTotal(tc.snap, tc.err)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("err=%v, want %v", err, tc.wantErr)
			}
			if ok != tc.wantOK {
				t.Errorf("ok=%v, want %v", ok, tc.wantOK)
			}
			if (total == nil) != (tc.wantTotal == nil) || (total != nil && total.Cmp(tc.wantTotal) != 0) {
				t.Errorf("total=%v, want %v", total, tc.wantTotal)
			}
		})
	}
}

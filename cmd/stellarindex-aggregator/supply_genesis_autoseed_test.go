package main

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Every kind is compared: equal nets with different mints and burns are still a hole in the served events.
func TestSEP41TotalsMatch(t *testing.T) {
	lake := clickhouse.TokenSupply{Mint: big.NewInt(900), Burn: big.NewInt(100), Clawback: big.NewInt(0)}
	for _, tc := range []struct {
		name   string
		served timescale.SEP41KindTotals
		ok     bool
	}{
		{"equal", timescale.SEP41KindTotals{Mint: big.NewInt(900), Burn: big.NewInt(100), Clawback: big.NewInt(0)}, true},
		{"nothing replayed", timescale.SEP41KindTotals{Mint: big.NewInt(0), Burn: big.NewInt(0), Clawback: big.NewInt(0)}, false},
		{"same net, missing pair", timescale.SEP41KindTotals{Mint: big.NewInt(850), Burn: big.NewInt(50), Clawback: big.NewInt(0)}, false},
		{"clawback differs", timescale.SEP41KindTotals{Mint: big.NewInt(900), Burn: big.NewInt(100), Clawback: big.NewInt(1)}, false},
	} {
		if err := sep41TotalsMatch(lake, tc.served, 50457424, 60000000); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

package lendinginterest

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/blendtest"
)

// The revenue route serves interest_accrued = supplier_share + backstop_take + protocol_fee per bucket; sums keep an
// identity that holds per row, so it is pinned here, on rows the writer folds from mainnet versions.
func TestFold_GrossIsTheSplitOnMainnetRows(t *testing.T) {
	byTx := map[string][]blendtest.Default{}
	for _, d := range blendtest.DefaultedDebt {
		byTx[d.Tx] = append(byTx[d.Tx], d)
	}
	accrued := 0
	for _, ds := range byTx {
		for _, r := range defaultFold(t, ds, nil, true) {
			split := new(big.Int).Add(new(big.Int).Add(r.SupplierIntended, r.Backstop), r.ProtocolFee)
			if split.Cmp(r.Gross) != 0 {
				t.Errorf("%s %s %v: gross %s, split %s", r.Pool, r.Asset, r.Hour, r.Gross, split)
			}
			if r.Gross.Sign() > 0 && r.Backstop.Sign() > 0 {
				accrued++
			}
		}
	}
	if accrued == 0 {
		t.Fatal("no folded row accrued interest with a backstop take: the identity was not exercised")
	}
}

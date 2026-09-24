// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package sdexclaim

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestBoughtSide pins the leg BoughtSide returns for every known variant:
// AssetBought/AmountBought (what the taker paid), never the sold leg.
func TestBoughtSide(t *testing.T) {
	sold := xdr.MustNewNativeAsset()
	bought := xdr.MustNewCreditAsset("USD", "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H")
	const soldAmt, boughtAmt = xdr.Int64(111), xdr.Int64(222)

	cases := map[string]xdr.ClaimAtom{
		"order book": {
			Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
			OrderBook: &xdr.ClaimOfferAtom{
				AssetSold: sold, AmountSold: soldAmt, AssetBought: bought, AmountBought: boughtAmt,
			},
		},
		"liquidity pool": {
			Type: xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool,
			LiquidityPool: &xdr.ClaimLiquidityAtom{
				AssetSold: sold, AmountSold: soldAmt, AssetBought: bought, AmountBought: boughtAmt,
			},
		},
		"v0": {
			Type: xdr.ClaimAtomTypeClaimAtomTypeV0,
			V0: &xdr.ClaimOfferAtomV0{
				AssetSold: sold, AmountSold: soldAmt, AssetBought: bought, AmountBought: boughtAmt,
			},
		},
	}
	for name, atom := range cases {
		asset, amount, known := BoughtSide(atom)
		if !known || !asset.Equals(bought) || amount != boughtAmt {
			t.Errorf("%s: BoughtSide = (%v, %d, %v), want (%v, %d, true)",
				name, asset, amount, known, bought, boughtAmt)
		}
	}

	if _, amount, known := BoughtSide(xdr.ClaimAtom{Type: xdr.ClaimAtomType(99)}); known || amount != 0 {
		t.Errorf("unknown variant: BoughtSide = (_, %d, %v), want (_, 0, false)", amount, known)
	}
}

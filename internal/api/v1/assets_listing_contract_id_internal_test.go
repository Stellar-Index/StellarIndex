// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Wallets key holdings by contract address, so a listing row must carry the same C-address the
// asset's detail page does: the derived SAC for classic and native, the token itself for Soroban.
func TestAssetDetailFromAssetRow_CarriesContractID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"native",
		"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		"CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J",
	} {
		a, err := canonical.ParseAsset(id)
		if err != nil {
			t.Fatalf("ParseAsset(%q): %v", id, err)
		}
		want := detailFromAsset(a).ContractID
		if want == nil {
			t.Fatalf("%s: detail has no contract_id", id)
		}
		got := assetDetailFromAssetRow(timescale.AssetRow{AssetID: id}).ContractID
		if got == nil || *got != *want {
			t.Errorf("%s: listing contract_id = %v, want %s", id, got, *want)
		}
	}
	if got := assetDetailFromAssetRow(timescale.AssetRow{AssetID: "not-an-asset"}).ContractID; got != nil {
		t.Errorf("unparseable id: contract_id = %s, want none", *got)
	}
}

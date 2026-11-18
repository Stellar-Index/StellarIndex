// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// An unpriced Soroban listing row with no supply reading takes every early return in the
// market-cap fill; its scale must still be read or flagged, never served as the stamped 7.
func TestFillMarketCaps_UnpricedSorobanRowWithoutSupplyOmitsDecimals(t *testing.T) {
	const contractID = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	for _, tc := range []struct {
		name           string
		stub           *listingLockstepDecStub
		wantDecimals   bool
		wantUndeclared bool
	}{
		{name: "undeclared", stub: &listingLockstepDecStub{found: false}, wantUndeclared: true},
		{name: "declared 18", stub: &listingLockstepDecStub{d: 18, found: true}, wantDecimals: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{TokenDecimals: tc.stub}}
			rows := []AssetDetail{{
				AssetID:  contractID,
				Type:     string(canonical.AssetSoroban),
				Code:     "TKN",
				Decimals: 7, // assetDetailFromAssetRow's placeholder
			}}
			s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

			if rows[0].DecimalsUndeclared != tc.wantUndeclared {
				t.Errorf("DecimalsUndeclared = %v, want %v", rows[0].DecimalsUndeclared, tc.wantUndeclared)
			}
			b, err := json.Marshal(rows[0])
			if err != nil {
				t.Fatal(err)
			}
			has := strings.Contains(string(b), `"decimals":`)
			if has != tc.wantDecimals {
				t.Errorf("decimals on the wire = %v, want %v: %s", has, tc.wantDecimals, b)
			}
			if tc.wantDecimals && !strings.Contains(string(b), `"decimals":18`) {
				t.Errorf("want the read scale 18: %s", b)
			}
		})
	}
}

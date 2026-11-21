// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// An unverified classic asset's SAC row is 7 decimals by protocol: derived from the classic row on the page, it needs no lake
// read, so an unwired reader neither flags it unresolved nor withholds its price.
func TestListingScales_UnverifiedSACOfPageClassicIsSevenWithoutRead(t *testing.T) {
	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	price := "0.05"
	rows := func() []AssetDetail {
		p := price
		return []AssetDetail{
			{AssetID: cetesAsset, Type: string(canonical.AssetClassic), Decimals: 7},
			{AssetID: sac, Type: string(canonical.AssetSoroban), Decimals: 7, PriceUSD: &p},
		}
	}

	t.Run("reader nil", func(t *testing.T) {
		s := &Server{logger: discardLogger()}
		got := rows()
		if unread := s.fillMarketCapsFromSupply(context.Background(), got, map[string]int{}); unread {
			t.Error("page reported unread for a SAC whose scale is fixed by the protocol")
		}
		if got[1].Decimals != 7 || got[1].scaleUnknown() || got[1].DecimalsUnresolved {
			t.Errorf("SAC row: decimals=%d unknown=%v unresolved=%v, want 7 known", got[1].Decimals, got[1].scaleUnknown(), got[1].DecimalsUnresolved)
		}
		if got[1].PriceUSD == nil || *got[1].PriceUSD != price {
			t.Errorf("SAC row price = %v, want %s", got[1].PriceUSD, price)
		}
	})

	t.Run("batch reader never asked", func(t *testing.T) {
		stub := &countingScaleReader{declared: map[string]uint32{}}
		s := &Server{Options: Options{TokenDecimals: stub}, logger: discardLogger()}
		got := rows()
		s.fillMarketCapsFromSupply(context.Background(), got, map[string]int{})
		if n := stub.batchIDs.Load() + stub.singles.Load(); n != 0 {
			t.Errorf("SAC scale read %d times, want 0", n)
		}
		if got[1].DecimalsUndeclared || got[1].Decimals != 7 {
			t.Errorf("SAC row: decimals=%d undeclared=%v, want 7", got[1].Decimals, got[1].DecimalsUndeclared)
		}
	})
}

// XLM's SAC is 7 by protocol under every reader state.
func TestLakeTokenDecimals_XLMSACIsSevenWithoutReader(t *testing.T) {
	s := &Server{logger: discardLogger()}
	d, known, err := s.lakeTokenDecimals(context.Background(), canonical.NativeSACContractID())
	if d != 7 || !known || err != nil {
		t.Errorf("XLM SAC = (%d, %v, %v), want (7, true, nil)", d, known, err)
	}
}

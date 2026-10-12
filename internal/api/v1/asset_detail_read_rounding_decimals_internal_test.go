// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The per-asset and price-history readers round a confirmed non-7-decimals token's raw ratio to 10+k places (the corrected
// price rounded to 10). The detail page must not push those through the flat 10-place floor, or an 18-decimals $14 token stays
// withheld on /v1/assets/{id} while the listing serves it.
func TestAssetDetail_ReadsRoundedAfterTheCorrectionAreNotFloored(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	token, err := canonical.ParseAsset(sorobanContract)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Options: Options{NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{sorobanContract: 18})}}

	// 21 fraction places: ROUND(raw, 10 + 11)::text, as getAssetBySlugSQL
	// and the history SQL now render it.
	t.Run("row price", func(t *testing.T) {
		raw := "0.000000000140000000000"
		detail := &AssetDetail{AssetID: sorobanContract}
		row := timescale.AssetRow{AssetID: sorobanContract, PriceUSD: &raw}
		s.applyAssetRowToDetail(context.Background(), detail, token, row, nil, sorobanContract)
		if detail.PriceUSD == nil {
			t.Fatal("price_usd = nil, want 14.0000000000")
		}
		if *detail.PriceUSD != "14.0000000000" {
			t.Errorf("price_usd = %s, want 14.0000000000", *detail.PriceUSD)
		}
		if detail.PriceBasis == priceBasisTransitive {
			t.Error("price came from the transitive fallback, not from the row")
		}
	})

	t.Run("history points", func(t *testing.T) {
		after, before := "0.000000000140000000000", "0.0000000001"
		out := s.normalizedAssetPointsToWire([]timescale.AssetPricePoint{
			{T: "2026-09-17T00:00:00Z", P: &after},
			{T: "2026-09-17T01:00:00Z", P: &before},
		}, token)
		if out[0].P == nil || *out[0].P != "14.0000000000" {
			t.Errorf("point rounded after the correction = %v, want 14.0000000000", out[0].P)
		}
		// Rounded on the raw scale (a reader or cache entry from before
		// the SQL change): the floor still applies, and 14 USD is never
		// published as exactly 10.
		if out[1].P != nil {
			t.Errorf("point rounded on the raw scale = %s, want it withheld", *out[1].P)
		}
	})
}

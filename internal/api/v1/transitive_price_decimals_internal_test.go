// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rawSurfaceDecimalsReader feeds a fixed `nonstandard_decimals_assets`
// snapshot to the cache under test.
type rawSurfaceDecimalsReader struct {
	rows []timescale.NonstandardDecimalsAsset
}

func (r rawSurfaceDecimalsReader) LoadNonstandardDecimalsAssets(context.Context) ([]timescale.NonstandardDecimalsAsset, error) {
	return r.rows, nil
}

// decimalsCacheFlagging returns a refreshed cache holding one confirmed
// non-7-decimals row per (asset id → decimals) entry.
func decimalsCacheFlagging(t *testing.T, flagged map[string]int) *NonstandardDecimalsCache {
	t.Helper()
	var rows []timescale.NonstandardDecimalsAsset
	for id, dec := range flagged {
		rows = append(rows, timescale.NonstandardDecimalsAsset{Asset: id, Decimals: dec, Source: "aquarius"})
	}
	c := NewNonstandardDecimalsCache(rawSurfaceDecimalsReader{rows: rows}, nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("decimals cache refresh: %v", err)
	}
	return c
}

// The transitive fill is a product of RAW prices_1m ratios, and it exists
// for Soroban-native contracts — the only asset class that can carry a
// non-7 decimals(). It used to publish that product verbatim while every
// sibling price surface normalised, so the one price a 9-decimals token
// could get here read 100x low.
//
// The chain telescopes: whatever the hop's decimals are, they cancel, and
// the product is off by 10^(asset decimals − 7). The "hop flagged, asset
// not" row pins that — a correction keyed on the (asset, hop) leg alone
// would rescale a price that was already right.
func TestTransitivePriceFor_NormalisesNonstandardDecimals(t *testing.T) {
	const (
		// The runbook's confirmed 9-decimals contract.
		flaggedID = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
		plainID   = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
		hopID     = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
	)
	tests := []struct {
		name    string
		assetID string
		flagged map[string]int
		raw     string
		want    string
	}{
		{
			name:    "9dp asset — raw product scaled up by 10^2",
			assetID: flaggedID,
			flagged: map[string]int{flaggedID: 9},
			raw:     "41.32",
			want:    "4132.0000000000",
		},
		{
			name:    "6dp asset — raw product scaled down by 10^1",
			assetID: flaggedID,
			flagged: map[string]int{flaggedID: 6},
			raw:     "41.32",
			want:    "4.1320000000",
		},
		{
			name:    "7dp asset beside a flagged table — byte-identical",
			assetID: plainID,
			flagged: map[string]int{flaggedID: 9},
			raw:     "7934.40",
			want:    "7934.40",
		},
		{
			name:    "flagged HOP, 7dp asset — the hop's decimals cancel, byte-identical",
			assetID: plainID,
			flagged: map[string]int{hopID: 9},
			raw:     "7934.40",
			want:    "7934.40",
		},
		{
			name:    "flagged asset AND flagged hop — only the asset's decimals survive",
			assetID: flaggedID,
			flagged: map[string]int{flaggedID: 9, hopID: 18},
			raw:     "41.32",
			want:    "4132.0000000000",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			asset, err := canonical.ParseAsset(tc.assetID)
			if err != nil {
				t.Fatalf("parse asset: %v", err)
			}
			s := &Server{
				transitive: &stubPricer{tp: timescale.TransitivePrice{PriceUSD: tc.raw, Hop: hopID}, ok: true},
				substance: &stubListingGate{allow: map[string]bool{
					tc.assetID + "|" + hopID: true,
					hopID + "|native":        true,
				}},
				nonstandardDecimals: decimalsCacheFlagging(t, tc.flagged),
			}
			got, ok := s.transitivePriceFor(context.Background(), asset, tc.assetID)
			if !ok {
				t.Fatal("expected a served price")
			}
			if got != tc.want {
				t.Errorf("transitive price = %q, want %q", got, tc.want)
			}

			// And through the production entry point: the no-catalogue-row
			// arm of applyAssetRowToDetail, which is where a Soroban-native
			// contract's price_usd actually comes from.
			var detail AssetDetail
			s.applyAssetRowToDetail(context.Background(), &detail, asset, timescale.AssetRow{}, sql.ErrNoRows, tc.assetID)
			if detail.PriceUSD == nil {
				t.Fatal("price_usd not filled")
			}
			if *detail.PriceUSD != tc.want {
				t.Errorf("price_usd = %q, want %q", *detail.PriceUSD, tc.want)
			}
		})
	}
}

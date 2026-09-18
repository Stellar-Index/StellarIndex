// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// TestAssetPriceSnapshot_DecimalsNormalisedOnceAtTheWriter pins WHERE the
// dex-nonstandard-decimals correction lives for the listing price: in
// the rollup's writer, exactly once, and nowhere on the read side of the
// same column. A reader that scaled aps.price_usd again would publish a
// price off by the factor squared, and nothing else would notice — the
// executing proof of the values is
// test/integration/asset_price_snapshot_decimals_test.go.
func TestAssetPriceSnapshot_DecimalsNormalisedOnceAtTheWriter(t *testing.T) {
	t.Parallel()

	upsert := sqlWithoutComments(refreshAssetPriceSnapshotUpsert)
	for _, want := range []string{
		"LEFT JOIN nonstandard_decimals_assets nda ON nda.asset = ca.asset_id",
		"power(10::numeric, (nda.decimals - 7)::numeric)",
		// An asset with no confirmed row takes the untouched arm, so its
		// stored NUMERIC is the one it always was.
		"CASE WHEN nda.decimals IS NULL",
	} {
		if !strings.Contains(upsert, want) {
			t.Errorf("price upsert missing %q", want)
		}
	}
	if got := strings.Count(upsert, "power(10::numeric"); got != 1 {
		t.Errorf("price upsert applies the decimals factor %d times, want exactly 1 "+
			"(the change columns are same-scale ratios and take none)", got)
	}

	// The listing spine reads the corrected column and must not correct
	// it again.
	if reader := sqlWithoutComments(listAssetsBaseSelect); strings.Contains(reader, "nonstandard_decimals_assets") {
		t.Error("the listing SELECT joins nonstandard_decimals_assets — asset_price_snapshot " +
			"is already normalised by its writer, so this would apply the factor twice")
	}
}

// TestCatalogueReads_RoundAfterTheDecimalsCorrection pins the OTHER half
// of the split: the per-asset row and the four price-history reads stay
// RAW (the API owns the multiply) but must not round a confirmed token's
// raw ratio on the flat 10-place scale, which for an 18-decimals token
// turns 14 USD into a value that reads back as exactly 10. The executing
// proof is TestAssetCatalogue_RoundsAfterDecimalsCorrection.
func TestCatalogueReads_RoundAfterTheDecimalsCorrection(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ sql, places string }{
		"GetAssetPriceHistory24h":       {getAssetPriceHistory24hSQL, catalogueRoundPlacesForAliases},
		"GetAssetPriceHistory7d":        {getAssetPriceHistory7dSQL, catalogueRoundPlacesForAliases},
		"GetAssetBySlug":                {getAssetBySlugSQL, catalogueRoundPlacesForRow},
		"GetAssetsPriceHistory24hBatch": {getAssetsPriceHistory24hBatchSQL, catalogueRoundPlacesForWanted},
		"GetAssetsPriceHistory7dBatch":  {getAssetsPriceHistory7dBatchSQL, catalogueRoundPlacesForWanted},
	} {
		if got := strings.Count(tc.sql, "), "+tc.places+")::text"); got != 1 {
			t.Errorf("%s rounds its price on the decimals-aware scale %d times, want exactly 1", name, got)
		}
		if strings.Contains(sqlWithoutComments(tc.sql), "), 10)::text") {
			t.Errorf("%s still rounds a price to a flat 10 places before the correction can run", name)
		}
		// Rounding scale only: a multiply here would stack on the API's.
		if strings.Contains(sqlWithoutComments(tc.sql), "power(") {
			t.Errorf("%s scales the price in SQL — the API already does, this would apply the factor twice", name)
		}
	}

	places := sqlWithoutComments(catalogueRoundPlacesForRow)
	for _, want := range []string{
		"10 + COALESCE(",                // no confirmed row: exactly the old ROUND(…, 10)
		"GREATEST(nda.decimals - 7, 0)", // scaling DOWN never narrows the rounding
		"nonstandard_decimals_assets",
	} {
		if !strings.Contains(places, want) {
			t.Errorf("rounding-scale expression missing %q: %s", want, places)
		}
	}
}

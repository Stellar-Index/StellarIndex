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

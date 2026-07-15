// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// TestCatalogueSlugToStellarAssetID probes the exact chain
// fillCatalogueStatsForPage relies on (AM-10 enrichment live-debug):
// wire Slug → LookupBySlug → StellarEntry → AssetID.
func TestCatalogueSlugToStellarAssetID(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"usdc", "aqua", "yxlm"} {
		vc, ok := cat.LookupBySlug(slug)
		if !ok {
			t.Errorf("%s: no catalogue match", slug)
			continue
		}
		e := vc.StellarEntry()
		if e == nil || e.AssetID == "" {
			t.Errorf("%s: matched but no stellar asset_id (entry=%v)", slug, e)
			continue
		}
		t.Logf("%s → %s", slug, e.AssetID)
	}
}

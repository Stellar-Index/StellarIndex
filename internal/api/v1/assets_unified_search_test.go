// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"net/url"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// TestUnifiedSearchFindsCatalogueAssetByStellarIssuance: the classic phase matches q against code, asset_id (slug) and issuer
// in SQL, then drops the matching twin as a catalogue duplicate. So for every Stellar-issued entry, a q equal to any of those
// columns must keep the catalogue row, or the asset vanishes from both phases.
func TestUnifiedSearchFindsCatalogueAssetByStellarIssuance(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	// A reader is wired so the classic fill runs; it holds no rows, so every row served is a catalogue row.
	s := New(Options{VerifiedCurrencies: cat, AssetsReader: &filterCapturingAssets{}})
	entries := cat.StellarIssued()
	if len(entries) == 0 {
		t.Fatal("embedded catalogue has no Stellar-issued entries; the test exercises nothing")
	}
	for _, vc := range entries {
		se := vc.StellarEntry()
		if se == nil {
			continue
		}
		for column, q := range map[string]string{"code": se.Code, "asset_id": se.AssetID, "issuer": se.Issuer} {
			if q == "" {
				continue
			}
			rec := serveAssets(t, s, "/v1/assets?asset_class=all&limit=100&q="+url.QueryEscape(q))
			found := false
			for _, row := range decodeAssetRows(t, rec) {
				if row.Slug == vc.Slug {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("q=%s (its %s) did not return catalogue row %q, whose classic twin is suppressed",
					q, column, vc.Slug)
			}
		}
	}
}

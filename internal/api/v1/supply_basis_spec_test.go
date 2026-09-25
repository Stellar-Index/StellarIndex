// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// unexcludedListingBases are the bases classicSupplyReading (and the SEP-41
// lake arm) publish as circulating_supply without netting out any issuer or
// locked-set balance: each is a total, and total == circulating under it.
var unexcludedListingBases = []supply.Basis{
	supply.BasisClassicLakeFlows,
	supply.BasisClassicTrustlineSum,
	supply.BasisSEP41LakeFlows,
}

// TestSpecNamesTheUnexcludedCirculatingBases — the listing's fallback arms
// publish an un-excluded total under circulating_supply while an exclusion
// basis subtracts the issuer and locked sets: one field, two definitions,
// and the spec's circulating_supply said neither. Every such basis must be
// named where the field is defined, so a consumer comparing a listing row
// to a detail page can tell a total from a netted figure.
func TestSpecNamesTheUnexcludedCirculatingBases(t *testing.T) {
	schemas, _ := loadSpecDoc(t)["components"].(map[string]any)["schemas"].(map[string]any)
	asset, _ := schemas["Asset"].(map[string]any)
	props, _ := asset["properties"].(map[string]any)
	circ, _ := props["circulating_supply"].(map[string]any)
	desc, _ := circ["description"].(string)
	if desc == "" {
		t.Fatal("Asset.circulating_supply has no description in the spec")
	}
	if !strings.Contains(desc, "un-excluded") {
		t.Errorf("Asset.circulating_supply does not say it is un-excluded under any basis:\n%s", desc)
	}
	for _, b := range unexcludedListingBases {
		if !strings.Contains(desc, "`"+b.String()+"`") {
			t.Errorf("Asset.circulating_supply does not name %s among the un-excluded bases:\n%s", b, desc)
		}
	}
}

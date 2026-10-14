// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"

	"github.com/Stellar-Index/StellarIndex/internal/supply"

	"gopkg.in/yaml.v3"
)

// The supply_basis vocabulary is declared once: the typed constants of [supply.Basis], which handlers stamp onto the wire
// (populateSupplyFields, rwaSupplyProvenance). The spec's two `supply_basis` enums are underivable YAML copies, pinned here;
// docs, Postman and types.ts are generated from the spec, so a missing value is missing everywhere.

// TestSupplyBasis_AssetSpecEnumIsTheGoVocabulary: `Asset.supply_basis` carries every Basis constant, in
// declaration order; a mismatch is a served basis the union rejects or a documented basis nothing emits.
func TestSupplyBasis_AssetSpecEnumIsTheGoVocabulary(t *testing.T) {
	got := specSupplyBasisEnum(t, "Asset")
	want := goSupplyBases(t)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi Asset.supply_basis enum = %v\n  internal/supply Basis consts   = %v\n"+
			"edit both (and regenerate docs/reference/api, examples/postman and "+
			"web/explorer/src/api/types.ts from the spec)", got, want)
	}
}

// TestSupplyBasis_RWAAssetSpecEnumIsTheGoVocabulary: `RWAAsset.supply_basis` carries every basis a supply can
// travel with. rwaSupplyProvenance names a basis only beside a non-nil circulating_supply, so
// [supply.BasisNoMetadata] (the basis of a nil figure) is the one constant that can't reach this row.
func TestSupplyBasis_RWAAssetSpecEnumIsTheGoVocabulary(t *testing.T) {
	got := specSupplyBasisEnum(t, "RWAAsset")
	var want []string
	for _, b := range goSupplyBases(t) {
		if b != string(supply.BasisNoMetadata) {
			want = append(want, b)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi RWAAsset.supply_basis enum = %v\n  internal/supply Basis consts      = %v (less no_metadata)\n"+
			"edit both (and regenerate docs/reference/api, examples/postman and "+
			"web/explorer/src/api/types.ts from the spec)", got, want)
	}
}

// specSupplyBasisEnum returns the `enum` of components.schemas.<schema>
// .properties.supply_basis, in spec order.
func specSupplyBasisEnum(t *testing.T, schema string) []string {
	t.Helper()
	return specPropertyEnum(t, schema, "supply_basis")
}

// specPropertyEnum returns the `enum` of components.schemas.<schema>
// .properties.<prop>, in spec order.
func specPropertyEnum(t *testing.T, schema, prop string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "openapi", "stellar-index.v1.yaml")) //nolint:gosec // repo-relative path
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}
	s, ok := doc.Components.Schemas[schema]
	if !ok {
		t.Fatalf("spec has no components.schemas.%s", schema)
	}
	p, ok := s.Properties[prop]
	if !ok {
		t.Fatalf("components.schemas.%s has no %s property", schema, prop)
	}
	if len(p.Enum) == 0 {
		t.Fatalf("components.schemas.%s.%s has no enum", schema, prop)
	}
	return p.Enum
}

// goSupplyBases reads every typed [supply.Basis] constant from internal/supply; the source is the declaration, so
// no hand-kept slice to drift. Every file is walked, as a constant in overlay.go is as servable as one in supply.go.
func goSupplyBases(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "internal", "supply")
	out := astguard.Values(astguard.Typed(t, astguard.PackageConsts(t, dir), "Basis"))
	// The walk must have seen the real block, or every assertion above
	// holds vacuously against an empty list.
	if len(out) == 0 {
		t.Fatalf("%s: no typed Basis constants found — this guard needs re-aiming", dir)
	}
	var seen bool
	for _, s := range out {
		if s == string(supply.BasisSEP41TotalOnly) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("%s: walk did not enumerate %q", dir, supply.BasisSEP41TotalOnly)
	}
	return out
}

// goFileStringConsts reads the string constants of type typeName in one file, in declaration order. Unlike
// packageStringConsts (a whole directory glob), it is scoped to a file, which
// TestPriceAuthority_SpecEnumIsTheGoVocabulary compares positionally.
func goFileStringConsts(t *testing.T, file, typeName string) []string {
	t.Helper()
	out := astguard.Values(astguard.Typed(t, astguard.FileConsts(t, file), typeName))
	if len(out) == 0 {
		t.Fatalf("%s: no typed %s constants found — this guard needs re-aiming", file, typeName)
	}
	return out
}

// TestSupplyBasis_ListingValuationSpecEnumIsTheGoVocabulary —
// `AssetListingValuation.supply_basis` is a separate vocabulary from [supply.Basis]: its
// values are this package's ListingSupplyBasis* constants, stamped verbatim by the handler.
func TestSupplyBasis_ListingValuationSpecEnumIsTheGoVocabulary(t *testing.T) {
	got := specSupplyBasisEnum(t, "AssetListingValuation")
	want := astguard.Values(astguard.Keep(t, astguard.PackageConsts(t, "."), func(c astguard.Const) bool {
		return strings.HasPrefix(c.Name, "ListingSupplyBasis")
	}))
	if len(want) == 0 {
		t.Fatal("no ListingSupplyBasis* constants found in package v1 — this guard needs re-aiming")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi AssetListingValuation.supply_basis enum = %v\n  v1 ListingSupplyBasis* consts               = %v\n"+
			"edit both (and regenerate docs/reference/api, examples/postman and "+
			"web/explorer/src/api/types.ts from the spec)", got, want)
	}
}

// unexcludedListingBases are the bases classicSupplyReading (and the SEP-41
// lake arm) publish as circulating_supply without netting out any issuer or
// locked-set balance: each is a total, and total == circulating under it.
var unexcludedListingBases = []supply.Basis{
	supply.BasisClassicLakeFlows,
	supply.BasisClassicTrustlineSum,
	supply.BasisSEP41LakeFlows,
}

// TestSpecNamesTheUnexcludedCirculatingBases: listing fallback arms publish an un-excluded total as
// circulating_supply while an exclusion basis nets out issuer and locked sets. Every such basis must be named where
// the field is defined, so a consumer can tell a total from a netted figure.
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

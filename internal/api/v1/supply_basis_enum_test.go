// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// ─── The supply_basis vocabulary has one declaration ─────────────────
//
// [supply.Basis]'s const block in internal/supply/supply.go is it. The
// handlers stamp the wire field straight from a snapshot's Basis
// (populateSupplyFields, rwaSupplyProvenance), so every constant there
// is a value a consumer can receive. The spec's two `supply_basis`
// enums are copies that cannot be derived — they are YAML — so they
// are pinned here, the way ohlc_intervals_test.go pins the interval
// ladder to its route table. The docs mirror, the Postman collection
// and the explorer's types.ts are all generated from the spec, so a
// value missing from the spec is missing from every published contract
// at once: `sep41_total_only` was served from v0.21.0 and absent from
// all of them until this pin.

// TestSupplyBasis_AssetSpecEnumIsTheGoVocabulary — `Asset.supply_basis`
// carries every Basis constant, in declaration order. A value in one
// and not the other is either a served basis the closed union rejects
// or a documented basis nothing can emit.
func TestSupplyBasis_AssetSpecEnumIsTheGoVocabulary(t *testing.T) {
	got := specSupplyBasisEnum(t, "Asset")
	want := goSupplyBases(t)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi Asset.supply_basis enum = %v\n  internal/supply Basis consts   = %v\n"+
			"edit both (and regenerate docs/reference/api, examples/postman and "+
			"web/explorer/src/api/types.ts from the spec)", got, want)
	}
}

// TestSupplyBasis_RWAAssetSpecEnumIsTheGoVocabulary —
// `RWAAsset.supply_basis` carries every basis a supply can travel
// WITH. rwaSupplyProvenance names a basis only beside a non-nil
// circulating_supply, and [supply.BasisNoMetadata] is by its own
// docstring the basis of a nil figure, so it is the one constant that
// cannot reach this row.
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

// goSupplyBases reads the [supply.Basis] const block out of
// internal/supply/supply.go in declaration order. The source is the
// declaration; a hand-maintained slice beside it would be a third copy
// for this test to drift from.
func goSupplyBases(t *testing.T) []string {
	t.Helper()
	file := filepath.Join(repoRoot(t), "internal", "supply", "supply.go")
	out := goStringConsts(t, file, "Basis")
	var seen bool
	for _, s := range out {
		if s == string(supply.BasisSEP41TotalOnly) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("%s: walk did not enumerate %q", file, supply.BasisSEP41TotalOnly)
	}
	return out
}

// goStringConsts reads the string constants of type typeName declared in
// file, in declaration order.
func goStringConsts(t *testing.T, file, typeName string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []string
	for _, decl := range parsed.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != typeName {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: %s const %v is not a string literal", file, typeName, vs.Names)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s: %v", file, lit.Value, err)
				}
				out = append(out, s)
			}
		}
	}
	// The walk must have seen the real block, or every assertion above
	// holds vacuously against an empty list.
	if len(out) == 0 {
		t.Fatalf("%s: no typed %s constants found — this guard needs re-aiming", file, typeName)
	}
	return out
}

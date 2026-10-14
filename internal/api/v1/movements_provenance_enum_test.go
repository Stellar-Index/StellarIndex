// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The movements provenance vocabulary has two declaration sites: [classicmovements.Provenance] and
// [clickhouse.ProvenanceCAP67Derived]. The spec's AccountMovement.provenance enum copies them and generated contracts
// derive from it, so a value missing from the spec is served but rejected.

// TestAccountMovementProvenance_SpecEnumIsTheGoVocabulary — the spec
// enum is exactly the set of provenance values a writer can stamp.
func TestAccountMovementProvenance_SpecEnumIsTheGoVocabulary(t *testing.T) {
	got := specAccountMovementProvenanceEnum(t)
	want := append(goClassicMovementProvenances(t), clickhouse.ProvenanceCAP67Derived)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi AccountMovement.provenance enum = %v\n  Go provenance consts              = %v\n"+
			"edit both (and regenerate docs/reference/api, examples/postman and "+
			"web/explorer/src/api/types.ts from the spec)", got, want)
	}
}

// specAccountMovementProvenanceEnum returns the `enum` of
// components.schemas.AccountMovement.properties.provenance.
func specAccountMovementProvenanceEnum(t *testing.T) []string {
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
	s, ok := doc.Components.Schemas["AccountMovement"]
	if !ok {
		t.Fatal("spec has no components.schemas.AccountMovement")
	}
	p, ok := s.Properties["provenance"]
	if !ok {
		t.Fatal("components.schemas.AccountMovement has no provenance property")
	}
	if len(p.Enum) == 0 {
		t.Fatal("components.schemas.AccountMovement.provenance has no enum")
	}
	return p.Enum
}

// goClassicMovementProvenances parses the [classicmovements.Provenance] const block from source instead of importing it:
// that package imports internal/api, and a string copy would be a third declaration to drift.
func goClassicMovementProvenances(t *testing.T) []string {
	t.Helper()
	file := filepath.Join(repoRoot(t), "internal", "sources", "classicmovements", "events.go")
	out := astguard.Values(astguard.Typed(t, astguard.FileConsts(t, file), "Provenance"))
	if len(out) == 0 {
		t.Fatalf("%s: found no Provenance consts — the walk is broken, and an empty subject set passes forever", file)
	}
	return out
}

// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ─── The movements provenance vocabulary has two declaration sites ──
//
// explorer.AccountMovements passes `provenance` straight through from the
// row, so every value a writer stamps is a value a consumer receives.
// Three writers, two declarations: the [classicmovements.Provenance]
// const block (classic_derived on the ADR-0047 archive; cap67_event on
// the Postgres recent tail) and [clickhouse.ProvenanceCAP67Derived]
// (the ch-cap67-movements derive that continues the archive past P23).
// The spec's `AccountMovement.provenance` enum is a YAML copy of that
// set, and the explorer types, Postman collection and docs mirror are
// generated from it — so a value missing from the spec is a value every
// published contract rejects while the API serves it. cap67_derived was
// served from the ClickHouse arm and absent from all of them until this
// pin.

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

// goClassicMovementProvenances reads the [classicmovements.Provenance]
// const block out of its source file. Parsed rather than imported: that
// package reaches back into internal/api, and a string copy here would
// be a third declaration for this test to drift from.
func goClassicMovementProvenances(t *testing.T) []string {
	t.Helper()
	file := filepath.Join(repoRoot(t), "internal", "sources", "classicmovements", "events.go")
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
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Provenance" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: Provenance const %v is not a string literal", file, vs.Names)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s: %v", file, lit.Value, err)
				}
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: found no Provenance consts — the walk is broken, and an empty subject set passes forever", file)
	}
	return out
}

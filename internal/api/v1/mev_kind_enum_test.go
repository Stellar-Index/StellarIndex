// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/mev"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMEVKind_SpecEnumIsTheHandlerVocabulary pins /v1/mev's ?kind=
// allow-list to the spec enum in both directions: a spec value the
// handler 400s breaks documented clients, and a handler value missing
// from the spec is served but rejected by every generated client.
func TestMEVKind_SpecEnumIsTheHandlerVocabulary(t *testing.T) {
	spec := specMEVKindEnum(t)
	handler := make([]string, 0, len(validMEVKinds))
	for k := range validMEVKinds {
		handler = append(handler, k)
	}
	sort.Strings(spec)
	sort.Strings(handler)
	if strings.Join(spec, ",") != strings.Join(handler, ",") {
		t.Errorf("openapi /mev ?kind= enum = %v\n  validMEVKinds          = %v\nedit both "+
			"(and regenerate docs/reference/api, examples/postman and web/explorer/src/api/types.ts)",
			spec, handler)
	}
}

// TestMEVKind_DetectorKindsAreInSpec — every kind a detector writes to
// mev_events.kind must be filterable, or its events are unreachable by
// ?kind= and a generated client rejects the value in responses.
func TestMEVKind_DetectorKindsAreInSpec(t *testing.T) {
	inSpec := map[string]bool{}
	for _, k := range specMEVKindEnum(t) {
		inSpec[k] = true
	}
	for _, k := range []string{
		mev.KindArbitrage, mev.KindSandwich, mev.KindOracleSandwich,
		mev.KindLiquidationCascade, mev.KindWashTrade,
	} {
		if !inSpec[k] {
			t.Errorf("detector kind %q is not in the openapi /mev ?kind= enum", k)
		}
	}
}

type enumStubMEVReader struct{ kinds []string }

func (r *enumStubMEVReader) ListMEVEvents(_ context.Context, kind string, _ int) ([]timescale.MEVEventRow, error) {
	r.kinds = append(r.kinds, kind)
	return nil, nil
}

// TestMEVKind_HandlerEnforcesSpecEnum drives the real handler: every
// spec value reaches the store unchanged, a value outside the enum is a
// 400 invalid-kind problem that never reaches the store, and that
// problem's detail names every accepted value.
func TestMEVKind_HandlerEnforcesSpecEnum(t *testing.T) {
	reader := &enumStubMEVReader{}
	h := New(Options{MEV: reader}).Handler()
	get := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/mev?kind="+q, nil))
		return rec
	}
	spec := specMEVKindEnum(t)
	for _, k := range spec {
		if rec := get(k); rec.Code != http.StatusOK {
			t.Errorf("?kind=%s: status %d, want 200: %s", k, rec.Code, rec.Body.String())
		}
	}
	if strings.Join(reader.kinds, ",") != strings.Join(spec, ",") {
		t.Errorf("store saw kinds %v, want the spec enum %v passed through", reader.kinds, spec)
	}

	reader.kinds = nil
	rec := get("oracle_sandwhich") //nolint:misspell // deliberately invalid kind
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid-kind") {
		t.Fatalf("?kind=oracle_sandwhich: status %d, want 400 invalid-kind: %s", rec.Code, rec.Body.String()) //nolint:misspell // deliberately invalid kind
	}
	if len(reader.kinds) != 0 {
		t.Errorf("an invalid kind reached the store: %v", reader.kinds)
	}
	for _, k := range spec {
		if !strings.Contains(rec.Body.String(), k) {
			t.Errorf("invalid-kind detail does not name accepted value %q: %s", k, rec.Body.String())
		}
	}
}

// specMEVKindEnum returns the `enum` of the `kind` query parameter on
// paths./mev.get.
func specMEVKindEnum(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "openapi", "stellar-index.v1.yaml")) //nolint:gosec // repo-relative path
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name   string `yaml:"name"`
					In     string `yaml:"in"`
					Schema struct {
						Enum []string `yaml:"enum"`
					} `yaml:"schema"`
				} `yaml:"parameters"`
			} `yaml:"get"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}
	op, ok := doc.Paths["/mev"]
	if !ok {
		t.Fatal("spec has no paths./mev")
	}
	for _, p := range op.Get.Parameters {
		if p.Name == "kind" && p.In == "query" {
			if len(p.Schema.Enum) == 0 {
				t.Fatal("paths./mev.get kind parameter has no enum")
			}
			return p.Schema.Enum
		}
	}
	t.Fatal("paths./mev.get has no kind query parameter")
	return nil
}

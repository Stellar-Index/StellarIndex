// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// specOpaqueObjectAllowlist names every spec property typed as a free-form
// object (`additionalProperties: true`, no `properties`). A generated
// client sees `unknown` for these, so each must be genuinely shapeless.
var specOpaqueObjectAllowlist = map[string]string{
	"sep1_payload": "issuer-authored stellar.toml content, verbatim",
	"fields":       "per-operation-type decoded fields; one shape per op type",
	"attributes":   "per-movement-kind remainder; one shape per kind",
}

// TestSpecOpaqueObjectsAreAllowlisted fails when a response property is
// documented as an untyped object — the shape that left protocol
// `bespoke` and account/me `user`/`account` as `unknown` to every
// generated consumer although the handler serves a fixed struct.
func TestSpecOpaqueObjectsAreAllowlisted(t *testing.T) {
	doc := loadSpecDoc(t)
	seen := map[string]bool{}
	var unlisted []string
	walkSpec(doc, "", func(key string, m map[string]any) {
		if ap, ok := m["additionalProperties"].(bool); !ok || !ap || m["properties"] != nil {
			return
		}
		seen[key] = true
		if specOpaqueObjectAllowlist[key] == "" {
			unlisted = append(unlisted, key)
		}
	})
	sort.Strings(unlisted)
	if len(unlisted) > 0 {
		t.Errorf("spec properties typed as opaque objects: %v — give them a schema matching the handler struct", unlisted)
	}
	for k := range specOpaqueObjectAllowlist {
		if !seen[k] {
			t.Errorf("stale specOpaqueObjectAllowlist entry %q; delete it", k)
		}
	}
}

// TestSpecJSONSuccessResponsesHaveSchema fails on a 2xx application/json
// response that documents only an example: nothing can validate it.
func TestSpecJSONSuccessResponsesHaveSchema(t *testing.T) {
	doc := loadSpecDoc(t)
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("spec has no paths — the walk is broken")
	}
	var missing []string
	checked := 0
	for path, item := range paths {
		ops, _ := item.(map[string]any)
		for method, op := range ops {
			opm, _ := op.(map[string]any)
			responses, _ := opm["responses"].(map[string]any)
			for code, resp := range responses {
				if !strings.HasPrefix(code, "2") {
					continue
				}
				respm, _ := resp.(map[string]any)
				content, _ := respm["content"].(map[string]any)
				media, ok := content["application/json"].(map[string]any)
				if !ok {
					continue
				}
				checked++
				if media["schema"] == nil {
					missing = append(missing, strings.ToUpper(method)+" "+path+" "+code)
				}
			}
		}
	}
	if checked < 50 {
		t.Fatalf("checked only %d JSON success responses — the walk is broken", checked)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("2xx application/json responses with no schema: %v", missing)
	}
}

// walkSpec visits every mapping node, passing the nearest enclosing key.
func walkSpec(node any, key string, visit func(string, map[string]any)) {
	switch v := node.(type) {
	case map[string]any:
		visit(key, v)
		for k, child := range v {
			walkSpec(child, k, visit)
		}
	case []any:
		for _, child := range v {
			walkSpec(child, key, visit)
		}
	}
}

// TestKeyCreatedAlwaysServesLabel: the spec marks `label` required on the
// key-mint response, so the wire must carry it even when empty.
func TestKeyCreatedAlwaysServesLabel(t *testing.T) {
	b, err := json.Marshal(KeyCreated{KeyID: "kid_x", Plaintext: "sip_x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"label":""`) {
		t.Fatalf("KeyCreated dropped required label: %s", b)
	}
}

// TestBespokeEmptyTableServesRowsArray: a table with no rows serves
// `rows: []`; null would fail the spec's array type.
func TestBespokeEmptyTableServesRowsArray(t *testing.T) {
	out := bespokeFromStore(&timescale.BespokeBlock{
		Category: "lending",
		Tables:   []timescale.BespokeTable{{Title: "Recent auctions", Columns: []string{"When"}}},
	})
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"rows":[]`) {
		t.Fatalf("empty bespoke table rows not an array: %s", b)
	}
}

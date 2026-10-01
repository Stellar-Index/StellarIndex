package v1

import (
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// divergenceReferences is every reference name the divergence worker
// records (cmd/stellarindex-api wiring + divergence.SyntheticCrossName).
var divergenceReferences = map[string]bool{
	"chainlink":                   true,
	"coingecko":                   true,
	"reflector-cex":               true,
	"reflector-fx":                true,
	"reflector-dex":               true,
	"redstone":                    true,
	"band":                        true,
	divergence.SyntheticCrossName: true,
}

// TestDivergenceReferenceSpecEnumsMatchAllowList pins every OpenAPI
// `reference` enum under /divergence* to divergenceReferences: a
// source the worker records but the spec omits makes a generated
// client reject a valid response.
func TestDivergenceReferenceSpecEnumsMatchAllowList(t *testing.T) {
	want := make([]string, 0, len(divergenceReferences))
	for ref := range divergenceReferences {
		want = append(want, ref)
	}
	sort.Strings(want)

	paths, _ := loadSpecDoc(t)["paths"].(map[string]any)
	var enums int
	for path, item := range paths {
		if !strings.HasPrefix(path, "/divergence") {
			continue
		}
		collectReferenceEnums(item, func(got []string) {
			enums++
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s: reference enum %v, want %v (divergenceReferences)", path, got, want)
			}
		})
	}
	// board pairs[].references[].reference, series points[].references[].reference
	if enums != 2 {
		t.Fatalf("found %d reference enums under /divergence*, want 2", enums)
	}
}

// TestDivergenceReferenceAllowListCoversSyntheticCross keeps the
// served synthetic reference in the spec's enum.
func TestDivergenceReferenceAllowListCoversSyntheticCross(t *testing.T) {
	if !divergenceReferences[divergence.SyntheticCrossName] {
		t.Errorf("divergenceReferences lacks %q", divergence.SyntheticCrossName)
	}
}

// collectReferenceEnums walks a spec subtree and reports the enum of
// every string schema reached through a `reference` property or a
// parameter named `reference`.
func collectReferenceEnums(node any, report func([]string)) {
	switch n := node.(type) {
	case map[string]any:
		if n["name"] == "reference" {
			if schema, ok := n["schema"].(map[string]any); ok {
				reportEnum(schema, report)
			}
		}
		for k, v := range n {
			if k == "reference" {
				if schema, ok := v.(map[string]any); ok {
					reportEnum(schema, report)
					continue
				}
			}
			collectReferenceEnums(v, report)
		}
	case []any:
		for _, v := range n {
			collectReferenceEnums(v, report)
		}
	}
}

func reportEnum(schema map[string]any, report func([]string)) {
	raw, ok := schema["enum"].([]any)
	if !ok {
		return
	}
	vals := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			vals = append(vals, s)
		}
	}
	report(vals)
}

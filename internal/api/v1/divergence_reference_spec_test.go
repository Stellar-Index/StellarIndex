package v1

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// TestDivergenceReferenceSpecEnumsMatchAllowList pins every OpenAPI
// `reference` enum under /divergence* to divergenceReferences: a
// source the handler serves but the spec omits makes a generated
// client reject a valid response or refuse to send a valid query.
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
				t.Errorf("%s: reference enum %v, want %v (anomalies.go divergenceReferences)", path, got, want)
			}
		})
	}
	// board observations[].reference, series ?reference=, series response reference
	if enums != 3 {
		t.Fatalf("found %d reference enums under /divergence*, want 3", enums)
	}
}

// TestDivergenceReferenceAllowListCoversSyntheticCross keeps the
// served synthetic reference admissible to /v1/divergence/series and
// named in its 400 message.
func TestDivergenceReferenceAllowListCoversSyntheticCross(t *testing.T) {
	if !divergenceReferences[divergence.SyntheticCrossName] {
		t.Errorf("divergenceReferences lacks %q", divergence.SyntheticCrossName)
	}
	rec := httptest.NewRecorder()
	newSeriesServer(nil, 0).handleDivergenceSeries(rec, httptest.NewRequest(http.MethodGet,
		"/v1/divergence/series?pair=crypto:BTC~fiat:USD&reference=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	for ref := range divergenceReferences {
		if !strings.Contains(rec.Body.String(), ref) {
			t.Errorf("invalid-reference 400 does not name %q: %s", ref, rec.Body.String())
		}
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

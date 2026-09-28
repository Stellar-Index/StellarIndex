package v1

import (
	"reflect"
	"sort"
	"testing"
)

// TestMethodologySourcesUseOwnSchema pins GH-630(d): /v1/methodology's
// `sources[]` documented the full Source schema (trade_count_24h,
// volume_24h_usd, markets_count_24h, volume_history_24h/7d) although
// handleMethodology never populates those — MethodologySource is the
// static registry subset it actually serves.
func TestMethodologySourcesUseOwnSchema(t *testing.T) {
	doc := loadSpecDoc(t)
	schemas := mustMap(t, doc, "components", "schemas")

	methodology := mustMap(t, schemas, "Methodology")
	sourcesProp := mustMap(t, methodology, "properties", "sources")
	items := mustMap(t, sourcesProp, "items")
	ref, _ := items["$ref"].(string)
	if ref != "#/components/schemas/MethodologySource" {
		t.Fatalf("Methodology.sources.items.$ref = %q, want #/components/schemas/MethodologySource", ref)
	}

	specProps := schemaPropertyNames(t, schemas, "MethodologySource")
	goProps := jsonFieldNames(reflect.TypeOf(MethodologySource{}))
	assertSameSet(t, "MethodologySource", specProps, goProps)
}

// TestDiagnosticsIngestionBackfillCoverageFieldsDocumented pins GH-630(c):
// BackfillCoverageRow serves density_pct, covered_ledgers,
// expected_ledgers, gap_free_pct and coverage_snapshot_at unconditionally
// (they are omitempty, but genuinely populated once the gap detector has
// run) and the spec named none of them as properties — a prose disclaimer
// is not a schema.
func TestDiagnosticsIngestionBackfillCoverageFieldsDocumented(t *testing.T) {
	doc := loadSpecDoc(t)
	op := mustMap(t, doc, "paths", "/diagnostics/ingestion", "get")
	schema := mustMap(t, op, "responses", "200", "content", "application/json", "schema")
	allOf, _ := schema["allOf"].([]any)
	if len(allOf) < 2 {
		t.Fatalf("/diagnostics/ingestion 200 schema.allOf has %d entries, want >= 2", len(allOf))
	}
	obj, _ := allOf[1].(map[string]any)
	data := mustMap(t, obj, "properties", "data", "properties", "backfill_coverage", "items", "properties")

	for _, want := range []string{"density_pct", "covered_ledgers", "expected_ledgers", "gap_free_pct", "coverage_snapshot_at"} {
		if _, ok := data[want]; !ok {
			t.Errorf("backfill_coverage[].%s: BackfillCoverageRow serves it (diagnostics_ingestion.go), spec doesn't document it", want)
		}
	}
}

// TestDiagnosticsIngestionSourcesEntriesAndEnabledDocumented pins the
// `sources[]` half of GH-630(c): SourceHealthRow serves entries_24h and
// enabled unconditionally (no omitempty) and the spec's inline sources
// item schema omitted both.
func TestDiagnosticsIngestionSourcesEntriesAndEnabledDocumented(t *testing.T) {
	doc := loadSpecDoc(t)
	op := mustMap(t, doc, "paths", "/diagnostics/ingestion", "get")
	schema := mustMap(t, op, "responses", "200", "content", "application/json", "schema")
	allOf, _ := schema["allOf"].([]any)
	obj, _ := allOf[1].(map[string]any)
	data := mustMap(t, obj, "properties", "data", "properties", "sources", "items", "properties")

	for _, want := range []string{"entries_24h", "enabled"} {
		if _, ok := data[want]; !ok {
			t.Errorf("sources[].%s: SourceHealthRow serves it unconditionally (diagnostics_ingestion.go), spec doesn't document it", want)
		}
	}
}

// --- helpers -----------------------------------------------------------

func mustMap(t *testing.T, root map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := root
	for i, k := range path {
		v, ok := cur[k]
		if !ok {
			t.Fatalf("path %v: key %q missing at depth %d", path, k, i)
		}
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("path %v: key %q at depth %d is not a map (%T)", path, k, i, v)
		}
		cur = m
	}
	return cur
}

// schemaPropertyNames returns the top-level `properties` keys of a named
// component schema.
func schemaPropertyNames(t *testing.T, schemas map[string]any, name string) []string {
	t.Helper()
	sch := mustMap(t, schemas, name)
	props := mustMap(t, sch, "properties")
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// jsonFieldNames returns the `json:"..."` tag names (ignoring "-") of a
// struct type's exported fields.
func jsonFieldNames(typ reflect.Type) []string {
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		for j, c := range tag {
			if c == ',' {
				name = tag[:j]
				break
			}
		}
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func assertSameSet(t *testing.T, label string, a, b []string) {
	t.Helper()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s: spec properties %v != Go json fields %v", label, a, b)
	}
}

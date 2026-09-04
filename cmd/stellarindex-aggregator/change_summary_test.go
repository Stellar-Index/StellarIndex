package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// changesSpecPath is the operation the change-summary worker is the
// sole producer for.
const changesSpecPath = "/changes/{entity_type}/{id}"

// specParameter is the slice of an OpenAPI parameter object this
// test reads: the documented value set (schema.enum) and the two
// places a documented example can live. Tooling reads both — Scalar
// prefers the parameter-level `example`, the Postman generator reads
// the schema-level one — so an example that 404s in either location
// is an example a user will actually send.
type specParameter struct {
	Name    string `yaml:"name"`
	In      string `yaml:"in"`
	Example any    `yaml:"example"`
	Schema  struct {
		Enum    []string `yaml:"enum"`
		Example any      `yaml:"example"`
	} `yaml:"schema"`
}

type specOperation struct {
	Parameters []specParameter `yaml:"parameters"`
}

type changesSpecDoc struct {
	Paths map[string]map[string]specOperation `yaml:"paths"`
}

// examples returns the distinct documented example strings for the
// parameter, in the order tooling would find them. The two locations
// normally carry the same value; deduping keeps one bad example from
// being reported once per location pairing.
func (p specParameter) examples() []string {
	var out []string
	add := func(v any) {
		s, ok := v.(string)
		if !ok || s == "" {
			return
		}
		for _, seen := range out {
			if seen == s {
				return
			}
		}
		out = append(out, s)
	}
	add(p.Example)
	add(p.Schema.Example)
	return out
}

// TestChangeSummarySpecMatchesEmittedEntities reconciles the
// documented surface of GET /v1/changes/{entity_type}/{id} with the
// only thing that populates it — buildChangeSummaryEntities over the
// aggregator's pair set.
//
// change_summary_5m has exactly one writer, this worker, and it can
// only key an entity on a canonical pair's VWAP series. The spec
// previously advertised four families (`coin`, `protocol`, `pair`,
// `source`) and used `source`/`binance` as its own example, so the
// documented example — and the SDK call built from it — 404'd on
// every deployment, with the 404 blaming worker lag for rows no
// worker computes. Anything the spec names must be a family this
// producer emits, and the documented example must name a row it
// actually writes.
func TestChangeSummarySpecMatchesEmittedEntities(t *testing.T) {
	families, entities := emittedChangeSummarySurface(t)

	params := changeSummaryParameters(t)
	entityType, ok := params["entity_type"]
	if !ok {
		t.Fatalf("spec %s GET declares no entity_type path parameter", changesSpecPath)
	}
	id, ok := params["id"]
	if !ok {
		t.Fatalf("spec %s GET declares no id path parameter", changesSpecPath)
	}

	if len(entityType.Schema.Enum) == 0 {
		t.Fatalf("spec %s entity_type declares no enum — the served families are undocumented",
			changesSpecPath)
	}
	for _, family := range entityType.Schema.Enum {
		if _, ok := families[family]; !ok {
			t.Errorf("spec %s documents entity_type %q, but buildChangeSummaryEntities emits %v — "+
				"every request for that family 404s; drop it from the enum or land the worker that writes it",
				changesSpecPath, family, sortedKeys(families))
		}
	}

	typeExamples, idExamples := entityType.examples(), id.examples()
	if len(typeExamples) == 0 || len(idExamples) == 0 {
		t.Fatalf("spec %s must document an entity_type AND an id example (got %v / %v)",
			changesSpecPath, typeExamples, idExamples)
	}
	for _, ft := range typeExamples {
		for _, ex := range idExamples {
			if _, ok := entities[ft+"/"+ex]; !ok {
				t.Errorf("spec %s documents the example request %s/%s, which no emitted entity matches — "+
					"a user pasting the documented example gets a 404",
					changesSpecPath, ft, ex)
			}
		}
	}
}

// emittedChangeSummarySurface returns the families and the
// "type/id" entity keys the worker writes for the built-in pair set.
// The built-in set is what the reference deployment runs (no
// [aggregate].pairs override), so it is the honest floor of what the
// endpoint can serve.
func emittedChangeSummarySurface(t *testing.T) (families, entities map[string]struct{}) {
	t.Helper()
	families = map[string]struct{}{}
	entities = map[string]struct{}{}
	for _, e := range buildChangeSummaryEntities(defaultPairs()) {
		families[e.Type] = struct{}{}
		entities[e.Type+"/"+e.ID] = struct{}{}
	}
	if len(entities) == 0 {
		t.Fatal("buildChangeSummaryEntities(defaultPairs()) emitted nothing — " +
			"this test's discovery is broken, not the spec")
	}
	return families, entities
}

// changeSummaryParameters loads the spec's path parameters for the
// change-summary operation, keyed by parameter name.
func changeSummaryParameters(t *testing.T) map[string]specParameter {
	t.Helper()

	// Walk up to the repo root: this test runs from
	// cmd/stellarindex-aggregator/ under `go test ./...`.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var specPath string
	for i := 0; i < 8; i++ {
		try := filepath.Join(dir, "openapi", "stellar-index.v1.yaml")
		if _, err := os.Stat(try); err == nil {
			specPath = try
			break
		}
		dir = filepath.Dir(dir)
	}
	if specPath == "" {
		t.Fatal("could not locate openapi/stellar-index.v1.yaml from cwd; " +
			"this test must run inside the stellar-index repo tree")
	}
	body, err := os.ReadFile(specPath) //nolint:gosec // repo-relative spec path resolved above
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var doc changesSpecDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("yaml decode %s: %v", specPath, err)
	}
	op, ok := doc.Paths[changesSpecPath]["get"]
	if !ok {
		t.Fatalf("spec declares no GET %s", changesSpecPath)
	}
	out := make(map[string]specParameter, len(op.Parameters))
	for _, p := range op.Parameters {
		if p.In != "path" {
			continue
		}
		out[p.Name] = p
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"
)

// TestSourceClassSurfacesAgree pins every surface naming a source class to the external.Class constants: registry,
// methodology glossary, ?class= allow-list and both spec enums. A gap makes /v1/sources serve rows it can't
// filter by and breaks spec-validating clients. Constants are read from source so a new class fails until named.
func TestSourceClassSurfacesAgree(t *testing.T) {
	want := goStringConsts(t, filepath.Join("internal", "sources", "external"), "Class")
	if len(want) == 0 {
		t.Fatal("found no external.Class constants — the source walk is broken")
	}

	doc := loadSpecDoc(t)
	surfaces := map[string][]string{
		"validSourceClasses (/v1/sources ?class= allow-list)": sortedMapKeys(validSourceClasses),
		"served /v1/methodology source_classes[].name":        servedMethodologyClassNames(t),
		"openapi /sources ?class= enum":                       specQueryParamEnum(t, doc, "/sources", "class"),
		"openapi Methodology.source_classes[].name enum": specEnumAt(t, doc,
			"components", "schemas", "Methodology", "properties", "source_classes", "items", "properties", "name"),
		"openapi Source.class enum": specEnumAt(t, doc, "components", "schemas", "Source", "properties", "class"),
	}
	for surface, got := range surfaces {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want every external.Class constant %v", surface, got, want)
		}
	}
}

// TestSourcesInvalidClassNamesEveryAcceptedClass keeps the 400 detail
// derived from the allow-list: a hand-written list named four of the six
// values the filter accepted.
func TestSourcesInvalidClassNamesEveryAcceptedClass(t *testing.T) {
	rec := httptest.NewRecorder()
	New(Options{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sources?class=nonsense", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	for class := range validSourceClasses {
		if !strings.Contains(rec.Body.String(), class) {
			t.Errorf("invalid-class 400 does not name accepted class %q: %s", class, rec.Body.String())
		}
	}
}

func servedMethodologyClassNames(t *testing.T) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	New(Options{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/methodology", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/methodology = %d, want 200", rec.Code)
	}
	var env struct {
		Data struct {
			SourceClasses []struct {
				Name string `json:"name"`
			} `json:"source_classes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode methodology: %v", err)
	}
	names := make([]string, 0, len(env.Data.SourceClasses))
	for _, c := range env.Data.SourceClasses {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

// goStringConsts returns, sorted, the value of every string constant
// declared with the named type in the non-test files of a repo-relative
// package directory.
func goStringConsts(t *testing.T, relDir, typeName string) []string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), relDir)
	out := astguard.Values(astguard.Typed(t, astguard.PackageConsts(t, dir), typeName))
	sort.Strings(out)
	return out
}

func specEnumAt(t *testing.T, node any, keys ...string) []string {
	t.Helper()
	for _, k := range keys {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("spec path %v: %q is not under an object", keys, k)
		}
		node = m[k]
	}
	m, _ := node.(map[string]any)
	raw, ok := m["enum"].([]any)
	if !ok {
		t.Fatalf("spec path %v has no enum", keys)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func specQueryParamEnum(t *testing.T, doc map[string]any, path, param string) []string {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	get, _ := item["get"].(map[string]any)
	params, _ := get["parameters"].([]any)
	for _, p := range params {
		pm, _ := p.(map[string]any)
		if pm["name"] == param && pm["in"] == "query" {
			return specEnumAt(t, pm, "schema")
		}
	}
	t.Fatalf("spec GET %s has no query parameter %q", path, param)
	return nil
}

func sortedMapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

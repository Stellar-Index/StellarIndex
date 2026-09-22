package v1

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPriceBatchSpecDocumentsPairsAlias closes T550: handlePriceBatch
// (price.go, "F-0073 closure") accepts `pairs=` as an alias for
// `asset_ids=` on GET /v1/price/batch, but the spec's parameters block
// documented only `asset_ids`. A client reading the spec — or a
// generator deriving a client from it — never learns `pairs` exists,
// even though the server accepts it.
func TestPriceBatchSpecDocumentsPairsAlias(t *testing.T) {
	names := specGetParamNames(t, "/price/batch")
	if len(names) == 0 {
		t.Fatal("resolved no GET parameters for /price/batch — the lookup is broken")
	}
	if !names["pairs"] {
		t.Errorf("openapi spec's GET /price/batch parameters do not include `pairs`, "+
			"but handlePriceBatch accepts it as an alias for asset_ids; got %v", names)
	}
	if !names["asset_ids"] {
		t.Errorf("openapi spec's GET /price/batch parameters lost `asset_ids`; got %v", names)
	}
}

// specGetParamNames returns the query parameter names declared on the
// GET operation for the given path.
func specGetParamNames(t *testing.T, path string) map[string]bool {
	t.Helper()
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
		t.Fatal("could not locate openapi/stellar-index.v1.yaml from cwd")
	}
	body, err := os.ReadFile(specPath) //nolint:gosec // repo-relative path resolved above
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name string `yaml:"name"`
				} `yaml:"parameters"`
			} `yaml:"get"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}
	op, ok := doc.Paths[path]
	if !ok {
		t.Fatalf("spec has no path entry for %q", path)
	}
	out := map[string]bool{}
	for _, p := range op.Get.Parameters {
		if p.Name != "" {
			out[p.Name] = true
		}
	}
	return out
}

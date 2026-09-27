package v1

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPIProtocolCategoryEnumMatchesRegistry pins ProtocolRow.category's
// wire enum to the vocabulary protocolRegistry actually emits (GH-655).
// The spec previously listed `token` as a valid category, but no entry in
// protocolRegistry has ever used it — a generated client had to model a
// value /v1/protocols can never serve.
func TestOpenAPIProtocolCategoryEnumMatchesRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	node := doc
	for _, key := range []string{"components", "schemas"} {
		next, ok := node[key].(map[string]any)
		if !ok {
			t.Fatalf("spec missing %q", key)
		}
		node = next
	}
	protocolRow, ok := node["ProtocolRow"].(map[string]any)
	if !ok {
		t.Fatal("spec missing components.schemas.ProtocolRow")
	}
	props, ok := protocolRow["properties"].(map[string]any)
	if !ok {
		t.Fatal("ProtocolRow has no properties")
	}
	category, ok := props["category"].(map[string]any)
	if !ok {
		t.Fatal("ProtocolRow.properties has no category")
	}
	rawEnum, ok := category["enum"].([]any)
	if !ok {
		t.Fatal("category has no enum")
	}

	var specEnum []string
	for _, v := range rawEnum {
		specEnum = append(specEnum, v.(string))
	}
	sort.Strings(specEnum)

	seen := map[string]bool{}
	var want []string
	for _, p := range protocolRegistry {
		if !seen[p.Category] {
			seen[p.Category] = true
			want = append(want, p.Category)
		}
	}
	sort.Strings(want)

	if strings.Join(specEnum, ",") != strings.Join(want, ",") {
		t.Errorf("openapi ProtocolRow.category enum = %v, want %v (protocolRegistry's "+
			"actual categories) — the spec must not claim a category no registry entry emits",
			specEnum, want)
	}
}

package v1

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestHandlerResponseFieldsAreDocumented closes the gap that let
// `display_decimals` ship invisible (wave-D F-SDK-04).
//
// Two gates already reconcile this area and NEITHER could see it:
//
//   - lint-docs.sh compares handlers to the spec at ROUTE granularity
//     (method + path) — never fields.
//   - pkg/client's TestSDKSchemasMatchSpec compares the SDK to the spec
//     bidirectionally, which is genuinely useful but reconciles two
//     DERIVED artifacts. When a field exists only on the server, the SDK
//     and the spec agree — on both being wrong — and the gate stays
//     green.
//
// So a server-added response field was invisible to the whole chain.
// That is not hypothetical: F-1321 moved the issuer's SEP-1 rounding
// hint OFF `decimals` (where it inflated market_cap_usd by up to
// 10^(7-display_decimals)× and was an issuer-controlled manipulation
// vector) onto a new `display_decimals` field. The field never entered
// the spec, so every SDK consumer and the generated explorer types
// dropped it — the remediation's whole replacement surface was
// unreachable from the published product.
//
// This compares the HANDLER struct, which is the source of truth, to
// the spec.
func TestHandlerResponseFieldsAreDocumented(t *testing.T) {
	props := specSchemaProps(t, "Asset")
	if len(props) == 0 {
		t.Fatal("resolved no properties for the Asset schema — the lookup is " +
			"broken, and a check with an empty subject set passes forever")
	}

	got := structJSONTags(reflect.TypeOf(AssetDetail{}))
	if len(got) == 0 {
		t.Fatal("AssetDetail exposed no json tags — the reflection walk is broken")
	}

	// Fields the handler serves that the spec does not document. This is
	// the direction that hid display_decimals: a consumer cannot ask for
	// what it has never been told exists.
	var undocumented []string
	for f := range got {
		if !props[f] && handlerFieldSpecExceptions[f] == "" {
			undocumented = append(undocumented, f)
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("AssetDetail serves field(s) the OpenAPI spec does not document: %v\n"+
			"A server field absent from the spec is invisible to pkg/client, to the "+
			"explorer's generated types, and to every downstream consumer — and the "+
			"SDK-vs-spec gate cannot see it, because that reconciles two derived "+
			"artifacts which agree when BOTH are missing the field. Document it in "+
			"openapi/stellar-index.v1.yaml (and regenerate), or record it in "+
			"handlerFieldSpecExceptions with the reason it is deliberately internal.",
			undocumented)
	}
}

// handlerFieldSpecExceptions records response fields deliberately absent
// from the published spec, with the reason. An entry is a decision, not
// a way to silence the check.
var handlerFieldSpecExceptions = map[string]string{}

// structJSONTags returns the wire field names of a struct, following
// embedded structs and skipping `json:"-"`.
func structJSONTags(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	if t == nil || t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			for k := range structJSONTags(f.Type) {
				out[k] = true
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
	return out
}

// specSchemaProps returns the property names of a named schema under
// components.schemas.
func specSchemaProps(t *testing.T, schema string) map[string]bool {
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
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}
	out := map[string]bool{}
	for k := range doc.Components.Schemas[schema].Properties {
		out[k] = true
	}
	return out
}

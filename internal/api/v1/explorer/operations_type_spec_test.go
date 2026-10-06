package explorer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// The spec's `type` enum is what generated clients send; ?type= accepts
// exactly xdrjson's vocabulary. A protocol op added to one and not the other
// is either unreachable or a documented value that 400s.
func TestOperationsTypeEnum_MatchesXDRJSONVocabulary(t *testing.T) {
	var doc struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name   string `yaml:"name"`
					Schema struct {
						Items struct {
							Enum []string `yaml:"enum"`
						} `yaml:"items"`
					} `yaml:"schema"`
				} `yaml:"parameters"`
			} `yaml:"get"`
		} `yaml:"paths"`
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "openapi", "stellar-index.v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var spec []string
	for _, p := range doc.Paths["/operations"].Get.Parameters {
		if p.Name == "type" {
			spec = append(spec, p.Schema.Items.Enum...)
		}
	}

	var vocab []string
	var e xdr.OperationType
	for i := int32(0); e.ValidEnum(i); i++ {
		vocab = append(vocab, xdrjson.OpTypeName(xdr.OperationType(i)))
	}
	if len(vocab) < 27 {
		t.Fatalf("enumerated %d XDR op types, want at least 27; the walk is broken", len(vocab))
	}
	for _, name := range spec {
		if _, ok := xdrjson.OpTypeEnumString(name); !ok {
			t.Errorf("spec enum %q is not accepted by ?type= (xdrjson.OpTypeEnumString)", name)
		}
	}
	sort.Strings(spec)
	sort.Strings(vocab)
	if len(spec) != len(vocab) {
		t.Fatalf("spec /operations type enum has %d values, XDR op types map to %d:\nspec  %v\nvocab %v", len(spec), len(vocab), spec, vocab)
	}
	for i := range spec {
		if spec[i] != vocab[i] {
			t.Fatalf("spec /operations type enum differs from xdrjson's vocabulary:\nspec  %v\nvocab %v", spec, vocab)
		}
	}
}

package v1

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// TestOpenAPIAccountTierEnumMatchesAuthTier pins Account.tier's documented
// enum to the values /v1/account/me actually serves (string(subject.Tier),
// account.go). The spec previously listed `partner` — not a real auth.Tier
// value — and omitted `sep10` and `operator`, both of which the handler
// serves live (RLT-073 / T527).
func TestOpenAPIAccountTierEnumMatchesAuthTier(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var specPath string
	for i := 0; i < 8; i++ {
		try := filepath.Join(dir, "openapi", "stellar-index.v1.yaml")
		if _, statErr := os.Stat(try); statErr == nil {
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

	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
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
	account, ok := node["Account"].(map[string]any)
	if !ok {
		t.Fatal("spec missing components.schemas.Account")
	}
	props, ok := account["properties"].(map[string]any)
	if !ok {
		t.Fatal("Account has no properties")
	}
	tier, ok := props["tier"].(map[string]any)
	if !ok {
		t.Fatal("Account.properties has no tier")
	}
	rawEnum, ok := tier["enum"].([]any)
	if !ok {
		t.Fatal("Account.tier has no enum")
	}

	var specEnum []string
	for _, v := range rawEnum {
		specEnum = append(specEnum, v.(string))
	}
	sort.Strings(specEnum)

	want := []string{
		string(auth.TierAnonymous),
		string(auth.TierAPIKey),
		string(auth.TierSEP10),
		string(auth.TierOperator),
	}
	sort.Strings(want)

	if strings.Join(specEnum, ",") != strings.Join(want, ",") {
		t.Errorf("openapi Account.tier enum = %v, want %v (every auth.Tier value) — "+
			"account.go serves string(subject.Tier) directly, so an undocumented or "+
			"nonexistent enum value here is a client-visible contract gap", specEnum, want)
	}
}

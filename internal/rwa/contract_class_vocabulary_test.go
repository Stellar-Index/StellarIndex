package rwa_test

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
)

// TestContractVocabularyContainsTheClassicOne — the contract arm may say MORE
// than SEP-1 has words for; it must never say LESS. A term dropped from the
// classic set while a binding still used it would refuse an address that was
// admitted yesterday, silently, on a vocabulary edit made for the other arm.
func TestContractVocabularyContainsTheClassicOne(t *testing.T) {
	contract := map[string]bool{}
	for _, c := range rwa.ContractAnchorClasses() {
		contract[c] = true
	}
	for _, c := range rwa.AnchorClasses() {
		if !contract[c] {
			t.Errorf("classic class %q is not in the contract vocabulary; the contract "+
				"arm is a superset of the classic one, never a different set", c)
		}
	}
}

// TestFundIsNotAcceptedFromASEP1Declaration is the half of the split that
// matters. `fund` exists because the contract arm makes its OWN statement
// from a primary source, reviewed in code. The classic arm READS the issuer's
// free-text anchor_asset_type, where accepting a term SEP-1 does not define
// is accepting an invented spelling — the exact failure the closed set exists
// to prevent, in a population that already carries `equity`, `etf`, `metal`,
// `rwa` and `sovereign`.
func TestFundIsNotAcceptedFromASEP1Declaration(t *testing.T) {
	for _, declared := range []string{"fund", "Fund", " FUND "} {
		if got := rwa.AnchorClass(declared); got != "" {
			t.Errorf("AnchorClass(%q) = %q, want \"\" — an issuer cannot declare its way "+
				"into a class the contract arm reserves for a curated, in-repo statement",
				declared, got)
		}
	}
	if !rwa.ContractInstrumentClass("fund") {
		t.Error("`fund` is not accepted on a curated binding, which is the only place it may appear")
	}
}

// TestContractVocabularyIsExactlyTheClassicOnePlusFund pins the widening to
// what was argued for. A vocabulary that grows by accident grows without an
// argument attached, and the argument is the only thing standing between a
// class and a label chosen to make a number larger.
func TestContractVocabularyIsExactlyTheClassicOnePlusFund(t *testing.T) {
	want := append([]string{"fund"}, rwa.AnchorClasses()...)
	sort.Strings(want)

	got := rwa.ContractAnchorClasses()

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("contract vocabulary = %v, want %v", got, want)
	}
}

// TestTheOvernightSwapFundIsNotFiledAsABond is the specific refusal this
// change was made to avoid having to make. The fund holds 152 listed equities
// at 119% of net assets and swaps every penny of that return away for an
// overnight index rate; `bond` is false on the assets and false on the
// exposure, and it would have been the single most visible number on the
// surface attached to the single most falsifiable false claim.
func TestTheOvernightSwapFundIsNotFiledAsABond(t *testing.T) {
	var seen int
	for _, b := range rwa.ContractInstrumentBindings() {
		if !strings.Contains(b.Instrument, "Overnight Swap Fund") {
			continue
		}
		seen++
		if b.Class != "fund" {
			t.Errorf("%s (%s) is class %q, want \"fund\"", b.ContractID, b.Instrument, b.Class)
		}
	}
	if seen != 4 {
		t.Errorf("found %d overnight-swap-fund bindings, want 4 (one per share class: "+
			"EUR, USD, GBP, CHF)", seen)
	}
}

// TestTheTBillFundsKeepTheClassTheyShippedWith — the widening must not
// reclassify anything already published. The T-Bill funds are invested in
// short-dated sovereign debt, `bond` is defensible for them, and moving them
// would change what an existing row says about an instrument that has not
// changed.
func TestTheTBillFundsKeepTheClassTheyShippedWith(t *testing.T) {
	var seen int
	for _, b := range rwa.ContractInstrumentBindings() {
		if !strings.Contains(b.Instrument, "T-Bills Money Market Fund") {
			continue
		}
		seen++
		if b.Class != "bond" {
			t.Errorf("%s (%s) is class %q, want \"bond\" — it shipped as bond and the "+
				"instrument has not changed", b.ContractID, b.Instrument, b.Class)
		}
	}
	if seen != 5 {
		t.Errorf("found %d T-Bill fund bindings, want 5", seen)
	}
}

// TestOpenAPIAnchorClassEnumMatchesContractVocabulary pins the wire contract
// to the Go vocabulary it serves. `anchor_class` is populated from BOTH the
// classic and contract arms (RLT-026): a term the contract arm may legally
// emit (`fund`) but the spec's enum omits is an undocumented value on every
// Spiko SAFO row, live, with no way for a generated client to model it.
func TestOpenAPIAnchorClassEnumMatchesContractVocabulary(t *testing.T) {
	raw, err := os.ReadFile("../../openapi/stellar-index.v1.yaml")
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
	rwaAsset, ok := node["RWAAsset"].(map[string]any)
	if !ok {
		t.Fatal("spec missing components.schemas.RWAAsset")
	}
	props, ok := rwaAsset["properties"].(map[string]any)
	if !ok {
		t.Fatal("RWAAsset has no properties")
	}
	anchorClass, ok := props["anchor_class"].(map[string]any)
	if !ok {
		t.Fatal("RWAAsset.properties has no anchor_class")
	}
	rawEnum, ok := anchorClass["enum"].([]any)
	if !ok {
		t.Fatal("anchor_class has no enum")
	}

	var specEnum []string
	for _, v := range rawEnum {
		specEnum = append(specEnum, v.(string))
	}
	sort.Strings(specEnum)

	want := rwa.ContractAnchorClasses() // superset the field is actually populated from
	sort.Strings(want)

	if strings.Join(specEnum, ",") != strings.Join(want, ",") {
		t.Errorf("openapi anchor_class enum = %v, want %v (rwa.ContractAnchorClasses) — "+
			"a class the contract arm may legally serve must be documented on the wire",
			specEnum, want)
	}
}

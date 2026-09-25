package supply

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestLowerBoundNamesEveryBasisInTheVocabulary is the guard that matters more
// than the two positive cases. [Basis.LowerBound] has a default arm, so a
// basis added later and never considered falls through it as "complete" —
// silently, and in the direction that publishes a floor as a total. This test
// enumerates the whole vocabulary and fails on any member it does not have an
// explicit expectation for, which forces the decision to be made rather than
// defaulted.
func TestLowerBoundNamesEveryBasisInTheVocabulary(t *testing.T) {
	want := map[Basis]bool{
		// Floors. Blind in different ways: the trustline sum misses
		// holding DOMAINS, the storage sum misses TIME.
		BasisClassicTrustlineSum:     true,
		BasisContractStorageBalances: true,

		// Flow sums. A flow does not know whether the token came to rest
		// in a trustline, a claimable balance, an LP reserve or a
		// SAC-held contract balance, so one sum covers all four. Their
		// failure mode is the opposite of a floor's — an incompletely
		// replayed burn stream over-counts.
		BasisClassicLakeFlows: false,
		BasisSEP41LakeFlows:   false,

		// Observer readings and operator statements. Complete by their
		// own definition; whether that definition is the one a consumer
		// wants is what the basis name is for.
		BasisXLMSDFReserveExclusion:       false,
		BasisXLMSDFReserveExclusionStatic: false,
		BasisXLMTotalOnly:                 false,
		BasisIssuerExclusion:              false,
		BasisAdminExclusion:               false,
		BasisSEP41TotalOnly:               false,
		BasisOverride:                     false,
		BasisSEP1DeclaredMax:              false,

		// Not a reading at all — there is no figure to bound.
		BasisNoMetadata: false,
	}
	for _, b := range allBases() {
		got, ok := want[b]
		if !ok {
			t.Errorf("basis %q has no lower-bound expectation here; decide whether a figure "+
				"on it is a floor and say so, rather than letting the default arm answer", b)
			continue
		}
		if b.LowerBound() != got {
			t.Errorf("%q.LowerBound() = %v, want %v", b, b.LowerBound(), got)
		}
	}
	if n := len(allBases()); n != len(want) {
		t.Errorf("vocabulary has %d bases, expectations cover %d", n, len(want))
	}
}

// TestLowerBoundIsFalseForAnUnknownBasis — a string that is not in the
// vocabulary is not a floor and not a total; it is a value this package did
// not produce. Claiming a bound for it would be a claim about a reading
// nothing here made.
func TestLowerBoundIsFalseForAnUnknownBasis(t *testing.T) {
	if Basis("something_else_entirely").LowerBound() {
		t.Error("an unrecognised basis was reported as a lower bound")
	}
}

// TestAllBasesMatchesTheDeclaredConstants keeps allBases in lockstep with
// the package source: Go has no enum enumeration, so without this a new
// Basis constant left off the list escapes every vocabulary-wide guard.
func TestAllBasesMatchesTheDeclaredConstants(t *testing.T) {
	declared := declaredBasisValues(t)
	listed := make(map[string]bool, len(allBases()))
	for _, b := range allBases() {
		if listed[string(b)] {
			t.Errorf("allBases lists %q twice", b)
		}
		listed[string(b)] = true
		if !declared[string(b)] {
			t.Errorf("allBases lists %q, which no Basis constant declares", b)
		}
	}
	for v := range declared {
		if !listed[v] {
			t.Errorf("Basis constant %q is declared but missing from allBases", v)
		}
	}
}

// declaredBasisValues returns the string value of every `X Basis = "..."`
// constant in this package's non-test sources.
func declaredBasisValues(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Basis" {
				return true
			}
			for i, name := range vs.Names {
				var lit *ast.BasicLit
				if i < len(vs.Values) {
					lit, _ = vs.Values[i].(*ast.BasicLit)
				}
				if lit == nil || lit.Kind != token.STRING {
					t.Fatalf("Basis constant %s is not a string literal; teach this guard its form", name.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				out[v] = true
			}
			return false
		})
	}
	if len(out) == 0 {
		t.Fatal("found no Basis constants; the parser guard is not looking at this package")
	}
	return out
}

// allBases is the vocabulary, listed once;
// TestAllBasesMatchesTheDeclaredConstants fails when it drifts.
func allBases() []Basis {
	return []Basis{
		BasisXLMSDFReserveExclusion,
		BasisXLMSDFReserveExclusionStatic,
		BasisXLMTotalOnly,
		BasisIssuerExclusion,
		BasisAdminExclusion,
		BasisSEP41TotalOnly,
		BasisOverride,
		BasisSEP1DeclaredMax,
		BasisSEP41LakeFlows,
		BasisClassicLakeFlows,
		BasisClassicTrustlineSum,
		BasisContractStorageBalances,
		BasisNoMetadata,
	}
}

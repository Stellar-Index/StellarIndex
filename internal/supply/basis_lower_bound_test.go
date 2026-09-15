package supply

import "testing"

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
		BasisXLMSDFReserveExclusion: false,
		BasisXLMTotalOnly:           false,
		BasisIssuerExclusion:        false,
		BasisAdminExclusion:         false,
		BasisSEP41TotalOnly:         false,
		BasisOverride:               false,
		BasisSEP1DeclaredMax:        false,

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

// allBases is the vocabulary, listed once. It is deliberately hand-written
// rather than reflected: the constants are untyped-string-valued and Go gives
// no enumeration, so the only thing that can force a new one to be considered
// is a list a compiler error points at when the name is wrong.
func allBases() []Basis {
	return []Basis{
		BasisXLMSDFReserveExclusion,
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

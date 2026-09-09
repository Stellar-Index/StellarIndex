package rwa

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestTokenizedInstrumentCodes_ClassifyEveryKnownCode is the gate that
// keeps the classification honest as ADR-0028 grows.
//
// Every allow-listed RWA code must be classified as EXACTLY one of
// "prices one token" or "prices an off-chain quantity". A code added to
// the ADR without a decision here would otherwise get the fail-closed
// default silently, and the next spot-commodity slot would look like an
// oversight rather than a decision.
func TestTokenizedInstrumentCodes_ClassifyEveryKnownCode(t *testing.T) {
	for _, code := range canonical.KnownRWACodes() {
		_, tokenized := tokenizedInstrumentCodes[code]
		_, offChain := offChainReferenceCodes[code]
		switch {
		case tokenized && offChain:
			t.Errorf("%s is classified as BOTH a tokenized instrument and an off-chain reference", code)
		case !tokenized && !offChain:
			t.Errorf("%s is in the ADR-0028 allow-list but classified in neither set — "+
				"decide whether its feed prices one token or an off-chain quantity "+
				"(see internal/rwa/oracle_reference.go)", code)
		}
	}
}

// TestClassifiedCodesAreAllowListed catches the mirror drift: a code
// classified here that ADR-0028 does not recognise at all. Such an entry
// can never match a feed, so it is dead weight that reads as coverage.
func TestClassifiedCodesAreAllowListed(t *testing.T) {
	known := map[string]struct{}{}
	for _, c := range canonical.KnownRWACodes() {
		known[c] = struct{}{}
	}
	for _, set := range []map[string]struct{}{tokenizedInstrumentCodes, offChainReferenceCodes} {
		for c := range set {
			if _, ok := known[c]; !ok {
				t.Errorf("%s is classified here but is not an ADR-0028 code", c)
			}
		}
	}
}

// TestSpotGoldIsNotComparableToAToken pins the case the classification
// exists for. `rwa:XAU` is the FX oracle's spot-gold slot — one troy
// ounce — while a Stellar token coded XAU is a token of unstated size.
// Comparing them would publish a unit conversion as a discount.
func TestSpotGoldIsNotComparableToAToken(t *testing.T) {
	if TokenizedInstrumentCode("XAU") {
		t.Error("XAU (spot gold per troy ounce) must not be comparable to a token's market price")
	}
	if !OffChainReferenceCode("XAU") {
		t.Error("XAU must be reported as an off-chain reference so the refusal has a stated reason")
	}
	// The Matrixdock token of the same metal IS a token, and its own
	// code stays comparable — the exclusion is per instrument, not per
	// commodity.
	if !TokenizedInstrumentCode("XAUm") {
		t.Error("XAUm is a tokenized instrument and must stay comparable")
	}
}

// TestTokenizedInstrumentCode_FoldsCase mirrors the membership arm: an
// on-chain asset code carries whatever case its issuer chose, while the
// allow-list spells instrument tickers.
func TestTokenizedInstrumentCode_FoldsCase(t *testing.T) {
	for _, code := range []string{"dejaaa", "DEJAAA", "deJAAA", " deJAAA "} {
		if !TokenizedInstrumentCode(code) {
			t.Errorf("TokenizedInstrumentCode(%q) = false, want true", code)
		}
	}
	if TokenizedInstrumentCode("NOTANRWA") {
		t.Error("an unrecognised code must not be comparable")
	}
	if TokenizedInstrumentCode("") {
		t.Error("the empty code must not be comparable")
	}
}

// TestTokenizedInstrumentCodes_Served checks the vocabulary is
// enumerable and stable, so the API can publish the rule alongside the
// rows instead of a consumer inferring it.
func TestTokenizedInstrumentCodes_Served(t *testing.T) {
	got := TokenizedInstrumentCodes()
	if len(got) != len(tokenizedInstrumentCodes) {
		t.Fatalf("TokenizedInstrumentCodes() returned %d of %d", len(got), len(tokenizedInstrumentCodes))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("TokenizedInstrumentCodes() not sorted: %v", got)
		}
	}
}

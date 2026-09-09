package rwa

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestInstrumentFeed_BindsThePairNotTheCode is the identity rule.
//
// Asset codes are not unique on Stellar. A binding that answered on the
// code would hand every account issuing a token called USTRY the real
// instrument's net asset value — a false financial claim about a real
// security, made about a token the oracle has never heard of.
func TestInstrumentFeed_BindsThePairNotTheCode(t *testing.T) {
	const impostor = "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"

	if feed, ok := InstrumentFeed("USTRY", etherfuseIssuer); !ok || feed != "USTRY" {
		t.Fatalf("InstrumentFeed(USTRY, bound issuer) = %q/%v, want USTRY/true", feed, ok)
	}
	if feed, ok := InstrumentFeed("USTRY", impostor); ok {
		t.Errorf("InstrumentFeed(USTRY, unrelated issuer) = %q/true — the code is not the identity", feed)
	}
	if _, ok := InstrumentFeed("USTRY", ""); ok {
		t.Error("an empty issuer must bind nothing")
	}
	if _, ok := InstrumentFeed("", etherfuseIssuer); ok {
		t.Error("an empty code must bind nothing")
	}
}

// TestInstrumentFeed_DoesNotFoldCase. The previous code-keyed join
// upper-cased both sides, so a token coded XAUM matched the XAUm feed. A
// case variant is a different token unless a binding says otherwise, and
// saying otherwise is what the table is for.
func TestInstrumentFeed_DoesNotFoldCase(t *testing.T) {
	for _, code := range []string{"ustry", "Ustry", "uSTRY"} {
		if feed, ok := InstrumentFeed(code, etherfuseIssuer); ok {
			t.Errorf("InstrumentFeed(%q, bound issuer) = %q/true — case variants are distinct tokens", code, feed)
		}
	}
	// Whitespace is never part of an identity, so it is trimmed.
	if _, ok := InstrumentFeed("  USTRY  ", " "+etherfuseIssuer+" "); !ok {
		t.Error("surrounding whitespace must not defeat a binding")
	}
}

// TestNoBindingTargetsAnOffChainReference. A feed that prices a troy
// ounce of spot metal or one share of a fund is measuring a different
// quantity from a token's price; binding one would publish a unit
// conversion as a premium.
func TestNoBindingTargetsAnOffChainReference(t *testing.T) {
	for _, b := range instrumentBindings {
		if OffChainReferenceCode(b.Feed) {
			t.Errorf("%s-%s is bound to %s, which prices an off-chain quantity in its own unit, "+
				"not one token", b.Code, b.Issuer, b.Feed)
		}
	}
}

// TestBindingsTargetAllowListedFeeds. A binding whose feed ADR-0028 does
// not recognise can never match a stored oracle row — it is dead weight
// that reads as coverage.
func TestBindingsTargetAllowListedFeeds(t *testing.T) {
	known := map[string]struct{}{}
	for _, c := range canonical.KnownRWACodes() {
		known[c] = struct{}{}
	}
	for _, b := range instrumentBindings {
		if _, ok := known[b.Feed]; !ok {
			t.Errorf("%s-%s binds feed %q, which is not an ADR-0028 code", b.Code, b.Issuer, b.Feed)
		}
	}
	for c := range offChainReferenceCodes {
		if _, ok := known[c]; !ok {
			t.Errorf("%s is classified as an off-chain reference but is not an ADR-0028 code", c)
		}
	}
}

// TestBindingsAreWellFormed. Every entry names a real Stellar issuing
// account and a non-empty code, and no pair is bound twice — a duplicate
// pair would make the served set disagree with the lookup index.
func TestBindingsAreWellFormed(t *testing.T) {
	seen := map[instrumentKey]struct{}{}
	for _, b := range instrumentBindings {
		if strings.TrimSpace(b.Code) == "" {
			t.Errorf("binding with an empty code: %+v", b)
		}
		if len(b.Issuer) != 56 || !strings.HasPrefix(b.Issuer, "G") {
			t.Errorf("binding issuer %q is not a Stellar account G-strkey", b.Issuer)
		}
		k := instrumentKey{code: b.Code, issuer: b.Issuer}
		if _, dup := seen[k]; dup {
			t.Errorf("%s-%s is bound more than once", b.Code, b.Issuer)
		}
		seen[k] = struct{}{}
	}
	if len(bindingIndex) != len(instrumentBindings) {
		t.Errorf("lookup index holds %d of %d bindings", len(bindingIndex), len(instrumentBindings))
	}
}

// TestInstrumentBindings_Served — the curated set is published so a
// consumer can audit every pair this surface will compare, rather than
// inferring the rule from whichever rows carry a figure today. Feeds are
// served in canonical `rwa:` form so the id goes straight to the oracle
// endpoints.
func TestInstrumentBindings_Served(t *testing.T) {
	got := InstrumentBindings()
	if len(got) != len(instrumentBindings) {
		t.Fatalf("InstrumentBindings() returned %d of %d", len(got), len(instrumentBindings))
	}
	for i, b := range got {
		if !strings.HasPrefix(b.Feed, "rwa:") {
			t.Errorf("feed %q is not a canonical asset id", b.Feed)
		}
		if _, err := canonical.ParseAsset(b.Feed); err != nil {
			t.Errorf("served feed %q does not parse: %v", b.Feed, err)
		}
		if i > 0 && got[i-1].Code > b.Code {
			t.Fatalf("InstrumentBindings() not ordered: %v", got)
		}
	}
}

// TestSpotGoldIsNotComparableToAToken pins the case the off-chain
// classification exists for. `rwa:XAU` is the FX oracle's spot-gold slot
// — one troy ounce — while a Stellar token coded XAU is a token of
// unstated size.
func TestSpotGoldIsNotComparableToAToken(t *testing.T) {
	if !OffChainReferenceCode("XAU") {
		t.Error("XAU must be reported as an off-chain reference so the refusal has a stated reason")
	}
	if OffChainReferenceCode("XAUm") {
		t.Error("XAUm is a tokenized instrument, not an off-chain reference")
	}
	if OffChainReferenceCode("NOTANRWA") || OffChainReferenceCode("") {
		t.Error("an unrecognised code is not an off-chain reference")
	}
}

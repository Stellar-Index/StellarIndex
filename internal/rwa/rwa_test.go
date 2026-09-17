package rwa

import (
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAnchorClass_ClosedVocabulary — only the four SEP-1 terms naming a
// real-world instrument normalise; every invented synonym present in the
// production payload set stays out. Accepting synonyms is how a closed
// set stops being closed, and the terms below were all measured on the
// live directory 2026-09-05.
func TestAnchorClass_ClosedVocabulary(t *testing.T) {
	for _, in := range []string{"stock", "BOND", " commodity ", "RealEstate"} {
		if AnchorClass(in) == "" {
			t.Errorf("AnchorClass(%q) = \"\", want a class", in)
		}
	}
	for _, in := range []string{
		"fiat", "crypto", "nft", "other", "",
		"equity", "etf", "fund", "index", "metal", "rwa", "real_estate", "sovereign", "security",
	} {
		if got := AnchorClass(in); got != "" {
			t.Errorf("AnchorClass(%q) = %q, want \"\"", in, got)
		}
	}
}

// TestAnchorClass_ExcludesFiat pins the one exclusion a reader is most
// likely to want to undo. A fiat-anchored token is a stablecoin, a
// different instrument counted on its own surface; folding it in would
// silently multiply the headline figure.
func TestAnchorClass_ExcludesFiat(t *testing.T) {
	if AnchorClass("fiat") != "" {
		t.Error("fiat normalised to a real-world class — stablecoins are not RWAs on this surface")
	}
	if slices.Contains(AnchorClasses(), "fiat") {
		t.Errorf("AnchorClasses() = %v, must not advertise fiat", AnchorClasses())
	}
}

// TestScamFlagged_ReadsTheOneVocabulary — the scam vocabulary is not
// restated here; it is read from the single list the price-withholding
// gate and the listing rank expression also read. A tag that withholds
// a price must also keep an issuer out of this set.
func TestScamFlagged_ReadsTheOneVocabulary(t *testing.T) {
	for _, tag := range timescale.DirectoryScamFlagTags {
		if !ScamFlagged([]string{tag}) {
			t.Errorf("ScamFlagged([%q]) = false — the vocabularies have drifted apart", tag)
		}
		if !ScamFlagged([]string{" " + tag + " "}) {
			t.Errorf("ScamFlagged did not trim %q", tag)
		}
	}
	if ScamFlagged([]string{"issuer", "anchor"}) {
		t.Error("a recognition tag was read as scam-class")
	}
}

// TestQualify_RequiresEveryRequirement walks the four requirements one
// at a time: from an admitted candidate, breaking each requirement on
// its own must refuse it, with the reason naming that requirement.
func TestQualify_RequiresEveryRequirement(t *testing.T) {
	good := Candidate{
		Code:               "USTRY",
		Issuer:             "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		BoundSep1:          true,
		DeclaredAnchorType: "bond",
		DirectoryTags:      []string{"issuer"},
	}
	if v := Qualify(good); !v.InSet || v.Basis != BasisSep1Anchor || v.AnchorClass != "bond" {
		t.Fatalf("baseline candidate refused: %+v", v)
	}

	tests := []struct {
		name   string
		mutate func(Candidate) Candidate
		want   string
	}{
		{"no issuer", func(c Candidate) Candidate { c.Issuer = ""; return c }, RejectNotClassic},
		{"no code", func(c Candidate) Candidate { c.Code = ""; return c }, RejectNotClassic},
		{"no bound sep1 entry", func(c Candidate) Candidate { c.BoundSep1 = false; return c }, RejectNoBoundSep1},
		{"scam-flagged issuer", func(c Candidate) Candidate {
			c.DirectoryTags = []string{"issuer", "malicious"}
			return c
		}, RejectScamFlagged},
		{"issuer not in the directory", func(c Candidate) Candidate { c.DirectoryTags = nil; return c }, RejectNoRecognition},
		{"issuer tagged but not recognised as one", func(c Candidate) Candidate {
			c.DirectoryTags = []string{"personal", "memo-required"}
			return c
		}, RejectNoRecognition},
		{"declares no real-world instrument", func(c Candidate) Candidate {
			c.DeclaredAnchorType = "crypto"
			c.Code = "MEME"
			return c
		}, RejectNoInstrumentClaim},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := Qualify(tc.mutate(good))
			if v.InSet {
				t.Fatalf("candidate admitted with %s", tc.name)
			}
			if v.Reject != tc.want {
				t.Errorf("reject = %q, want %q", v.Reject, tc.want)
			}
		})
	}
}

// TestQualify_ScamTagBeatsRecognition — an account carrying both is
// refused as flagged. Reporting it as merely unrecognised would
// understate what is known about it.
func TestQualify_ScamTagBeatsRecognition(t *testing.T) {
	v := Qualify(Candidate{
		Code: "USTRY", Issuer: "G1", BoundSep1: true, DeclaredAnchorType: "bond",
		DirectoryTags: []string{"issuer", "anchor", "unsafe"},
	})
	if v.InSet || v.Reject != RejectScamFlagged {
		t.Errorf("verdict = %+v, want refused as %s", v, RejectScamFlagged)
	}
}

// TestQualify_OracleBasisNeverAdmitsOnCodeAlone is the anti-phishing
// invariant in its sharpest form. USTRY, BENJI and XAU are all codes an
// RWA oracle prices, and all three are issued on the live network by
// accounts the directory flags. The oracle basis must not carry any of
// them past requirement 3.
func TestQualify_OracleBasisNeverAdmitsOnCodeAlone(t *testing.T) {
	for _, code := range []string{"USTRY", "BENJI", "XAU", "xaum", "deJTRSY"} {
		unrecognised := Qualify(Candidate{Code: code, Issuer: "G1", BoundSep1: true})
		if unrecognised.InSet {
			t.Errorf("%s admitted from an unrecognised issuer on the code alone", code)
		}
		flagged := Qualify(Candidate{
			Code: code, Issuer: "G1", BoundSep1: true,
			DirectoryTags: []string{"issuer", "malicious"},
		})
		if flagged.InSet {
			t.Errorf("%s admitted from a scam-flagged issuer", code)
		}
		ok := Qualify(Candidate{
			Code: code, Issuer: "G1", BoundSep1: true, DirectoryTags: []string{"issuer"},
		})
		if !ok.InSet || ok.Basis != BasisOracleFeed {
			t.Errorf("%s from a recognised issuer: %+v, want the oracle basis", code, ok)
		}
		if ok.AnchorClass != "" {
			t.Errorf("%s got anchor_class %q — an oracle feed names an instrument, not a class",
				code, ok.AnchorClass)
		}
	}
}

// TestCouldQualify_MatchesQualifyOnTheAssetSideInputs — the storage
// pre-filter must never drop an entry [Qualify] would have admitted.
// A pre-filter that is narrower than the rule it precedes is a silent
// membership change with no test to catch it.
//
// The matrix spans EVERY asset-side input a requirement-4 arm reads:
// code (oracle arm), anchor type (class arm) and anchor asset (ISIN
// arm). The third dimension was missing when the ISIN arm shipped, so
// the guard passed while the pre-filter dropped the three Franklin
// share classes the arm exists for.
func TestCouldQualify_MatchesQualifyOnTheAssetSideInputs(t *testing.T) {
	codes := []string{"USTRY", "BENJI", "XAU", "XAUm", "MEME", "USDC", ""}
	types := []string{"bond", "stock", "commodity", "realestate", "fiat", "crypto", "nft", "other", ""}
	anchors := []string{
		"LU2900381208", // gBENJI, valid ISIN
		"SGXZ71843866", // sgBENJI, valid ISIN
		"US0378331005", // Apple, valid ISIN
		"LU2900381209", // wrong check digit
		"FOBXX",        // a ticker, not an ISIN
		"",
	}
	for _, code := range codes {
		for _, typ := range types {
			for _, anchor := range anchors {
				admitted := Qualify(Candidate{
					Code: code, Issuer: "G1", BoundSep1: true,
					DeclaredAnchorType:  typ,
					DeclaredAnchorAsset: anchor,
					DirectoryTags:       []string{"issuer"},
				}).InSet
				if admitted && !CouldQualify(code, typ, anchor) {
					t.Errorf("CouldQualify(%q, %q, %q) = false but Qualify admits it", code, typ, anchor)
				}
			}
		}
	}
}

// TestCouldQualify_KeepsAnISINDeclaredEntryWithNoClass pins the exact
// shape that was dropped: a code no oracle prices, type `other`, and a
// well-formed ISIN in anchor_asset. The matrix above covers it too;
// this names it so a future narrowing fails on a line that says why.
func TestCouldQualify_KeepsAnISINDeclaredEntryWithNoClass(t *testing.T) {
	for _, c := range []struct{ code, isin string }{
		{"gBENJI", "LU2900381208"},
		{"grBENJI", "LU3258450587"},
		{"sgBENJI", "SGXZ71843866"},
	} {
		if !CouldQualify(c.code, "other", c.isin) {
			t.Errorf("CouldQualify(%q, other, %s) = false: the ISIN arm is unreachable through the pre-filter", c.code, c.isin)
		}
	}
	if CouldQualify("gBENJI", "other", "LU2900381209") {
		t.Error("a malformed ISIN (bad check digit) must not pass the pre-filter on the ISIN arm")
	}
	if CouldQualify("MEME", "other", "") {
		t.Error("type other with no anchor asset and no oracle code must still be filtered")
	}
}

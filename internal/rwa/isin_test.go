// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package rwa

import "testing"

// The two ISINs Franklin Templeton declares for its Stellar share
// classes, plus published ISINs whose check digits are documented, plus
// the shapes an `anchor_asset` field actually contains when an issuer
// fills it in with prose.
//
// Every positive fixture here had its check digit reproduced by a
// separate ISO 6166 implementation before it was written down — one of
// them was wrong on the first pass and the test caught it, which is the
// only reason to say so. A fixture whose expected value is taken from
// the implementation under test asserts nothing.
func TestIsISIN(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
		why  string
	}{
		// Live declarations, read from Franklin Templeton's SEP-1 on
		// 2026-09-15. These are the assets this arm exists to admit.
		{"LU2900381208", true, "gBENJI's declared anchor_asset"},
		{"LU3258450587", true, "grBENJI's declared anchor_asset"},
		// Published ISINs with known-good check digits.
		{"US0378331005", true, "a US equity"},
		{"GB0002634946", true, "a GB security"},
		{"XS1748480172", true, "a supranational prefix is a valid country code"},
		// Lower case is accepted: an issuer's own file is not normalised.
		{"lu2900381208", true, "case is the declarer's, not the standard's"},
		{"  LU2900381208  ", true, "surrounding whitespace is the file's, not the value's"},

		// The whole point of checking the digit: one transposition and
		// it is a different security or no security at all.
		{"LU2900381209", false, "check digit off by one"},
		{"LU2900318208", false, "two body digits transposed"},
		{"US0378331004", false, "check digit off by one"},

		// What anchor_asset contains when it is not an identifier. Every
		// one of these is a real shape from the wild.
		{"", false, "declared nothing"},
		{"FOBXX", false, "a fund ticker, not an ISIN"},
		{"US Treasury Notes", false, "prose"},
		{"gold", false, "prose"},
		{"LU29003812", false, "too short"},
		{"LU29003812080", false, "too long"},
		{"1U2900381208", false, "first prefix character is not a letter"},
		{"L12900381208", false, "second prefix character is not a letter"},
		{"LU290038120X", false, "check position is not a digit"},
		{"LU-900381208", false, "non-alphanumeric in the body"},
		// Unicode case folding must not manufacture an ISIN: these
		// upper-case to US0378331005 and IE00B4L5Y983, but the served
		// anchor_asset would be the non-ASCII string nobody can look up.
		{"Uſ0378331005", false, "long s (U+017F) is not the letter S"},
		{"ıE00B4L5Y983", false, "dotless i (U+0131) is not the letter I"},
		{"IE00B4L5Y983", true, "the ASCII original of the case above"},
		{"DE0007164600", true, "check digit 0: the mod-10 complement wraps to 0, not 10"},
		{"au000000bhp4", true, "a lower-case 'a' folds like every other letter"},
		// Luhn-valid bodies, so only the prefix rule can refuse them.
		{"L12900381204", false, "second prefix character is a digit, check digit valid"},
		{"1U2900381202", false, "first prefix character is a digit, check digit valid"},
	} {
		if got := IsISIN(tc.in); got != tc.want {
			t.Errorf("IsISIN(%q) = %v, want %v — %s", tc.in, got, tc.want, tc.why)
		}
	}
}

// The arm admits, and it admits WITHOUT a class. Publishing one would
// state a classification the issuer did not make.
func TestISINArmAdmitsWithoutInventingAClass(t *testing.T) {
	v := Qualify(Candidate{
		Code: "gBENJI", Issuer: "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP",
		BoundSep1:           true,
		DeclaredAnchorType:  "other",
		DeclaredAnchorAsset: "LU2900381208",
		DirectoryTags:       []string{"issuer"},
	})
	if !v.InSet {
		t.Fatalf("refused: %s", v.Reject)
	}
	if v.Basis != BasisSep1ISIN {
		t.Errorf("basis = %q, want %q", v.Basis, BasisSep1ISIN)
	}
	if v.AnchorClass != "" {
		t.Errorf("anchor_class = %q, want empty — an ISIN names an instrument, not a class", v.AnchorClass)
	}
}

// A declared class still wins: it carries strictly more information, so
// an asset that can be admitted with one must not fall through to the
// arm that cannot say what it is.
func TestDeclaredClassOutranksTheISINArm(t *testing.T) {
	v := Qualify(Candidate{
		Code: "USTRY", Issuer: "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		BoundSep1:           true,
		DeclaredAnchorType:  "bond",
		DeclaredAnchorAsset: "LU2900381208",
		DirectoryTags:       []string{"issuer"},
	})
	if v.Basis != BasisSep1Anchor {
		t.Errorf("basis = %q, want %q — a declared class is more informative", v.Basis, BasisSep1Anchor)
	}
	if v.AnchorClass != "bond" {
		t.Errorf("anchor_class = %q, want bond", v.AnchorClass)
	}
}

// THE GUARD. The arm relaxes R4, not R2 or R3. An ISIN is the issuer's
// own claim and is checked for form only, so it must never carry a
// candidate past the two requirements that bind the identity — which is
// the whole reason it is safe to accept a self-declared value here.
func TestISINNeverBypassesBindingOrRecognition(t *testing.T) {
	base := Candidate{
		Code: "gBENJI", Issuer: "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP",
		BoundSep1:           true,
		DeclaredAnchorAsset: "LU2900381208",
		DirectoryTags:       []string{"issuer"},
	}
	unbound := base
	unbound.BoundSep1 = false
	if v := Qualify(unbound); v.InSet || v.Reject != RejectNoBoundSep1 {
		t.Errorf("unbound SEP-1: got in_set=%v reject=%q, want refusal at R2", v.InSet, v.Reject)
	}

	unrecognised := base
	unrecognised.DirectoryTags = nil
	if v := Qualify(unrecognised); v.InSet || v.Reject != RejectNoRecognition {
		t.Errorf("unrecognised issuer: got in_set=%v reject=%q, want refusal at R3", v.InSet, v.Reject)
	}

	flagged := base
	flagged.DirectoryTags = []string{"issuer", "scam"}
	if v := Qualify(flagged); v.InSet || v.Reject != RejectScamFlagged {
		t.Errorf("scam-flagged issuer: got in_set=%v reject=%q, want refusal as flagged", v.InSet, v.Reject)
	}
}

// Prose in anchor_asset is the ordinary case and must still be refused,
// or the arm would admit every recognised issuer that filled the field
// in at all.
func TestProseAnchorAssetIsStillRefused(t *testing.T) {
	v := Qualify(Candidate{
		Code: "AUMTL", Issuer: "GACKTN5DAZGWXRWB2WLM6OPBDHAMT6SJNGLJZPQMEZBUX2UDX2UK7VXX",
		BoundSep1:           true,
		DeclaredAnchorType:  "other",
		DeclaredAnchorAsset: "AU",
		DirectoryTags:       []string{"issuer"},
	})
	if v.InSet {
		t.Fatalf("admitted on prose anchor_asset %q with basis %q", "AU", v.Basis)
	}
	if v.Reject != RejectNoInstrumentClaim {
		t.Errorf("reject = %q, want %q", v.Reject, RejectNoInstrumentClaim)
	}
}

// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package rwa_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
)

// referenceISIN is an independent ISO 6166 check: the body is expanded
// to a decimal STRING (letters as their two-digit ordinal) and the
// textbook Luhn runs over that string with the check digit included, so
// a valid ISIN sums to 0 mod 10. It shares no code shape with [rwa.IsISIN].
func referenceISIN(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 12 {
		return false
	}
	var expanded strings.Builder
	for i := range len(s) {
		ch := s[i]
		if ch >= 'a' && ch <= 'z' {
			ch -= 'a' - 'A'
		}
		switch {
		case i < 2 && (ch < 'A' || ch > 'Z'):
			return false
		case i == 11 && (ch < '0' || ch > '9'):
			return false
		case ch >= '0' && ch <= '9':
			expanded.WriteByte(ch)
		case ch >= 'A' && ch <= 'Z':
			expanded.WriteString(strconv.Itoa(int(ch-'A') + 10))
		default:
			return false
		}
	}
	d := expanded.String()
	sum := 0
	for i := range len(d) {
		n := int(d[len(d)-1-i] - '0')
		if i%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

func FuzzIsISIN(f *testing.F) {
	for _, s := range []string{
		"LU2900381208", "LU3258450587", "US0378331005", "GB0002634946",
		"XS1748480172", "lu2900381208", "  LU2900381208  ", "LU2900381209",
		"LU290038120X", "LU-900381208", "", "FOBXX", "US Treasury Notes",
		// Non-ASCII letters whose Unicode upper case is ASCII: the long s
		// and the dotless i. Neither is an ISO 6166 character.
		"Uſ0378331005", "Uſ037833100", "ıE00B4L5Y983",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := rwa.IsISIN(s)
		if want := referenceISIN(s); got != want {
			t.Fatalf("IsISIN(%q) = %v, reference = %v", s, got, want)
		}
		if !got {
			return
		}
		// A well-formed ISIN is 12 ASCII alphanumerics once trimmed: it
		// is served verbatim as the identifier a reader looks up.
		body := strings.TrimSpace(s)
		if len(body) != 12 {
			t.Fatalf("IsISIN(%q) accepted a %d-byte body", s, len(body))
		}
		for i := range len(body) {
			c := body[i]
			if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
				t.Fatalf("IsISIN(%q) accepted non-alphanumeric byte %#x at %d", s, c, i)
			}
		}
		// The check digit is unique: every other digit in its place fails.
		for d := byte('0'); d <= '9'; d++ {
			if d == body[11] {
				continue
			}
			alt := body[:11] + string(d)
			if rwa.IsISIN(alt) {
				t.Fatalf("IsISIN accepted both %q and %q", body, alt)
			}
		}
	})
}

// FuzzQualify pins the structural properties of the classic verdict that
// no fixture enumerates: the pre-filter is never narrower than the rule,
// a scam tag or a missing SEP-1 binding never admits, and an admitted row
// carries exactly the basis and class its inputs justify.
func FuzzQualify(f *testing.F) {
	const issuer = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
	f.Add("gBENJI", issuer, true, "other", "LU2900381208", "issuer", "", false)
	f.Add("BENJI", issuer, true, "fiat", "", "anchor", "", false)
	f.Add("XAUm", issuer, true, "", "", "", "", true)
	f.Add("USDC", issuer, true, "stock", "", "issuer", "scam", false)
	f.Add("GOLD", issuer, false, "commodity", "", "issuer", "", false)
	f.Add(" ", issuer, true, "bond", "", "issuer", "", false)
	f.Fuzz(func(t *testing.T, code, iss string, bound bool, anchorType, anchorAsset, tag1, tag2 string, sibling bool) {
		c := rwa.Candidate{
			Code: code, Issuer: iss, BoundSep1: bound,
			DeclaredAnchorType: anchorType, DeclaredAnchorAsset: anchorAsset,
			DirectoryTags: []string{tag1, tag2}, SiblingRecognised: sibling,
		}
		v := rwa.Qualify(c)
		if v.InSet == (v.Reject != "") {
			t.Fatalf("verdict must be exactly one of in-set or rejected: %+v", v)
		}
		if !v.InSet {
			return
		}
		if !rwa.CouldQualify(code, anchorType, anchorAsset) {
			t.Fatalf("pre-filter refused a member: %+v -> %+v", c, v)
		}
		if strings.TrimSpace(code) == "" || strings.TrimSpace(iss) == "" || !bound {
			t.Fatalf("admitted without a bound classic identity: %+v", c)
		}
		if rwa.ScamFlagged(c.DirectoryTags) {
			t.Fatalf("admitted a scam-flagged issuer: %+v", c)
		}
		direct := rwa.HasRecognitionTag(c.DirectoryTags)
		switch {
		case direct && v.Recognition != rwa.RecognitionDirectory,
			!direct && (!sibling || v.Recognition != rwa.RecognitionDomainSibling):
			t.Fatalf("recognition %q does not follow tags=%q sibling=%v", v.Recognition, c.DirectoryTags, sibling)
		}
		class := rwa.AnchorClass(anchorType)
		switch v.Basis {
		case rwa.BasisSep1Anchor:
			if class == "" || v.AnchorClass != class {
				t.Fatalf("sep1_anchor with class %q for declared %q", v.AnchorClass, anchorType)
			}
		case rwa.BasisOracleFeed:
			if class != "" || v.AnchorClass != "" {
				t.Fatalf("oracle arm won over a declared class %q", class)
			}
		case rwa.BasisSep1ISIN:
			if class != "" || v.AnchorClass != "" || !rwa.IsISIN(anchorAsset) {
				t.Fatalf("isin arm on class=%q asset=%q", class, anchorAsset)
			}
		default:
			t.Fatalf("unknown basis %q", v.Basis)
		}
	})
}

// FuzzQualifyContract pins the contract verdict's gates: identity is a
// CRC-checked C-strkey, the scam tag beats every arm, the pre-filter is
// never narrower than the rule, and the listing arm alone admits nothing.
func FuzzQualifyContract(f *testing.F) {
	bound := rwa.ContractInstrumentBindings()
	if len(bound) == 0 {
		f.Fatal("no curated contract bindings to seed from")
	}
	id := bound[0].ContractID
	f.Add(id, true, "issuer", "", true, true, "")
	f.Add(id, false, "", "", true, true, "")
	f.Add(id, false, "", "", false, true, "")
	f.Add(id, true, "issuer", "phishing", true, true, "")
	f.Add("CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA", true, "issuer", "", false, false, "XAUm")
	f.Add("CNOTACONTRACT", true, "issuer", "", true, true, "BENJI")
	f.Fuzz(func(t *testing.T, contractID string, named bool, tag1, tag2 string, listingNamed, listingAvail bool, symbol string) {
		c := rwa.ContractCandidate{
			ContractID: contractID, DirectoryNamed: named, DirectoryTags: []string{tag1, tag2},
			ListingNamed: listingNamed, ListingAvailable: listingAvail, Symbol: symbol,
		}
		v := rwa.QualifyContract(c)
		if v.InSet == (v.Reject != "") {
			t.Fatalf("verdict must be exactly one of in-set or rejected: %+v", v)
		}
		if !v.InSet {
			return
		}
		if !canonical.IsContractID(strings.TrimSpace(contractID)) {
			t.Fatalf("admitted a non-contract identity %q", contractID)
		}
		if rwa.ScamFlagged(c.DirectoryTags) {
			t.Fatalf("admitted a scam-flagged contract: %+v", c)
		}
		if !rwa.CouldQualifyContract(contractID, symbol) {
			t.Fatalf("pre-filter refused a member: %+v", c)
		}
		if !named && !(listingAvail && listingNamed) {
			t.Fatalf("admitted with nobody naming the address: %+v -> %+v", c, v)
		}
		if v.Basis == rwa.BasisContractOracleFeed && v.AnchorClass != "" {
			t.Fatalf("oracle arm invented class %q", v.AnchorClass)
		}
		if v.Basis == rwa.BasisCuratedContract && !rwa.ContractInstrumentClass(v.AnchorClass) {
			t.Fatalf("curated arm served class %q outside the vocabulary", v.AnchorClass)
		}
	})
}

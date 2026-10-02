// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package rwa

import "strings"

// IsISIN reports whether s is a well-formed ISO 6166 ISIN, check digit
// included.
//
// Form is the whole of what this validates, and the distinction is the
// point: a well-formed ISIN is not a claim that the security exists,
// that it is the one named, or that the declaring issuer is entitled to
// it. What it rules out is a free-text string wearing the shape of an
// identifier — which is exactly what `anchor_asset` is when an issuer
// fills it in with a product name.
//
// Two properties make it worth checking at all:
//
//   - The country prefix and the check digit are assigned by a national
//     numbering agency, not chosen by the declarer. A typo fails; an
//     invented one fails with probability 9 in 10.
//   - It is externally resolvable. A reader handed LU2900381208 can look
//     it up; a reader handed "Franklin OnChain Euro Government Money
//     Fund" cannot check anything.
//
// The check digit is Luhn over the digit expansion of the alphanumeric
// body (A=10 … Z=35), per ISO 6166.
func IsISIN(s string) bool {
	s, ok := upperASCII12(s)
	if !ok {
		return false
	}
	// Prefix: two letters. 'XS' and other supranational prefixes are
	// valid country codes here; this deliberately does not check the
	// code against a list, because the ISO 3166 set moves and a new
	// entry must not silently refuse a real security.
	for i := range 2 {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	// Body + check digit: alphanumeric, and the last character must be a
	// digit.
	if s[11] < '0' || s[11] > '9' {
		return false
	}
	// Expand to digits: letters become their two-digit ordinal.
	digits := make([]int, 0, 24)
	for i := range 12 {
		switch ch := s[i]; {
		case ch >= '0' && ch <= '9':
			digits = append(digits, int(ch-'0'))
		case ch >= 'A' && ch <= 'Z':
			v := int(ch-'A') + 10
			digits = append(digits, v/10, v%10)
		default:
			return false
		}
	}
	// Luhn from the right, doubling every second digit. The check digit
	// itself is position 0 from the right and is never doubled.
	sum := 0
	double := true
	for i := len(digits) - 2; i >= 0; i-- {
		d := digits[i]
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return (10-sum%10)%10 == digits[len(digits)-1]
}

// ISINClaim is one admitted (code, issuer) and the anchor_asset its
// issuer-bound SEP-1 entry declares, verbatim.
type ISINClaim struct {
	Code                string
	Issuer              string
	DeclaredAnchorAsset string
}

// ContestedISINClaims reports, index for index, which claims name an
// ISIN that another ISSUER among claims also declares.
//
// An ISIN identifies one registered security, so two issuers declaring
// it are two tokens wearing one identity, and both have passed R2 and
// R3 — nothing in the evidence says which is entitled to it. Every
// claimant is refused, except the pair the constant-NAV table binds to
// that ISIN: the table was read against the security's own page, so it
// names the holder. One issuer declaring the same ISIN on several codes
// is not contested here. Pass only admitted candidates: a claimant that
// could not pass R3 itself must not be able to evict one that did.
func ContestedISINClaims(claims []ISINClaim) []bool {
	isins := make([]string, len(claims))
	issuers := map[string]map[string]struct{}{}
	for i, c := range claims {
		isin, ok := upperASCII12(c.DeclaredAnchorAsset)
		if !ok || !IsISIN(isin) {
			continue
		}
		isins[i] = isin
		if issuers[isin] == nil {
			issuers[isin] = map[string]struct{}{}
		}
		issuers[isin][strings.TrimSpace(c.Issuer)] = struct{}{}
	}
	out := make([]bool, len(claims))
	for i, c := range claims {
		isin := isins[i]
		if isin == "" || len(issuers[isin]) < 2 {
			continue
		}
		b, bound := constantNAVIndex.byISIN[isin]
		holder := bound && b.Code == strings.TrimSpace(c.Code) && b.Issuer == strings.TrimSpace(c.Issuer)
		out[i] = !holder
	}
	return out
}

// upperASCII12 trims s and upper-cases ASCII letters only, reporting
// whether 12 bytes remain. strings.ToUpper would fold U+017F and U+0131
// onto 'S' and 'I', accepting a string no ISIN lookup resolves.
func upperASCII12(s string) (string, bool) {
	b := []byte(strings.TrimSpace(s))
	if len(b) != 12 {
		return "", false
	}
	for i, ch := range b {
		if ch >= 'a' && ch <= 'z' {
			b[i] = ch - ('a' - 'A')
		}
	}
	return string(b), true
}

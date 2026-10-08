// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package rwa

import "strings"

// IsISIN reports whether s is a well-formed ISO 6166 ISIN (Luhn check digit over the
// A=10…Z=35 expansion). Form only, not existence: it rules out a product name posing as an
// identifier, and a real ISIN is agency-assigned and externally resolvable.
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

// CanonicalISIN returns s in the trimmed, upper-case form every ISIN
// comparison keys on, and whether it is a well-formed ISIN at all.
func CanonicalISIN(s string) (string, bool) {
	u, ok := upperASCII12(s)
	if !ok || !IsISIN(u) {
		return "", false
	}
	return u, true
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

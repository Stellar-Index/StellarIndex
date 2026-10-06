// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import "testing"

const prefixTestIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestParseAsset_PrefixWordClassicCodes pins that a classic token whose
// code is a prefix word parses as that classic asset in both its `-` and
// `:` forms, and that the prefixed off-chain forms still parse as before.
func TestParseAsset_PrefixWordClassicCodes(t *testing.T) {
	t.Parallel()

	classic := func(code string) Asset { return Asset{Type: AssetClassic, Code: code, Issuer: prefixTestIssuer} }
	rwaCode := KnownRWACodes()[0]
	cases := []struct {
		in   string
		want Asset
	}{
		{"fiat:" + prefixTestIssuer, classic("fiat")},
		{"fiat-" + prefixTestIssuer, classic("fiat")},
		{"crypto:" + prefixTestIssuer, classic("crypto")},
		{"crypto-" + prefixTestIssuer, classic("crypto")},
		{"rwa:" + prefixTestIssuer, classic("rwa")},
		{"rwa-" + prefixTestIssuer, classic("rwa")},
		{"raw-" + prefixTestIssuer, classic("raw")},
		// `raw:<symbol>` is the raw form's String, so a G-strkey symbol stays raw.
		{"raw:" + prefixTestIssuer, Asset{Type: AssetOracleRaw, Code: prefixTestIssuer}},
		{"fiat:USD", Asset{Type: AssetFiat, Code: "USD"}},
		{"crypto:XLM", Asset{Type: AssetCrypto, Code: "XLM"}},
		{"rwa:" + rwaCode, Asset{Type: AssetRWA, Code: rwaCode}},
		{"raw:BTC", Asset{Type: AssetOracleRaw, Code: "BTC"}},
	}
	for _, tc := range cases {
		got, err := ParseAsset(tc.in)
		if err != nil {
			t.Errorf("ParseAsset(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("ParseAsset(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		back, err := ParseAsset(got.String())
		if err != nil || !back.Equal(got) {
			t.Errorf("ParseAsset(%q.String()=%q) = %+v, %v; want %+v", tc.in, got.String(), back, err, got)
		}
	}

	for _, bad := range []string{"fiat:XXX", "crypto:NOPE", "rwa:NOPE", "fiat:" + prefixTestIssuer[:55]} {
		if a, err := ParseAsset(bad); err == nil {
			t.Errorf("ParseAsset(%q) = %+v, want error", bad, a)
		}
	}
}

// TestPrefixAllowListsHoldNoAccountID pins the precondition ParseAsset's
// fallthrough relies on: a G-strkey after `fiat:`/`crypto:`/`rwa:` can only
// be a classic issuer, never an allow-listed code.
func TestPrefixAllowListsHoldNoAccountID(t *testing.T) {
	t.Parallel()
	for name, list := range map[string]map[string]struct{}{
		"fiat": knownFiatCodes, "crypto": knownCryptoCodes, "rwa": knownRWACodes,
	} {
		for code := range list {
			if IsAccountID(code) {
				t.Errorf("%s allow-list holds G-strkey %q; ParseAsset would read %s:%s as classic", name, code, name, code)
			}
		}
	}
}

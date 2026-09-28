// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/metadata"
)

// tomlListsIssuer must canonicalise the same way timescale.Sep1EntryBindsTo
// does (case-fold, trim) — otherwise an issuer that types its own key
// lowercase in [[CURRENCIES]] gets org_verified: false here while
// AllSep1Images/BoundSep1Currencies already treat the entry as bound,
// overlay its logo, and admit it to /v1/rwa/assets.
func TestTomlListsIssuer(t *testing.T) {
	const gStrkey = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	cases := []struct {
		name       string
		currencies []metadata.Currency
		want       bool
	}{
		{
			name:       "exact match",
			currencies: []metadata.Currency{{Code: "USDC", Issuer: gStrkey}},
			want:       true,
		},
		{
			name:       "issuer typed lowercase",
			currencies: []metadata.Currency{{Code: "USDC", Issuer: "ga5zsejyb37jrc5avcia5mop4rhtm335x2kgx3ihojapp5re34k4kzvn"}},
			want:       true,
		},
		{
			name:       "issuer with surrounding whitespace",
			currencies: []metadata.Currency{{Code: "USDC", Issuer: " " + gStrkey + " "}},
			want:       true,
		},
		{
			name:       "different issuer",
			currencies: []metadata.Currency{{Code: "USDC", Issuer: "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"}},
			want:       false,
		},
		{
			name:       "not shaped like a strkey",
			currencies: []metadata.Currency{{Code: "USDC", Issuer: "not-an-account"}},
			want:       false,
		},
		{
			name:       "no currencies",
			currencies: nil,
			want:       false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tomlListsIssuer(c.currencies, gStrkey); got != c.want {
				t.Errorf("tomlListsIssuer(%+v, %q) = %v, want %v", c.currencies, gStrkey, got, c.want)
			}
		})
	}
}

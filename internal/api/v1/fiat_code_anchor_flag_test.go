// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// K033/F006 — the fiat carve-out (TestFiatCodedAnchorKeepsItsValuation)
// stops a SEP-1 anchor's deposit token from being flagged as an
// impersonator, but that leaves the catalogue silent about it: nothing
// on the wire distinguishes a recognised anchor from an arbitrary
// issuer that happens to code its asset "USD". This pins the positive
// verdict: a classic asset denominated in a catalogue fiat ticker gets
// a `fiat_code_anchor` note, and `known_anchor` is true exactly when
// the issuer is in the catalogue's curated set for that ticker.
func TestVerifiedCurrencyFlagsFiatCodeAnchor(t *testing.T) {
	const (
		anchorIssuer  = "GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX"
		unknownIssuer = "GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
		seed          = `verified_currencies:
  - ticker: USD
    slug: us-dollar
    name: US Dollar
    class: fiat
    circulating_supply: "1000"
    supply_decimals: 0
    networks: []
    known_anchors: ["GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX"]
`
	)
	cat, err := currency.LoadFromBytes([]byte(seed))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}

	cases := []struct {
		name            string
		issuer          string
		wantKnownAnchor bool
	}{
		{"curated anchor", anchorIssuer, true},
		{"uncurated issuer", unknownIssuer, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asset, err := canonical.ParseAsset("USD-" + tc.issuer)
			if err != nil {
				t.Fatalf("ParseAsset: %v", err)
			}
			s := &Server{verifiedCurrencies: cat}
			var detail AssetDetail
			flags := s.verifiedCurrencyFlags(&detail, asset)

			if flags.UnverifiedTickerCollision {
				t.Errorf("unverified_ticker_collision = true, want false — a fiat-coded token "+
					"denominates, it doesn't impersonate (%s)", tc.name)
			}
			if detail.FiatCodeAnchor == nil {
				t.Fatalf("fiat_code_anchor is nil, want populated for a catalogue fiat code")
			}
			if detail.FiatCodeAnchor.Ticker != "USD" {
				t.Errorf("fiat_code_anchor.ticker = %q, want USD", detail.FiatCodeAnchor.Ticker)
			}
			if detail.FiatCodeAnchor.KnownAnchor != tc.wantKnownAnchor {
				t.Errorf("fiat_code_anchor.known_anchor = %v, want %v",
					detail.FiatCodeAnchor.KnownAnchor, tc.wantKnownAnchor)
			}
			if detail.FiatCodeAnchor.Note == "" {
				t.Errorf("fiat_code_anchor.note is empty, want a rendered sentence")
			}
		})
	}
}

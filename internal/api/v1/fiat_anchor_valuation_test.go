// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// K033 — the money leg, on the surface a client actually lands on.
//
// A SEP-1 fiat anchor codes its deposit token with the ISO code of the
// currency it denominates (`anchor_asset_type: fiat`, `anchor_asset:
// USD`, classic code `USD`). The catalogue holds USD/EUR/GBP/… as
// sovereign-currency entries with no Stellar issuance, and filing those
// tickers in the impersonation index made StellarCollision report every
// such anchor — so populateMarketCap returned before the cap fill and
// /v1/assets/USD-G… served `market_cap_usd: null` for an asset that has
// a price and a supply, while the listing stamped the same row
// `unverified_ticker_collision: true` and the detail page warned that
// the code "matches a well-known asset that has NO verified issuance on
// Stellar" — of a dollar token, about the dollar.
//
// The control cases below are the other half of the pin: the
// impersonation machinery is untouched for tickers that name an ISSUED
// asset, which is what the catalogue's fiat carve-out must not disarm.
func TestFiatCodedAnchorKeepsItsValuation(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	// A real SEP-24 dollar anchor's issuing account, and a third party
	// holding a reference-only crypto ticker it cannot legitimately hold.
	const (
		anchorIssuer = "GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX"
		lookalike    = "GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	)

	cases := []struct {
		name string
		code string
		// issuer of the classic asset under test.
		issuer string
		// wantCap is the market_cap_usd the detail path must serve, or
		// "" when the impersonation guard must withhold it.
		wantCap string
	}{
		{"dollar anchor", "USD", anchorIssuer, "5000000.00"},
		{"euro anchor", "EUR", anchorIssuer, "5000000.00"},
		{"reference-only ticker impersonator", "XRP", lookalike, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asset, err := canonical.ParseAsset(tc.code + "-" + tc.issuer)
			if err != nil {
				t.Fatalf("ParseAsset: %v", err)
			}
			s := &Server{verifiedCurrencies: cat, minMarketCapVolumeUSD: 1000}
			price := "1.00"
			detail := AssetDetail{
				AssetID:  asset.String(),
				Code:     tc.code,
				Decimals: 7,
				PriceUSD: &price,
			}
			// 5,000,000 tokens at 7dp, priced at $1 → a $5M cap.
			snap := supply.Supply{
				CirculatingSupply: new(big.Int).SetUint64(50_000_000_000_000),
			}
			s.populateMarketCap(context.Background(), &detail, asset, snap, detail.AssetID, 5)

			switch {
			case tc.wantCap == "" && detail.MarketCapUSD != nil:
				t.Errorf("market_cap_usd = %q, want withheld — %s-G… impersonates an issued asset",
					*detail.MarketCapUSD, tc.code)
			case tc.wantCap == "":
			case detail.MarketCapUSD == nil:
				t.Errorf("market_cap_usd withheld, want %q — a %s-denominated anchor token is "+
					"following SEP-1, not impersonating the currency", tc.wantCap, tc.code)
			case *detail.MarketCapUSD != tc.wantCap:
				t.Errorf("market_cap_usd = %q, want %q", *detail.MarketCapUSD, tc.wantCap)
			}

			// The listing stamp and the detail warning read the same
			// verdict, so they move together with the cap.
			rows := []AssetDetail{{Code: tc.code, Issuer: strptr(tc.issuer)}}
			s.stampListingCollisions(rows)
			var warned AssetDetail
			gotWarning := applyUnverifiedWarning(&warned, asset, cat)
			wantFlagged := tc.wantCap == ""
			if rows[0].UnverifiedTickerCollision != wantFlagged {
				t.Errorf("unverified_ticker_collision = %v, want %v",
					rows[0].UnverifiedTickerCollision, wantFlagged)
			}
			if gotWarning != wantFlagged {
				t.Errorf("applyUnverifiedWarning = %v, want %v", gotWarning, wantFlagged)
			}
		})
	}
}

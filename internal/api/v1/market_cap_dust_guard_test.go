// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// dustGuardAssetID is an obscure classic asset with a huge supply — the
// "billions from a dust trade" shape the owner's requirement calls out:
// 10^17 raw units / 10^7 decimals = 10^10 whole tokens, which at $0.50
// multiplies to a $5,000,000,000 market cap off whatever price we serve.
const dustGuardAssetID = "OBSC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestMarketCap_DustLiquiditySuppression_DetailPath exercises the real
// populateMarketCap path on /v1/assets/{id} across the three cases the
// valuation-integrity guard must distinguish:
//
//  1. dust  (single venue AND sub-floor 24h volume) → market_cap_usd +
//     fdv_usd SUPPRESSED (null) and market_cap_low_liquidity=true;
//  2. single-venue but LIQUID ($100k/day) → market cap PRESENT (the AND
//     means one venue alone is not enough to suppress);
//  3. MULTI-source but thin ($10/day) → market cap PRESENT (multi-source
//     alone is not enough to suppress).
//
// In every case price_usd still serves — the guard is on the valuation,
// not the price. Without the guard, case 1 serves market_cap_usd
// "5000000000.00" (the exact defect); this test fails red against the
// un-guarded populateMarketCap and passes green with it.
func TestMarketCap_DustLiquiditySuppression_DetailPath(t *testing.T) {
	// Huge supply: an un-guarded dust price would assert billions.
	snap := supply.Supply{
		AssetKey:          dustGuardAssetID,
		TotalSupply:       mustBigInt("100000000000000000"),
		CirculatingSupply: mustBigInt("100000000000000000"),
		MaxSupply:         mustBigInt("100000000000000000"), // non-nil → skips SEP-1 overlay
		Basis:             supply.BasisOverride,
	}
	priceKey := dustGuardAssetID + "/fiat:USD"

	cases := []struct {
		name           string
		sources        []string
		volume         string
		wantSuppressed bool
	}{
		{"dust: single venue AND sub-floor volume", []string{"sdex"}, "10", true},
		{"single venue but liquid volume", []string{"sdex"}, "100000", false},
		{"multi-source but thin volume", []string{"sdex", "kraken"}, "10", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			priceStub := &stubPriceReader{
				snapshots: map[string]v1.PriceSnapshot{
					priceKey: {Price: "0.50", PriceType: "vwap"},
				},
				sources: map[string][]string{priceKey: tc.sources},
			}
			srv := v1.New(v1.Options{
				Prices:                priceStub,
				Supply:                &stubSupplyLooker{hit: true, snap: snap},
				Volume:                &stubVolumeReader{volume: tc.volume},
				MinMarketCapVolumeUSD: 1000,
			})
			ts := startHTTPTest(t, srv.Handler())

			resp := mustGet(t, ts.URL+"/v1/assets/"+dustGuardAssetID)
			body, _ := readAll(resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}

			// The price itself is never suppressed.
			mustContain(t, body, `"price_usd":"0.50"`)

			if tc.wantSuppressed {
				mustContain(t, body, `"market_cap_low_liquidity":true`)
				if strings.Contains(body, `"market_cap_usd"`) {
					t.Errorf("dust cap must be suppressed (market_cap_usd absent), body=%s", body)
				}
				if strings.Contains(body, `"fdv_usd"`) {
					t.Errorf("dust fdv must be suppressed (fdv_usd absent), body=%s", body)
				}
				return
			}
			// Present cases: the real (un-suppressed) cap is $5B.
			mustContain(t, body, `"market_cap_usd":"5000000000.00"`)
			mustContain(t, body, `"fdv_usd":"5000000000.00"`)
			if strings.Contains(body, `"market_cap_low_liquidity"`) {
				t.Errorf("market_cap_low_liquidity must be omitted when cap is present, body=%s", body)
			}
		})
	}
}

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

// TestMarketCap_NativeNeverDustSuppressed_DetailPath is the L2 regression: the
// listing SQL forces native XLM's source_count to NULL (asset_catalogue.go:
// `WHEN ca.asset_id = 'native' THEN NULL::int`), so XLM — triangulated and
// definitionally liquid — is NEVER dust-suppressed on the listing. The detail
// path (populateMarketCap) must agree, or /v1/assets/native and /v1/assets can
// disagree on the flag. A classic asset with the identical (single-venue,
// sub-floor-volume) shape IS suppressed — proving the carve-out is native-only,
// not a blanket disable.
//
// Fails red against the un-fixed populateMarketCap (native suppressed:
// market_cap_low_liquidity=true, cap absent); passes green with the carve-out.
func TestMarketCap_NativeNeverDustSuppressed_DetailPath(t *testing.T) {
	// 10^17 raw @ 7dp × $0.50 = $5,000,000,000.00.
	snap := supply.Supply{
		TotalSupply:       mustBigInt("100000000000000000"),
		CirculatingSupply: mustBigInt("100000000000000000"),
		MaxSupply:         mustBigInt("100000000000000000"), // non-nil → skips SEP-1 overlay
		Basis:             supply.BasisOverride,
	}

	cases := []struct {
		name           string
		assetPath      string
		priceKey       string
		wantSuppressed bool
	}{
		// Single venue AND sub-floor $10 volume — dust shape for BOTH.
		{"native XLM is carved out (never dust)", "native", "native/fiat:USD", false},
		{
			"classic control with identical shape IS suppressed",
			"OBSC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			"OBSC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN/fiat:USD", true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			priceStub := &stubPriceReader{
				snapshots: map[string]v1.PriceSnapshot{
					tc.priceKey: {Price: "0.50", PriceType: "vwap"},
				},
				sources: map[string][]string{tc.priceKey: {"sdex"}}, // single venue
			}
			srv := v1.New(v1.Options{
				Prices:                priceStub,
				Supply:                &stubSupplyLooker{hit: true, snap: snap},
				Volume:                &stubVolumeReader{volume: "10"}, // positive, sub-floor
				MinMarketCapVolumeUSD: 1000,
			})
			ts := startHTTPTest(t, srv.Handler())

			resp := mustGet(t, ts.URL+"/v1/assets/"+tc.assetPath)
			body, _ := readAll(resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			mustContain(t, body, `"price_usd":"0.50"`) // price always serves

			if tc.wantSuppressed {
				mustContain(t, body, `"market_cap_low_liquidity":true`)
				if strings.Contains(body, `"market_cap_usd"`) {
					t.Errorf("classic dust cap must be suppressed, body=%s", body)
				}
				return
			}
			mustContain(t, body, `"market_cap_usd":"5000000000.00"`)
			if strings.Contains(body, `"market_cap_low_liquidity"`) {
				t.Errorf("native XLM must never be dust-suppressed, body=%s", body)
			}
		})
	}
}

// TestMarketCap_NegativeSupplyOmitted_DetailPath is the finding-#4 regression:
// a negative circulating supply (Σburn > Σmint data error) must NOT serve a
// negative market_cap_usd on /v1/assets/{id}. usdMarketValue rejects the
// negative result (matching computeMarketCapUSD's Sign()<0 guard on the listing
// path), so market_cap_usd is OMITTED. fdv_usd (backed by a positive max
// supply) still serves — proving only the negative field is dropped, not all
// valuation.
//
// Multi-source + liquid volume so the dust guard is out of the picture — the
// only thing that can drop market_cap_usd here is the negative-value guard.
// Fails red against the un-fixed usdMarketValue (serves
// market_cap_usd:"-5000000000.00"); passes green with the guard.
func TestMarketCap_NegativeSupplyOmitted_DetailPath(t *testing.T) {
	const assetID = "OBSC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	snap := supply.Supply{
		TotalSupply:       mustBigInt("100000000000000000"),
		CirculatingSupply: mustBigInt("-100000000000000000"), // corrupt: negative
		MaxSupply:         mustBigInt("100000000000000000"),  // positive → fdv still valid
		Basis:             supply.BasisOverride,
	}
	priceKey := assetID + "/fiat:USD"
	priceStub := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{priceKey: {Price: "0.50", PriceType: "vwap"}},
		sources:   map[string][]string{priceKey: {"sdex", "kraken"}}, // multi-source
	}
	srv := v1.New(v1.Options{
		Prices:                priceStub,
		Supply:                &stubSupplyLooker{hit: true, snap: snap},
		Volume:                &stubVolumeReader{volume: "100000"}, // liquid
		MinMarketCapVolumeUSD: 1000,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/assets/"+assetID)
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if strings.Contains(body, `"market_cap_usd"`) {
		t.Errorf("negative-supply market_cap_usd must be OMITTED, not served; body=%s", body)
	}
	if strings.Contains(body, "-5000000000") {
		t.Errorf("a negative market cap must never appear on the wire; body=%s", body)
	}
	// The positive-max FDV still computes — only the negative field is dropped.
	mustContain(t, body, `"fdv_usd":"5000000000.00"`)
}

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

// dustGuardAssetID is an obscure classic asset with a huge supply — the
// "billions from a dust trade" shape the owner's requirement calls out:
// 10^17 raw units / 10^7 decimals = 10^10 whole tokens, which at $0.50
// multiplies to a $5,000,000,000 market cap off whatever price we serve.
const dustGuardAssetID = "OBSC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

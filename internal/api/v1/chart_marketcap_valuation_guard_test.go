// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// chartGuardLookalike is AQUA's code under an issuer that is not Aquarius':
// a verified-ticker collision the detail page refuses to value.
const chartGuardLookalike = "AQUA-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestChartMarketCap_ValuationGuards pins /v1/chart?price_type=market_cap to
// the valuation guards populateMarketCap applies on /v1/assets/{id}: a series
// the detail page would refuse as a headline cap (ticker collision, single
// venue with sub-floor volume, cap beyond the turnover ceiling) is withheld,
// and a liquidity refusal says so rather than reading as "no data".
func TestChartMarketCap_ValuationGuards(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name         string
		asset        string
		sources      []string
		volume       string
		floor, ratio float64
		wantPoints   int
		wantLowLiq   bool
	}{
		{"single venue, liquid: served", dustGuardAssetID, []string{"sdex"}, "100000", 1000, 0, 2, false},
		{"single venue, sub-floor volume: withheld", dustGuardAssetID, []string{"sdex"}, "10", 1000, 0, 0, true},
		{"multi-venue, cap beyond turnover ceiling: withheld", dustGuardAssetID, []string{"sdex", "kraken"}, "10", 0, 50000, 0, true},
		{"verified-ticker collision: withheld", chartGuardLookalike, []string{"sdex", "kraken"}, "100000", 1000, 50000, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			priceKey := tc.asset + "/fiat:USD"
			srv := v1.New(v1.Options{
				History: &stubHistoryReader{points: []v1.HistoryPoint{
					{Bucket: d(1), VWAP: "0.50"},
					{Bucket: d(2), VWAP: "0.50"},
				}},
				Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
					{Bucket: d(1), Circulating: mustBigInt("100000000000000000")}, // 10^10 tokens: $5B at $0.50
				}},
				Prices: &stubPriceReader{
					snapshots: map[string]v1.PriceSnapshot{priceKey: {Price: "0.50", PriceType: "vwap"}},
					sources:   map[string][]string{priceKey: tc.sources},
				},
				Volume:                  &stubVolumeReader{volume: tc.volume},
				VerifiedCurrencies:      newTestCatalogue(t),
				MinMarketCapVolumeUSD:   tc.floor,
				MaxMarketCapVolumeRatio: tc.ratio,
			})
			ts := httpTestServer(t, srv)
			resp := mustGet(t, ts.URL+"/v1/chart?asset="+tc.asset+"&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var env struct {
				Data struct {
					Points []struct {
						P string `json:"p"`
					} `json:"points"`
					MarketCapLowLiquidity bool `json:"market_cap_low_liquidity"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := len(env.Data.Points); got != tc.wantPoints {
				t.Errorf("points = %d, want %d: %+v", got, tc.wantPoints, env.Data.Points)
			}
			if tc.wantPoints > 0 && env.Data.Points[0].P != "5000000000.00" {
				t.Errorf("first cap = %q, want 5000000000.00", env.Data.Points[0].P)
			}
			if env.Data.MarketCapLowLiquidity != tc.wantLowLiq {
				t.Errorf("market_cap_low_liquidity = %v, want %v", env.Data.MarketCapLowLiquidity, tc.wantLowLiq)
			}
		})
	}
}

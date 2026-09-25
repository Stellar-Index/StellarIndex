// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// supplySnapAt is lockstepSupply's 1000-token (9 dp) circulating figure,
// observed at `at` and ledger 4242.
func supplySnapAt(at time.Time) *stubSupplyLooker {
	circ := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil))
	return &stubSupplyLooker{hit: true, snap: supply.Supply{
		CirculatingSupply: circ, ObservedAt: at, LedgerSequence: 4242,
	}}
}

// /v1/assets/{id} carries the supply's vintage, and an observation older than
// the listing's 6 h precise-arm bound is not multiplied by today's price: the
// supply serves with its as-of, the cap is withheld, the body is stale.
func TestAssetDetail_SupplyAgeBoundsTheCap(t *testing.T) {
	cases := []struct {
		name      string
		age       time.Duration
		wantCap   bool
		wantStale bool
	}{
		{"fresh observation", time.Hour, true, false},
		{"observer stopped a day ago", 25 * time.Hour, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().UTC().Add(-tc.age).Truncate(time.Second)
			srv := v1.New(v1.Options{Prices: lockstepPrices(), Supply: supplySnapAt(at)})
			body := lockstepGet(t, srv)

			if want := `"supply_as_of":"` + at.Format(time.RFC3339); !strings.Contains(body, want) {
				t.Errorf("body missing %s: %s", want, body)
			}
			if !strings.Contains(body, `"supply_as_of_ledger":4242`) {
				t.Errorf("body missing supply_as_of_ledger 4242: %s", body)
			}
			if got := strings.Contains(body, `"market_cap_usd":"`); got != tc.wantCap {
				t.Errorf("market_cap_usd present = %v, want %v: %s", got, tc.wantCap, body)
			}
			if got := strings.Contains(body, `"stale":true`); got != tc.wantStale {
				t.Errorf("flags.stale = %v, want %v: %s", got, tc.wantStale, body)
			}
		})
	}
}

// /v1/chart?price_type=market_cap forward-fills daily supply onto price days,
// but not indefinitely: a supply series that stops is not carried to today
// with stale:false.
func TestChartMarketCap_DeadSupplyIsNotForwardFilledForever(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	var price []v1.HistoryPoint
	for day := 1; day <= 10; day++ {
		price = append(price, v1.HistoryPoint{Bucket: d(day), VWAP: "0.50"})
	}
	priceKey := dustGuardAssetID + "/fiat:USD"
	srv := v1.New(v1.Options{
		History: &stubHistoryReader{points: price},
		Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
			{Bucket: d(1), Circulating: mustBigInt("100000000000000000")}, // last observed June 1
		}},
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{priceKey: {Price: "0.50", PriceType: "vwap"}},
		},
	})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?asset="+dustGuardAssetID+"&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Points []struct {
				T time.Time `json:"t"`
			} `json:"points"`
		} `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// June 1 supply covers June 1-3 (<= 48 h carry); June 4-10 are cut.
	if n := len(env.Data.Points); n != 3 || !env.Data.Points[n-1].T.Equal(d(3)) {
		t.Errorf("points = %+v, want June 1-3 only", env.Data.Points)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false on a series whose supply stopped a week before its last price day")
	}
}

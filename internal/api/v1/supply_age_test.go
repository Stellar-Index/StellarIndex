// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"math/big"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
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

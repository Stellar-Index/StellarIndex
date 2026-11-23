// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRWAReference_AMarketOutsideTheBandWithholdsThePremiumNotTheRow: a market price an order of magnitude from the
// oracle's value publishes no premium and names why, while the row and its reference stay served.
func TestRWAReference_AMarketOutsideTheBandWithholdsThePremiumNotTheRow(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107000000", 8, now.Add(-time.Hour)),
	})
	for _, price := range []string{"11.77", "0.09", "0"} {
		a := admittedRow("USTRY", price)
		rwaApplyReference(&a, snap, nil, listingAnswered(nil), now)
		if a.Premium.Status != RWAPremiumMarketOutsideBand || a.Premium.Pct != nil {
			t.Errorf("market %s against 1.07: premium = %+v, want %q and no pct", price, a.Premium, RWAPremiumMarketOutsideBand)
		}
		if a.Reference == nil {
			t.Errorf("market %s: the reference was dropped with the premium", price)
		}
	}
}

// TestRWAPremium_BothSurfacesRefuseTheSamePrints: /v1/rwa/assets and /v1/rwa/premium judge one market price against
// one published value alike, whatever market history precedes the print, so a token is never published on one and
// listed off-band on the other.
func TestRWAPremium_BothSurfacesRefuseTheSamePrints(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107000000", 8, now.Add(-time.Hour)),
	})
	prints := []string{"1.08", "2.00", "3.21", "3.22", "4.00", "9.50", "10.70", "12.00", "0.3567", "0.3566", "0.30", "0.12", "0.09"}
	for _, before := range []string{"1.08", "2.90", "0.40"} {
		for _, depth := range []int{0, 1, 4, 5, 14} {
			for _, price := range prints {
				a := admittedRow("USTRY", price)
				rwaApplyReference(&a, snap, nil, listingAnswered(nil), now)
				pointInTime := a.Premium.Status == RWAPremiumPublished

				market := make([]timescale.MarketDay, 0, depth+1)
				for d := 1; d <= depth; d++ {
					market = append(market, premGoodDay(d, before))
				}
				market = append(market, premGoodDay(depth+1, price))
				got := rwaPremiumJoin(premCand(), premRefDays(depth+1), market, premDefaultFloor(), nil)
				history := premPublished(got, depth+1)

				if pointInTime != history {
					t.Errorf("market %s against 1.07 after %d day(s) at %s: /v1/rwa/assets published = %v, /v1/rwa/premium published = %v",
						price, depth, before, pointInTime, history)
				}
				if !pointInTime && a.Premium.Status != RWAPremiumMarketOutsideBand {
					t.Errorf("market %s: point-in-time status = %q, want %q", price, a.Premium.Status, RWAPremiumMarketOutsideBand)
				}
			}
		}
	}
}

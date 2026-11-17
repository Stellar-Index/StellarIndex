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
// one published value alike, so a token is never published on one and listed off-band on the other.
func TestRWAPremium_BothSurfacesRefuseTheSamePrints(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107000000", 8, now.Add(-time.Hour)),
	})
	for _, price := range []string{"1.08", "2.00", "10.70", "10.71", "12.00", "0.107", "0.1069", "0.09"} {
		a := admittedRow("USTRY", price)
		rwaApplyReference(&a, snap, nil, listingAnswered(nil), now)
		pointInTime := a.Premium.Status == RWAPremiumPublished

		got := rwaPremiumJoin(premCand(), premRefDays(1), []timescale.MarketDay{premGoodDay(1, price)}, premDefaultFloor())
		history := premPublished(got, 1)

		if pointInTime != history {
			t.Errorf("market %s against 1.07: /v1/rwa/assets published = %v, /v1/rwa/premium published = %v", price, pointInTime, history)
		}
		if !pointInTime && a.Premium.Status != RWAPremiumMarketOutsideBand {
			t.Errorf("market %s: point-in-time status = %q, want %q", price, a.Premium.Status, RWAPremiumMarketOutsideBand)
		}
	}
}

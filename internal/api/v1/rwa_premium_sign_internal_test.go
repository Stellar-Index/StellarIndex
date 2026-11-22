// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A premium that rounds to zero is "0.0000" on both RWA surfaces whichever side of the reference the market sits; a
// non-zero rounding keeps its sign.
func TestRWAPremium_ZeroHasNoSign(t *testing.T) {
	for market, want := range map[string]string{
		"0.9999999": "0.0000",
		"1.0000001": "0.0000",
		"0.999999":  "-0.0001",
		"1.000001":  "0.0001",
	} {
		day, ok := rwaPremiumDayOf(timescale.MarketDay{VWAP: market, VolumeUSD: "5000"}, big.NewRat(1, 1))
		if !ok || day.pct != want {
			t.Errorf("/v1/rwa/premium at market %s: pct=%q ok=%v, want %q", market, day.pct, ok, want)
		}

		a := fundNAVAsset("BENJI", benjiFTIss)
		m := market
		a.Valuation.PriceUSD = &m
		snap := fundNAVSnap(t)
		snap.byFeed["BENJI"] = rwaReference{priceUSD: big.NewRat(1, 1), wire: "1.000000", source: "redstone", feed: "rwa:BENJI", asOf: fundNAVNow}
		rwaApplyReference(a, snap, nil, listingAnswered(nil), fundNAVNow)
		if a.Premium.Status != RWAPremiumPublished || a.Premium.Pct == nil || *a.Premium.Pct != want {
			t.Errorf("/v1/rwa/assets at market %s: premium=%+v, want published %q", market, a.Premium, want)
		}
	}
}

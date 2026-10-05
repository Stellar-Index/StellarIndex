// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fixedSubstance answers every substance read with one measurement and
// records the grain the point-in-time gate asked for.
type fixedSubstance struct {
	sub   timescale.MarketSubstance
	grain timescale.HistoryGranularity
}

func (f *fixedSubstance) PairMarketSubstance(context.Context, []canonical.Asset, []canonical.Asset, time.Duration) (timescale.MarketSubstance, error) {
	return f.sub, nil
}

func (f *fixedSubstance) PairMarketSubstanceAt(
	_ context.Context, _, _ []canonical.Asset, _ time.Time, _ time.Duration, g timescale.HistoryGranularity,
) (timescale.MarketSubstance, error) {
	f.grain = g
	return f.sub, nil
}

// TestValuationSubstanceOK_MatchesThePublishedPriceGate pins the resolver's
// mirrored floor to pricingguard's default point-in-time gate: at each
// grain, a measurement exactly at the floor and one unit short on each leg
// gets the same verdict and the same grain from both.
func TestValuationSubstanceOK_MatchesThePublishedPriceGate(t *testing.T) {
	sub := func(vol string, buckets, span int64) timescale.MarketSubstance {
		return timescale.MarketSubstance{VolumeUSD: vol, Buckets: buckets, SpanSeconds: span, ValuedBuckets: buckets}
	}
	const sixHours = int64(6 * 3600)
	cases := []struct {
		age  time.Duration
		subs []timescale.MarketSubstance
	}{
		{2 * time.Hour, []timescale.MarketSubstance{
			sub("1000", 20, sixHours), sub("999.99999999", 20, sixHours),
			sub("1000", 19, sixHours), sub("1000", 20, sixHours-1), sub("", 20, sixHours),
		}},
		{30 * 24 * time.Hour, []timescale.MarketSubstance{
			sub("1000", 2, sixHours), sub("999.99999999", 2, sixHours),
			sub("1000", 1, sixHours), sub("1000", 2, sixHours-1), sub("", 2, sixHours),
		}},
	}
	token, err := canonical.NewClassicAsset("TOKN", "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		for _, s := range tc.subs {
			t.Run(fmt.Sprintf("age=%s/%+v", tc.age, s), func(t *testing.T) {
				store := &fixedSubstance{sub: s}
				gate := pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{})
				at := time.Now().Add(-tc.age).Truncate(time.Hour)
				want := gate.AllowedAt(context.Background(), token, canonical.NativeAsset(), at, "lockstep_test")
				if g := timescale.ValuationSubstanceGrain(tc.age); g != store.grain {
					t.Fatalf("valuation grain %s, published-price gate grain %s", g, store.grain)
				}
				if got := timescale.ValuationSubstanceOK(s, store.grain); got != want {
					t.Errorf("valuation verdict %t, published-price gate %t", got, want)
				}
			})
		}
	}
}

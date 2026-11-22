// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const capRefusalContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

// rwaReasonsByStatus is every reason a valuation status may be served beside: a pair outside it was read from two readings.
var rwaReasonsByStatus = map[string]map[MarketCapWithheldReason]bool{
	RWAValuationPublished:       {"": true},
	RWAValuationNoSupply:        {"": true},
	RWAValuationLowLiquidity:    {MarketCapWithheldLowLiquidity: true},
	RWAValuationDecimalsUnknown: {MarketCapWithheldDecimalsUnknown: true, MarketCapWithheldDecimalsDiffer: true},
	RWAValuationCapWithheld: {
		MarketCapWithheldSupplyFloor: true, MarketCapWithheldThinMarket: true,
		MarketCapWithheldPriceBasis: true, MarketCapWithheldTickerCollision: true,
	},
}

func assertRWAReasonMatchesStatus(t *testing.T, v RWAValuation) {
	t.Helper()
	if !rwaReasonsByStatus[v.Status][v.MarketCapWithheldReason] {
		t.Errorf("valuation status %q served with reason %q: the pair comes from two different readings", v.Status, v.MarketCapWithheldReason)
	}
}

// capRefusalArm runs the contract arm over one row the generic listing fill already stamped.
func capRefusalArm(s *Server, row AssetDetail, scaled bool, sources int) AssetDetail {
	rows := []AssetDetail{row}
	sc := map[string]struct{}{}
	if scaled {
		sc[row.AssetID] = struct{}{}
	}
	src := map[string]timescale.AssetRow{row.AssetID: {AssetID: row.AssetID, SourceCount: &sources}}
	s.fillContractMarketCaps(context.Background(), rows, src, sc)
	return rows[0]
}

// The contract arm owns the row's cap, so a withheld reason the generic fill stamped must not outlive the arm's own verdict.
func TestContractArmReasonIsTheArmsOwnVerdict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stale      MarketCapWithheldReason
		decimals   int
		volume     string
		scaled     bool
		wantStatus string
		wantReason MarketCapWithheldReason
	}{
		{"dust after a generic decimals_unknown", MarketCapWithheldDecimalsUnknown, 7, "10", true, RWAValuationLowLiquidity, MarketCapWithheldLowLiquidity},
		{"turnover after a generic decimals_unknown", MarketCapWithheldDecimalsUnknown, 7, "2000", true, RWAValuationLowLiquidity, MarketCapWithheldLowLiquidity},
		{"scale disagreement after a generic low_liquidity", MarketCapWithheldLowLiquidity, 6, "250000", true, RWAValuationDecimalsUnknown, MarketCapWithheldDecimalsDiffer},
		{"unread scale after a generic supply_floor", MarketCapWithheldSupplyFloor, 7, "250000", false, RWAValuationDecimalsUnknown, MarketCapWithheldDecimalsUnknown},
		{"published after a generic decimals_unknown", MarketCapWithheldDecimalsUnknown, 7, "250000", true, RWAValuationPublished, ""},
		{"no contract supply after a generic decimals_unknown", MarketCapWithheldDecimalsUnknown, 7, "250000", true, RWAValuationNoSupply, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supplies := map[string]string{capRefusalContract: "100000000000000"}
			if tc.wantStatus == RWAValuationNoSupply {
				supplies = nil
			}
			s := &Server{
				Options: Options{
					TokenSupply:             capDecimalsTokenSupply{byID: supplies},
					MinMarketCapVolumeUSD:   1000,
					MaxMarketCapVolumeRatio: 1000,
				},
				logger: discardLogger(),
			}
			price, volume := "1.00", tc.volume
			got := capRefusalArm(s, AssetDetail{
				AssetID: capRefusalContract, Kind: "soroban_token", Decimals: tc.decimals,
				PriceUSD: &price, VolumeUSD24h: &volume, MarketCapWithheldReason: tc.stale,
			}, tc.scaled, 1)
			v := rwaValuationOf(got)
			if v.Status != tc.wantStatus || v.MarketCapWithheldReason != tc.wantReason {
				t.Errorf("valuation = %q reason %q, want %q reason %q", v.Status, v.MarketCapWithheldReason, tc.wantStatus, tc.wantReason)
			}
			assertRWAReasonMatchesStatus(t, v)
		})
	}
}

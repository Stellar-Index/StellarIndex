// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// An fdv_usd withheld beside a served market cap names its reason on the wire; the reason leaves once no cap or no price
// stands beside it, since market_cap_withheld_reason then explains both.
func TestFDVWithheldReasonOnFDVOnlyCeilingBreach(t *testing.T) {
	s := &Server{
		Options: Options{MinMarketCapVolumeUSD: 1000, MaxMarketCapVolumeRatio: 50_000},
		logger:  slogDiscard(),
	}
	price, vol := "1.5666161388", slvrVolumeUSD
	detail := AssetDetail{Decimals: 7, PriceUSD: &price, VolumeUSD24h: &vol}
	circ, _ := new(big.Int).SetString("500000000000000", 10)    // $78.3M cap, under the ~$189.56M ceiling
	maxSup, _ := new(big.Int).SetString("2000000000000000", 10) // $313.3M FDV, over it
	circWire := circ.String()
	detail.CirculatingSupply = &circWire
	asset, err := canonical.ParseAsset("SLVR-GBZVELEQD3WBN3R3VAG64HVBDOZ76ZL6QPLSFGKWPFED33Q3234NSLVR")
	if err != nil {
		t.Fatal(err)
	}
	s.populateMarketCap(context.Background(), &detail, asset, supply.Supply{CirculatingSupply: circ, MaxSupply: maxSup}, asset.String(), 5)

	wire := func(d AssetDetail) map[string]any {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	got := wire(detail)
	if got["market_cap_usd"] == nil || got["fdv_usd"] != nil {
		t.Fatalf("want a served cap and a withheld fdv_usd, got cap %v fdv %v", got["market_cap_usd"], got["fdv_usd"])
	}
	if got["fdv_withheld_reason"] != string(MarketCapWithheldLowLiquidity) {
		t.Errorf("fdv_withheld_reason = %v, want %q", got["fdv_withheld_reason"], MarketCapWithheldLowLiquidity)
	}

	unpriced := detail
	unpriced.PriceUSD, unpriced.MarketCapUSD = nil, nil
	if r, ok := wire(unpriced)["fdv_withheld_reason"]; ok {
		t.Errorf("fdv_withheld_reason = %v on a row a later suppressor unpriced, want omitted", r)
	}
}

// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import "testing"

func strptr(s string) *string { return &s }

// TestDustLiquiditySuppressed pins the shared valuation-integrity predicate
// that BOTH the listing path (fillMarketCapsFromSupply → computeMarketCapUSD)
// and the detail path (populateMarketCap) gate on. The AND is load-bearing:
// only a single backing venue AND sub-floor 24h volume suppresses.
func TestDustLiquiditySuppressed(t *testing.T) {
	const floor = 1000.0
	cases := []struct {
		name        string
		sourceCount int
		volume      *string
		floor       float64
		want        bool
	}{
		{"dust: 1 source, $10 vol", 1, strptr("10"), floor, true},
		{"dust: 1 source, $0 vol", 1, strptr("0"), floor, true},
		{"single venue but liquid $100k", 1, strptr("100000"), floor, false},
		{"single venue exactly at floor", 1, strptr("1000"), floor, false},
		{"multi-source thin $10", 2, strptr("10"), floor, false},
		{"multi-source thin, 3 venues", 3, strptr("5"), floor, false},
		{"floor disabled (0)", 1, strptr("10"), 0, false},
		{"unmeasured source count (0)", 0, strptr("10"), floor, false},
		{"volume unknown (nil)", 1, nil, floor, false},
		{"volume unparseable", 1, strptr("n/a"), floor, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dustLiquiditySuppressed(tc.sourceCount, tc.volume, tc.floor); got != tc.want {
				t.Errorf("dustLiquiditySuppressed(%d, %v, %v) = %v, want %v",
					tc.sourceCount, tc.volume, tc.floor, got, tc.want)
			}
		})
	}
}

// TestListingMarketCap_DustGuard drives the exact decision the listing fill
// loop (fillMarketCapsFromSupply) makes: suppress-and-flag when dust, else
// compute the real market cap via computeMarketCapUSD. The three headline
// cases from the finding are asserted end-to-end over the real functions.
//
// Supply 10^17 raw @ 7dp × $0.50 = $5,000,000,000 — the "billions" an
// un-guarded dust price would assert.
func TestListingMarketCap_DustGuard(t *testing.T) {
	const (
		floor = 1000.0
		circ  = "100000000000000000"
		price = "0.50"
		dec   = 7
	)
	cases := []struct {
		name        string
		sourceCount int
		volume      *string
		wantMC      string // "" means suppressed (null on the wire)
		wantLowLiq  bool
	}{
		{"dust one-trade → suppressed", 1, strptr("10"), "", true},
		{"single-venue liquid → present", 1, strptr("100000"), "5000000000.00", false},
		{"multi-source thin → present", 2, strptr("10"), "5000000000.00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				gotMC     string
				gotLowLiq bool
			)
			if dustLiquiditySuppressed(tc.sourceCount, tc.volume, floor) {
				gotLowLiq = true // cap left null
			} else {
				gotMC = computeMarketCapUSD(circ, price, dec)
			}
			if gotMC != tc.wantMC {
				t.Errorf("market cap = %q, want %q", gotMC, tc.wantMC)
			}
			if gotLowLiq != tc.wantLowLiq {
				t.Errorf("low-liquidity flag = %v, want %v", gotLowLiq, tc.wantLowLiq)
			}
		})
	}
}

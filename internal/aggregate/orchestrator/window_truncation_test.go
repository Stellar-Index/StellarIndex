// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestTick_WindowAtTheRowCapCountsAsTruncated pins the truncation
// detector: a window whose fetch comes back at MaxTradesPerWindow rows
// was cut by the store's LIMIT, and operators only learn that from
// AggregatorWindowTruncatedTotal. A window under the cap must not count.
func TestTick_WindowAtTheRowCapCountsAsTruncated(t *testing.T) {
	const maxRows = 3
	now := time.Now()
	fiveTrades := make([]canonical.Trade, 5)
	for i := range fiveTrades {
		fiveTrades[i] = buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000),
			now.Add(-time.Duration(5-i)*time.Minute/10))
	}

	for _, tc := range []struct {
		name   string
		trades []canonical.Trade
		want   float64
	}{
		{"over the cap", fiveTrades, 1},
		{"under the cap", fiveTrades[:maxRows-1], 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockStore{trades: tc.trades}
			rdb, _ := newTestRedis(t)
			orch := New(store, rdb, Config{
				Pairs:              []canonical.Pair{xlmUsdtPair(t)},
				Windows:            []time.Duration{5 * time.Minute},
				MaxTradesPerWindow: maxRows,
			})

			before := testutil.ToFloat64(obs.AggregatorWindowTruncatedTotal)
			if err := orch.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			if store.lastLimit != maxRows {
				t.Fatalf("store fetched with limit %d, want the configured cap %d", store.lastLimit, maxRows)
			}
			if got := testutil.ToFloat64(obs.AggregatorWindowTruncatedTotal) - before; got != tc.want {
				t.Errorf("AggregatorWindowTruncatedTotal advanced by %v, want %v", got, tc.want)
			}
		})
	}
}

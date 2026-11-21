// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Abandoned venue trades have no ledger, so the loss must carry the time window an operator refetches them over.
func TestShutdownLossBoundsTheOffChainWindow(t *testing.T) {
	at := func(source string, ledger uint32, ts time.Time) canonical.Trade {
		tr := mkTrade(source, ledger)
		tr.Timestamp = ts
		return tr
	}
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	lt := &lossTracker{}
	lt.trades([]canonical.Trade{
		at("binance", 0, t0.Add(5*time.Minute)),
		at("kraken", 0, t0),
		at("sdex", 800, t0.Add(time.Hour)), // on-chain: covered by MinLedger, not the window
		at("binance", 0, t0.Add(9*time.Minute)),
	})
	want := ShutdownLoss{Rows: 4, MinLedger: 800, LedgerUnknown: true, OffChainFrom: t0, OffChainTo: t0.Add(9 * time.Minute), Venues: "binance,kraken"}
	if got := lt.snapshot(); got != want {
		t.Errorf("ShutdownLoss = %+v; want %+v", got, want)
	}
}

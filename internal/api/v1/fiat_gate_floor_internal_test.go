// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// The point gate answers a bucket on the stored bars' own floor bit, not on a quote amount read at a guessed scale.
func TestTradeGateBuckets_UsesTheStoredFloorOnly(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	mk := func(source string, at time.Duration, quote int64, clears bool) canonical.Trade {
		return canonical.Trade{
			Source: source, Timestamp: t0.Add(at),
			BaseAmount:          canonical.NewAmount(big.NewInt(1_0000000)),
			QuoteAmount:         canonical.NewAmount(big.NewInt(quote)),
			ClearsNotionalFloor: clears,
		}
	}
	const unregistered = "not-a-registered-venue"
	if external.Registered(unregistered) {
		t.Fatalf("%q is registered; pick another name", unregistered)
	}
	answered := tradeGateBuckets([]canonical.Trade{
		// $20 of quote, but usd_volume not yet known: not above the floor.
		mk("sdex", 0, 20_0000000, false),
		// An unregistered 7dp venue's $0.05 trade that cleared the stored floor.
		mk(unregistered, 2*time.Minute, 500_000, true),
	})
	if _, ok := answered[fiatPointGateBucket(t0)]; ok {
		t.Error("a trade of unknown USD value answered its bucket")
	}
	if _, ok := answered[fiatPointGateBucket(t0.Add(2*time.Minute))]; !ok {
		t.Error("a trade above the stored floor did not answer its bucket: the gate applied a second floor")
	}
}

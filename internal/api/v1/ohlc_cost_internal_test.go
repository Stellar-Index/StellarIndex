// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A wider series span is never cheaper: time.Time.Sub saturates past ~292 years, and a span that wraps negative would price
// an all-history read at one token.
func TestOHLCSeriesCost_NeverFallsAsTheSpanGrows(t *testing.T) {
	s := &Server{}
	pair := canonical.Pair{Base: canonical.NativeAsset(), Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}}
	to := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	for _, interval := range []ohlcInterval{"1m", "5m", "1h", "1d", "1w"} {
		yearCost := s.ohlcSeriesCost(pair, interval, to.AddDate(-1, 0, 0), to)
		prev := 0
		for _, from := range []time.Time{
			to.AddDate(-1, 0, 0), to.AddDate(-100, 0, 0), to.AddDate(-291, 0, 0), to.AddDate(-293, 0, 0),
			to.AddDate(-300, 0, 0), time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
			{},
		} {
			cost := s.ohlcSeriesCost(pair, interval, from, to)
			if cost < prev || cost < yearCost {
				t.Errorf("%s from=%s: cost %d, want >= %d (narrower span) and >= %d (one year)", interval, from, cost, prev, yearCost)
			}
			prev = cost
		}
	}
}

// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"sync/atomic"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// panickingHist panics on its first N calls and then succeeds, so a test
// can assert both halves of the contract: the panic is contained, AND the
// cache entry it left behind is still RETRYABLE.
type panickingHist struct {
	HistoryReader
	calls     atomic.Int64
	panicUpTo int64
}

func (f *panickingHist) LatestTradePerSource(
	_ context.Context, _ canonical.Pair, _ string,
) ([]canonical.Trade, error) {
	if f.calls.Add(1) <= f.panicUpTo {
		// The shape the finding names: an index into an empty slice
		// returned by a degraded read.
		var degraded []canonical.Trade
		return []canonical.Trade{degraded[0]}, nil
	}
	return []canonical.Trade{{Source: "sdex"}}, nil
}

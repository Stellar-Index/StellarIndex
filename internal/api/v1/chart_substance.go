// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// noteBook records a source pair that claimed at least one served bucket, once.
func (r *chartWalkResult) noteBook(sp canonical.Pair) {
	for _, b := range r.books {
		if b.Base.Equal(sp.Base) && b.Quote.Equal(sp.Quote) {
			return
		}
	}
	r.books = append(r.books, sp)
}

// chartThin is [Server.thinMarketEvidence] over the books a chart walk served buckets from: the measurement /v1/vwap, /v1/twap
// and /v1/ohlc flag as thin_market and /v1/assets/{id} withholds a valuation on. An empty series measures nothing.
func (s *Server) chartThin(ctx context.Context, pair canonical.Pair, walk chartWalkResult) bool {
	return len(walk.books) > 0 && s.thinMarketEvidence(ctx, pair.Base, pair.Quote, walk.books, "chart") != nil
}

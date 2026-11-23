// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// noteBook records a source pair that claimed at least one served bucket, once, and whether it owns the newest one so far.
func (r *chartWalkResult) noteBook(sp canonical.Pair, newest time.Time) {
	if newest.After(r.latestAt) {
		r.latestBooks, r.latestAt = []canonical.Pair{sp}, newest
	}
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

// chartLatestThin measures only the books under the newest served bucket, the price /v1/assets/{id} values: a dust book
// that filled an older bucket does not withhold a valuation the headline serves. unmeasured: a book's read failed.
func (s *Server) chartLatestThin(ctx context.Context, pair canonical.Pair, walk chartWalkResult) (thin, unmeasured bool) {
	if walk.latestAt.IsZero() || len(walk.latestBooks) == 0 {
		return false, false
	}
	ev, unmeasured := s.thinMarketMeasure(ctx, pair.Base, pair.Quote, walk.latestBooks, "chart")
	return ev != nil, unmeasured
}

// noteCrossLatest records the books under a cross's newest bucket: every crossed point multiplies BOTH legs, so both back it.
// A leg with no recorded owner there contributes all its books, so the measurement can only widen.
func (r *chartWalkResult) noteCrossLatest(crossed []HistoryPoint, legs ...chartWalkResult) {
	if len(crossed) == 0 {
		return
	}
	at := crossed[len(crossed)-1].Bucket.UTC()
	r.latestAt, r.latestBooks = at, nil
	for _, leg := range legs {
		if book, ok := leg.bookAt[at]; ok {
			r.latestBooks = append(r.latestBooks, book)
			continue
		}
		r.latestBooks = append(r.latestBooks, leg.books...)
	}
}

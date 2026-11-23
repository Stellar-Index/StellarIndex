// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rawReadFanout is how many books one read of pair walks: a fiat quote reads every established and held-back constituent in
// turn (both fiat combine paths), any other quote stops at the first alias spelling with rows.
func (s *Server) rawReadFanout(pair canonical.Pair) int {
	if pair.Quote.Type != canonical.AssetFiat {
		return 1
	}
	established, heldBack := s.usdPeggedConstituentSets(pair)
	return max(1, len(established)+len(heldBack))
}

// tradeReadCost is [tradeScanCost] per book the raw-trade fetch reads: each book read is its own trade scan.
func (s *Server) tradeReadCost(pair canonical.Pair, from, to time.Time) int {
	return saturatingCost(int64(tradeScanCost(from, to)), s.rawReadFanout(pair))
}

// ohlcSeriesCost prices a /v1/ohlc series by the SOURCE rows it aggregates across every book, one token per
// [chartPointsPerToken]: the store groups all of [from, to) before its LIMIT, so `limit` bounds the answer, not the work.
func (s *Server) ohlcSeriesCost(pair canonical.Pair, interval ohlcInterval, from, to time.Time) int {
	route, ok := timescale.OHLCRouteFor(string(interval))
	grain := route.Native
	if grain == "" {
		grain = route.Source
	}
	width := grain.BucketDuration()
	if !ok || width <= 0 || !to.After(from) {
		return 1
	}
	// Divide before rounding up: Sub saturates at MaxInt64 past ~292 years, and adding width-1 to that wraps negative.
	span := to.Sub(from)
	buckets := int64(span / width)
	if span%width != 0 {
		buckets++
	}
	rows := saturatingCost(buckets, s.rawReadFanout(pair))
	return max(1, (rows+chartPointsPerToken-1)/chartPointsPerToken)
}

// saturatingCost is perRead x reads capped at MaxInt32; a negative perRead is an overflowed count, so it saturates too.
func saturatingCost(perRead int64, reads int) int {
	if perRead < 0 || perRead > math.MaxInt32/int64(reads) {
		return math.MaxInt32
	}
	return int(perRead * int64(reads))
}

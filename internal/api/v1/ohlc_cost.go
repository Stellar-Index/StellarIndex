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

// tradeReadCost is [tradeScanCost] per book the raw-trade fetch reads.
func (s *Server) tradeReadCost(pair canonical.Pair, from, to time.Time) int {
	return saturatingCost(int64(tradeScanCost(from, to)), s.rawReadFanout(pair))
}

// ohlcSeriesCost prices a /v1/ohlc series by the SOURCE rows its window aggregates, per book: the store groups all of
// [from, to) before its LIMIT, so `limit` bounds the answer, not the work.
func (s *Server) ohlcSeriesCost(pair canonical.Pair, interval ohlcInterval, from, to time.Time) int {
	route, ok := timescale.OHLCRouteFor(string(interval))
	grain := route.Native
	if grain == "" {
		grain = route.Source
	}
	width := grain.BucketDuration()
	if !ok || width <= 0 || !to.After(from) {
		return s.rawReadFanout(pair)
	}
	rows := int64((to.Sub(from) + width - 1) / width)
	return saturatingCost(1+rows/chartPointsPerToken, s.rawReadFanout(pair))
}

func saturatingCost(perRead int64, reads int) int {
	if perRead > math.MaxInt32/int64(reads) {
		return math.MaxInt32
	}
	return int(perRead * int64(reads))
}

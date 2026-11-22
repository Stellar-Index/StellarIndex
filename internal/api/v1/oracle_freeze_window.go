// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// FreezeWindowReader is the optional PriceReader extension prices() uses to refuse the buckets a recorded freeze covered,
// including after it recovered; the live freeze marker only says whether the pair is frozen now.
type FreezeWindowReader interface {
	FreezeWindows(ctx context.Context, assets []canonical.Asset, quote canonical.Asset, since time.Time) ([]timescale.FreezeWindow, error)
}

// dropFrozenBuckets removes every closed 1-minute bucket inside a recorded freeze of any spelling of asset against
// fiat:USD. dropped reports a removal; unknown a failed read, in which case the series is returned as read.
func (s *Server) dropFrozenBuckets(ctx context.Context, asset canonical.Asset, snaps []PriceSnapshot) (kept []PriceSnapshot, dropped, unknown bool) {
	fr, ok := s.Prices.(FreezeWindowReader)
	if !ok || len(snaps) == 0 {
		return snaps, false, false
	}
	oldest := snaps[0].ObservedAt.Time()
	for _, snap := range snaps[1:] {
		if at := snap.ObservedAt.Time(); at.Before(oldest) {
			oldest = at
		}
	}
	windows, err := fr.FreezeWindows(ctx, assetAliases(asset), defaultPriceQuote, oldest.Add(-time.Minute))
	if err != nil {
		s.logger.Warn("freeze window read failed (sep40 prices)", "err", err, "asset", asset.String())
		return snaps, false, true
	}
	kept = make([]PriceSnapshot, 0, len(snaps))
	for _, snap := range snaps {
		if bucketInAnyFreeze(windows, snap.ObservedAt.Time().Add(-time.Minute)) {
			dropped = true
			continue
		}
		kept = append(kept, snap)
	}
	return kept, dropped, false
}

func bucketInAnyFreeze(windows []timescale.FreezeWindow, bucket time.Time) bool {
	for _, w := range windows {
		if w.CoversMinute(bucket) {
			return true
		}
	}
	return false
}

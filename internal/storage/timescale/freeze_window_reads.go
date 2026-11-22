// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"time"
)

// FreezeTriggerLookback widens a freeze back over the minutes that tripped it; [xlmUSDFreezeTriggerLookback] is the same span in SQL.
const FreezeTriggerLookback = 5 * time.Minute

// FreezeWindow is one recorded freeze of a pair (ADR-0019); RecoveredAt is nil while it is still firing.
type FreezeWindow struct {
	FrozenAt    time.Time
	RecoveredAt *time.Time
}

// CoversMinute reports whether the closed 1-minute bucket starting at bucket overlaps the freeze, the predicate
// [xlmUSDFreezeWindow] applies to the XLM/USD anchor.
func (w FreezeWindow) CoversMinute(bucket time.Time) bool {
	if !w.FrozenAt.Before(bucket.Add(time.Minute + FreezeTriggerLookback)) {
		return false
	}
	return w.RecoveredAt == nil || w.RecoveredAt.After(bucket)
}

// FreezeWindowsForPair returns every freeze recorded for any of assetIDs against quoteID that had not recovered by since. A USD
// proxy quote stands for every USD proxy in either stored direction, the population [xlmUSDFreezePairs] keeps off the anchor.
func (s *Store) FreezeWindowsForPair(ctx context.Context, assetIDs []string, quoteID string, since time.Time) ([]FreezeWindow, error) {
	if len(assetIDs) == 0 {
		return nil, nil
	}
	pairs := `fe.asset_id = ANY($1) AND fe.quote_id = $3`
	args := []any{assetIDs, since, quoteID}
	if isUSDProxyAsset(quoteID) {
		pairs, args = usdProxyFreezePairs("= ANY($1)"), args[:2]
	}
	q := `
		SELECT fe.frozen_at, fe.recovered_at
		  FROM freeze_events fe
		 WHERE ` + pairs + `
		   AND (fe.recovered_at IS NULL OR fe.recovered_at > $2::timestamptz)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: FreezeWindowsForPair: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FreezeWindow
	for rows.Next() {
		var w FreezeWindow
		if err := rows.Scan(&w.FrozenAt, &w.RecoveredAt); err != nil {
			return nil, fmt.Errorf("timescale: FreezeWindowsForPair scan: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

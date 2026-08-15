package timescale

import (
	"context"
	"fmt"
	"time"
)

// PriceSourceContribution is one row's worth of per-source weight
// for a single (asset, quote, bucket).
type PriceSourceContribution struct {
	AssetID    string
	QuoteID    string
	Bucket     time.Time
	Source     string
	Weight     float64
	VolumeUSD  *float64
	TradeCount int
}

// InsertPriceSourceContributions writes a batch of per-source
// contribution rows. This table is APPEND-PER-TICK: the conflict key
// bucket is stamped with the orchestrator's ComputedAt (time.Now() at
// flush, orchestrator.go), NOT a truncated window boundary, so every
// recompute of the same (asset, quote, window) carries a distinct
// bucket and INSERTs a fresh row. The ON CONFLICT (asset, quote,
// bucket, source) DO UPDATE arm below is therefore effectively
// unreachable in practice (two flushes never share a ComputedAt for
// one key) and does NOT refresh a historical row in place — an
// earlier docstring claiming it did was false (MR-2). Readers must
// pick the latest row per (asset, quote, source) for a window rather
// than assuming one row per source; the raw table accumulates one row
// per tick per source.
//
// Consequently the unguarded volume_usd never overwrites a prior
// value (no in-place regression risk), but also never corrects one —
// which is why this table is NOT part of the INV-3
// generation-guarded corrective-upsert family.
//
// Volume is optional (some on-chain pairs don't have a USD-volume
// computation today; the source-donut gracefully degrades).
func (s *Store) InsertPriceSourceContributions(ctx context.Context, rows []PriceSourceContribution) error {
	if len(rows) == 0 {
		return nil
	}
	const q = `
		INSERT INTO price_source_contributions (
		    asset_id, quote_id, bucket, source,
		    weight, volume_usd, trade_count
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (asset_id, quote_id, bucket, source) DO UPDATE SET
		    weight       = EXCLUDED.weight,
		    volume_usd   = EXCLUDED.volume_usd,
		    trade_count  = EXCLUDED.trade_count
	`
	for _, r := range rows {
		var volumeUSD any
		if r.VolumeUSD != nil {
			volumeUSD = *r.VolumeUSD
		}
		if _, err := s.db.ExecContext(ctx, q,
			r.AssetID, r.QuoteID, r.Bucket.UTC(), r.Source,
			r.Weight, volumeUSD, r.TradeCount,
		); err != nil {
			return fmt.Errorf("timescale: InsertPriceSourceContributions %s/%s/%s: %w",
				r.AssetID, r.QuoteID, r.Source, err)
		}
	}
	return nil
}

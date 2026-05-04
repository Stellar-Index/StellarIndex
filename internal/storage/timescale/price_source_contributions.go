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
// contribution rows. ON CONFLICT (asset, quote, bucket, source) DO
// UPDATE replaces the values — useful when the orchestrator
// re-computes a window with fresher data and we want to refresh
// the historical row rather than spawning a duplicate.
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

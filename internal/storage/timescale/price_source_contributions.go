package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrContributionWindowRequired is returned when a contribution row has no aggregation window, without which the 5m/1h/24h breakdowns of a pair are indistinguishable.
var ErrContributionWindowRequired = errors.New(
	"timescale: price source contribution needs a positive whole-second Window")

// PriceSourceContribution is one per-source weight for a single (asset, quote, window, bucket); Window is required and Weight/VolumeUSD are decimal strings bound to NUMERIC.
type PriceSourceContribution struct {
	AssetID    string
	QuoteID    string
	Window     time.Duration
	Bucket     time.Time
	Source     string
	Weight     string
	VolumeUSD  *string
	TradeCount int
}

// InsertPriceSourceContributions writes a batch of per-source contribution rows; the table is append-per-tick (bucket is ComputedAt at flush, not a truncated boundary), so each recompute inserts a fresh row.
func (s *Store) InsertPriceSourceContributions(ctx context.Context, rows []PriceSourceContribution) error {
	if len(rows) == 0 {
		return nil
	}
	for _, r := range rows {
		if r.Window <= 0 || r.Window%time.Second != 0 {
			return fmt.Errorf("%w: %s/%s/%s window=%s",
				ErrContributionWindowRequired, r.AssetID, r.QuoteID, r.Source, r.Window)
		}
	}
	const q = `
		INSERT INTO price_source_contributions (
		    asset_id, quote_id, window_seconds, bucket, source,
		    weight, volume_usd, trade_count
		)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8)
		ON CONFLICT (asset_id, quote_id, window_seconds, source, bucket) DO UPDATE SET
		    weight       = EXCLUDED.weight,
		    volume_usd   = EXCLUDED.volume_usd,
		    trade_count  = EXCLUDED.trade_count
	`
	// One transaction: a batch is one bucket's weights, which only mean
	// anything together (they sum to 1). A mid-batch failure must leave
	// no rows rather than a committed prefix.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: InsertPriceSourceContributions begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, r := range rows {
		var volumeUSD any
		if r.VolumeUSD != nil {
			volumeUSD = *r.VolumeUSD
		}
		if _, err := tx.ExecContext(ctx, q,
			r.AssetID, r.QuoteID, int64(r.Window/time.Second), r.Bucket.UTC(), r.Source,
			r.Weight, volumeUSD, r.TradeCount,
		); err != nil {
			return fmt.Errorf("timescale: InsertPriceSourceContributions %s/%s/%s: %w",
				r.AssetID, r.QuoteID, r.Source, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: InsertPriceSourceContributions commit: %w", err)
	}
	return nil
}

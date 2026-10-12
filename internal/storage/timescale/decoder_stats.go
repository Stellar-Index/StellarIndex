package timescale

import (
	"context"
	"fmt"
	"time"
)

// DecoderStatsBucket is one row's worth of dispatcher counters for a single source over a 5-minute window.
type DecoderStatsBucket struct {
	Bucket       time.Time
	Source       string
	EventsSeen   int64
	DecodeErrors int64
	OrphanEvents int64
	LastLedger   uint32
}

// InsertDecoderStats writes decoder_stats_5m rows (empty input is a no-op), merging two flushes of one bucket via ON CONFLICT DO UPDATE; callers pass deltas, not running totals, and the write is all-or-nothing in one transaction.
func (s *Store) InsertDecoderStats(ctx context.Context, rows []DecoderStatsBucket) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: InsertDecoderStats begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	const q = `
		INSERT INTO decoder_stats_5m (
		    bucket, source, events_seen, decode_errors, orphan_events, last_ledger
		)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (bucket, source) DO UPDATE SET
		    events_seen   = decoder_stats_5m.events_seen   + EXCLUDED.events_seen,
		    decode_errors = decoder_stats_5m.decode_errors + EXCLUDED.decode_errors,
		    orphan_events = decoder_stats_5m.orphan_events + EXCLUDED.orphan_events,
		    last_ledger   = GREATEST(decoder_stats_5m.last_ledger, EXCLUDED.last_ledger)
	`
	for _, r := range rows {
		var lastLedger any
		if r.LastLedger > 0 {
			lastLedger = int64(r.LastLedger)
		}
		if _, err := tx.ExecContext(ctx, q,
			r.Bucket.UTC(), r.Source,
			r.EventsSeen, r.DecodeErrors, r.OrphanEvents,
			lastLedger,
		); err != nil {
			return fmt.Errorf("timescale: InsertDecoderStats %s/%s: %w",
				r.Bucket.Format(time.RFC3339), r.Source, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: InsertDecoderStats commit: %w", err)
	}
	return nil
}

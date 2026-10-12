package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// StoredBaseline is the storage-to-aggregator wire shape: a [baseline.MultiBaseline] plus refresh metadata; Day1/Day7 nil means bootstrap at that scale, while Day30 is required.
type StoredBaseline struct {
	Pair        canonical.Pair
	ComputedAt  time.Time
	WindowStart time.Time
	WindowEnd   time.Time
	Multi       baseline.MultiBaseline
}

// ErrBaselineNotFound is returned by [Store.LatestBaseline] when the requested pair has never had a baseline written.
var ErrBaselineNotFound = errors.New("timescale: baseline not found for pair")

// UpsertBaseline writes the pair's single baseline row (the aggregator refresh worker is the only writer), requiring a non-nil Day30 with N >= [baseline.MinSamples] and WindowEnd > WindowStart.
func (s *Store) UpsertBaseline(ctx context.Context, sb StoredBaseline) error {
	if sb.Multi.Day30 == nil {
		return fmt.Errorf("timescale: UpsertBaseline %s: 30d baseline is nil", sb.Pair.String())
	}
	if sb.Multi.Day30.N < baseline.MinSamples {
		return fmt.Errorf("timescale: UpsertBaseline %s: 30d sample_count=%d < %d",
			sb.Pair.String(), sb.Multi.Day30.N, baseline.MinSamples)
	}
	if !sb.WindowEnd.After(sb.WindowStart) {
		return fmt.Errorf("timescale: UpsertBaseline %s: window_end %v <= window_start %v",
			sb.Pair.String(), sb.WindowEnd, sb.WindowStart)
	}

	const q = `
		INSERT INTO volatility_baseline_1m
		    (base_asset, quote_asset, computed_at, window_start, window_end,
		     median, mad, sample_count,
		     median_1d, mad_1d, n_1d,
		     median_7d, mad_7d, n_7d)
		VALUES
		    ($1, $2, $3, $4, $5,
		     $6, $7, $8,
		     $9, $10, $11,
		     $12, $13, $14)
		ON CONFLICT (base_asset, quote_asset) DO UPDATE SET
		    computed_at  = EXCLUDED.computed_at,
		    window_start = EXCLUDED.window_start,
		    window_end   = EXCLUDED.window_end,
		    median       = EXCLUDED.median,
		    mad          = EXCLUDED.mad,
		    sample_count = EXCLUDED.sample_count,
		    median_1d    = EXCLUDED.median_1d,
		    mad_1d       = EXCLUDED.mad_1d,
		    n_1d         = EXCLUDED.n_1d,
		    median_7d    = EXCLUDED.median_7d,
		    mad_7d       = EXCLUDED.mad_7d,
		    n_7d         = EXCLUDED.n_7d
	`
	d1Median, d1MAD, d1N := nullableBaseline(sb.Multi.Day1)
	d7Median, d7MAD, d7N := nullableBaseline(sb.Multi.Day7)

	_, err := s.db.ExecContext(ctx, q,
		sb.Pair.Base.String(),
		sb.Pair.Quote.String(),
		sb.ComputedAt.UTC(),
		sb.WindowStart.UTC(),
		sb.WindowEnd.UTC(),
		sb.Multi.Day30.Median,
		sb.Multi.Day30.MAD,
		sb.Multi.Day30.N,
		d1Median, d1MAD, d1N,
		d7Median, d7MAD, d7N,
	)
	if err != nil {
		return fmt.Errorf("timescale: UpsertBaseline %s: %w", sb.Pair.String(), err)
	}
	return nil
}

// nullableBaseline converts a `*baseline.Baseline` into the three
// (NullFloat, NullFloat, NullInt) pgx-compatible scalars that the
// `*_1d` / `*_7d` columns expect. Nil pointer → all-NULL.
func nullableBaseline(b *baseline.Baseline) (sql.NullFloat64, sql.NullFloat64, sql.NullInt64) {
	if b == nil {
		return sql.NullFloat64{}, sql.NullFloat64{}, sql.NullInt64{}
	}
	return sql.NullFloat64{Valid: true, Float64: b.Median},
		sql.NullFloat64{Valid: true, Float64: b.MAD},
		sql.NullInt64{Valid: true, Int64: int64(b.N)}
}

// LatestBaseline returns the pair's current baseline by primary-key point lookup, or [ErrBaselineNotFound] so the caller falls through to the bootstrap policy; API hot path.
func (s *Store) LatestBaseline(ctx context.Context, pair canonical.Pair) (StoredBaseline, error) {
	const q = `
		SELECT computed_at, window_start, window_end,
		       median, mad, sample_count,
		       median_1d, mad_1d, n_1d,
		       median_7d, mad_7d, n_7d
		  FROM volatility_baseline_1m
		 WHERE base_asset = $1 AND quote_asset = $2
	`
	var (
		sb              StoredBaseline
		median30        float64
		mad30           float64
		n30             int
		d1Median, d1MAD sql.NullFloat64
		d1N             sql.NullInt64
		d7Median, d7MAD sql.NullFloat64
		d7N             sql.NullInt64
	)
	sb.Pair = pair
	err := s.db.QueryRowContext(ctx, q,
		pair.Base.String(), pair.Quote.String(),
	).Scan(
		&sb.ComputedAt,
		&sb.WindowStart,
		&sb.WindowEnd,
		&median30, &mad30, &n30,
		&d1Median, &d1MAD, &d1N,
		&d7Median, &d7MAD, &d7N,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StoredBaseline{}, ErrBaselineNotFound
		}
		return StoredBaseline{}, fmt.Errorf("timescale: LatestBaseline %s: %w", pair.String(), err)
	}
	sb.Multi = baseline.MultiBaseline{
		Day30: &baseline.Baseline{Median: median30, MAD: mad30, N: n30},
	}
	if d1N.Valid {
		sb.Multi.Day1 = &baseline.Baseline{
			Median: d1Median.Float64, MAD: d1MAD.Float64, N: int(d1N.Int64),
		}
	}
	if d7N.Valid {
		sb.Multi.Day7 = &baseline.Baseline{
			Median: d7Median.Float64, MAD: d7MAD.Float64, N: int(d7N.Int64),
		}
	}
	return sb, nil
}

// CountBaselines returns the row count of volatility_baseline_1m; a diagnostic for aggregator metrics, not for hot paths.
func (s *Store) CountBaselines(ctx context.Context) (int64, error) {
	const q = `SELECT count(*) FROM volatility_baseline_1m`
	var n int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: CountBaselines: %w", err)
	}
	return n, nil
}

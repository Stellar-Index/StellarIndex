package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// foldClosedThrough returns the pair's first trade that `view` has not materialised; a fold may serve only out-buckets
// ending at or before it. Not valid means there is none.
//
// An out-bucket that wall-clock closed can still lack its last source buckets: the refresh policy materialises them
// minutes to days later, and folding what exists would serve a closed bar whose close, extremes and volume then
// change. The view's watermark alone cannot be the bound: it is the end of the newest materialised bucket across ALL
// pairs, so on a quiet network it stops at the last trade and a finished bar would never appear. Two statements, so
// the trades probe plans against a constant lower bound and touches only the newest chunks.
func (s *Store) foldClosedThrough(ctx context.Context, view string, p canonical.Pair) (sql.NullTime, error) {
	var through sql.NullTime
	var watermark time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT GREATEST(_timescaledb_functions.to_timestamp(_timescaledb_functions.cagg_watermark(mat_hypertable_id)),
		                'epoch'::timestamptz)
		  FROM _timescaledb_catalog.continuous_agg
		 WHERE (quote_ident(user_view_schema) || '.' || quote_ident(user_view_name))::regclass = $1::regclass`,
		view).Scan(&watermark)
	if err != nil {
		return through, fmt.Errorf("timescale: %s materialisation watermark: %w", view, err)
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT LEAST((SELECT min(ts) FROM trades WHERE base_asset = $1 AND quote_asset = $2 AND ts >= $3),
		             (SELECT min(ts) FROM trades WHERE base_asset = $2 AND quote_asset = $1 AND ts >= $3))`,
		p.Base.String(), p.Quote.String(), watermark.UTC()).Scan(&through)
	if err != nil {
		return through, fmt.Errorf("timescale: %s unmaterialised trades of %s: %w", view, p, err)
	}
	return through, nil
}

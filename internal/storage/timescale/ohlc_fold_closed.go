package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

type foldBoundsKey struct{}

type foldBounds struct {
	mu     sync.Mutex
	byView map[string]sql.NullTime
}

// WithFoldBounds returns a context under which every folded read of one view uses the bound the first of them read. A
// request that merges several pairs needs it: a refresh committing between two reads would otherwise serve a bucket
// for the later pair that was withheld for the earlier one.
func WithFoldBounds(ctx context.Context) context.Context {
	return context.WithValue(ctx, foldBoundsKey{}, &foldBounds{byView: map[string]sql.NullTime{}})
}

// foldClosedThrough returns the first trade of ANY pair that `view` has not materialised; a fold may serve only
// out-buckets ending at or before it. Not valid means there is none.
//
// An out-bucket that wall-clock closed can still lack its last source buckets: the refresh policy materialises them
// minutes to days later, and folding what exists would serve a closed bar whose close, extremes and volume then
// change. The view's watermark alone cannot be the bound: it is the end of the newest materialised bucket across all
// pairs, so on a quiet network it stops at the last trade and a finished bar would never appear.
//
// The bound is the same for every pair on purpose. A per-pair bound withholds a busy pair's bar while serving a quiet
// pair's, and the fiat-combined routes then publish a bucket built from the quiet pair alone; the alias loop would
// likewise fall through to another market. One bound withholds a bucket for all of them or none.
//
// Two statements, so the trades probe plans against a constant lower bound and touches only the newest chunks.
func (s *Store) foldClosedThrough(ctx context.Context, view string) (sql.NullTime, error) {
	shared, _ := ctx.Value(foldBoundsKey{}).(*foldBounds)
	if shared != nil {
		shared.mu.Lock()
		defer shared.mu.Unlock()
		if through, ok := shared.byView[view]; ok {
			return through, nil
		}
	}
	through, err := s.readFoldClosedThrough(ctx, view)
	if err == nil && shared != nil {
		shared.byView[view] = through
	}
	return through, err
}

func (s *Store) readFoldClosedThrough(ctx context.Context, view string) (sql.NullTime, error) {
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
	err = s.db.QueryRowContext(ctx, `SELECT min(ts) FROM trades WHERE ts >= $1::timestamptz`, watermark.UTC()).Scan(&through)
	if err != nil {
		return through, fmt.Errorf("timescale: %s unmaterialised trades: %w", view, err)
	}
	return through, nil
}

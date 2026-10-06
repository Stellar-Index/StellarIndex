// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CAGGInvalidationBacklog is what a non-forced refresh of one continuous
// aggregate still has to re-materialise, read from Timescale's
// invalidation logs.
type CAGGInvalidationBacklog struct {
	View string
	// Ranges are the view's bounded materialization-log entries, already
	// widened to its buckets; From/To is their hull and Span the summed
	// length (overlaps counted twice). From/To are zero when Ranges is 0.
	Ranges   int64
	From, To time.Time
	Span     time.Duration
	// OpenEnded counts entries reaching ±infinity: the edges of history
	// the view has never been refreshed over, not late writes.
	OpenEnded int64
	// SourceRanges are entries in the source hypertable's log that no
	// refresh has yet moved into the per-view logs; every view on that
	// source inherits them. SourceFrom/SourceTo is their hull.
	SourceRanges         int64
	SourceFrom, SourceTo time.Time
}

// CAGGInvalidationBacklogs reads the pending invalidation ranges of each
// of views, in that order. It reads TimescaleDB's internal catalog
// (_timescaledb_catalog, checked against 2.26.4), so it is a sizing aid,
// not a contract: a view missing from the catalog is an error.
func (s *Store) CAGGInvalidationBacklogs(ctx context.Context, views []string) ([]CAGGInvalidationBacklog, error) {
	// The log stores Timescale's internal time (Unix µs); int64 min/max
	// are its -infinity/+infinity sentinels.
	const q = `
		WITH lim AS (SELECT (-9223372036854775807 - 1)::bigint AS lo, 9223372036854775807::bigint AS hi)
		SELECT m.n, _timescaledb_functions.to_timestamp(m.lo), _timescaledb_functions.to_timestamp(m.hi), m.span_us, m.open_ended,
		       h.n, _timescaledb_functions.to_timestamp(h.lo), _timescaledb_functions.to_timestamp(h.hi)
		  FROM _timescaledb_catalog.continuous_agg ca, lim
		  CROSS JOIN LATERAL (
		    SELECT count(*) FILTER (WHERE b) AS n,
		           min(lowest_modified_value) FILTER (WHERE b) AS lo,
		           max(greatest_modified_value) FILTER (WHERE b) AS hi,
		           coalesce(sum(greatest_modified_value - lowest_modified_value + 1) FILTER (WHERE b), 0)::bigint AS span_us,
		           count(*) FILTER (WHERE NOT b) AS open_ended
		      FROM (SELECT lowest_modified_value, greatest_modified_value,
		                   lowest_modified_value > lim.lo AND greatest_modified_value < lim.hi AS b
		              FROM _timescaledb_catalog.continuous_aggs_materialization_invalidation_log
		             WHERE materialization_id = ca.mat_hypertable_id) ml
		  ) m
		  CROSS JOIN LATERAL (
		    SELECT count(*) AS n, min(lowest_modified_value) AS lo, max(greatest_modified_value) AS hi
		      FROM _timescaledb_catalog.continuous_aggs_hypertable_invalidation_log
		     WHERE hypertable_id = ca.raw_hypertable_id
		       AND lowest_modified_value > lim.lo AND greatest_modified_value < lim.hi
		  ) h
		 WHERE ca.user_view_schema = 'public' AND ca.user_view_name = $1`
	out := make([]CAGGInvalidationBacklog, 0, len(views))
	for _, v := range views {
		b := CAGGInvalidationBacklog{View: v}
		var from, to, srcFrom, srcTo sql.NullTime
		var spanUS int64
		err := s.db.QueryRowContext(ctx, q, v).Scan(&b.Ranges, &from, &to, &spanUS, &b.OpenEnded,
			&b.SourceRanges, &srcFrom, &srcTo)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("timescale: CAGGInvalidationBacklogs: %s is not a continuous aggregate", v)
		}
		if err != nil {
			return nil, fmt.Errorf("timescale: CAGGInvalidationBacklogs(%s): %w", v, err)
		}
		b.From, b.To, b.Span = from.Time, to.Time, time.Duration(spanUS)*time.Microsecond
		b.SourceFrom, b.SourceTo = srcFrom.Time, srcTo.Time
		out = append(out, b)
	}
	return out, nil
}

// TradeLedgersInTimeRange returns the ledgers of the first trade at or
// after from and the last at or before to, the inverse of
// [Store.LedgerRangeToTimeRange]. Each is one index probe on trades.ts.
// [ErrNotFound] when no trade lies in [from, to].
func (s *Store) TradeLedgersInTimeRange(ctx context.Context, from, to time.Time) (uint32, uint32, error) {
	const q = `
		SELECT (SELECT ledger FROM trades WHERE ts >= $1::timestamptz AND ts <= $2::timestamptz ORDER BY ts ASC LIMIT 1),
		       (SELECT ledger FROM trades WHERE ts >= $1::timestamptz AND ts <= $2::timestamptz ORDER BY ts DESC LIMIT 1)`
	var lo, hi sql.Null[uint32]
	if err := s.db.QueryRowContext(ctx, q, from, to).Scan(&lo, &hi); err != nil {
		return 0, 0, fmt.Errorf("timescale: TradeLedgersInTimeRange: %w", err)
	}
	if !lo.Valid || !hi.Valid {
		return 0, 0, ErrNotFound
	}
	return lo.V, hi.V, nil
}

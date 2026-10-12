package timescale

import (
	"context"
	"fmt"
)

// realTimeCAGGs are the only continuous aggregates allowed to serve their open bucket; both are
// volume counters, not prices. Every other CAGG must be materialized_only, the closed-bucket guard
// for readers with no `bucket <= now() - INTERVAL` predicate of their own.
var realTimeCAGGs = map[string]bool{
	"source_volume_1h":    true,
	"pools_per_source_1h": true,
}

// RealTimeCAGGAllowed reports whether view may run with real-time aggregation.
func RealTimeCAGGAllowed(view string) bool { return realTimeCAGGs[view] }

// OpenBucketCAGGs returns, sorted, every public continuous aggregate serving its in-progress bucket (materialized_only = false) outside the real-time allowlist; empty means the closed-bucket invariant holds.
func (s *Store) OpenBucketCAGGs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT view_name
		  FROM timescaledb_information.continuous_aggregates
		 WHERE view_schema = 'public'
		   AND NOT materialized_only
		 ORDER BY view_name`)
	if err != nil {
		return nil, fmt.Errorf("timescale: OpenBucketCAGGs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("timescale: OpenBucketCAGGs scan: %w", err)
		}
		if !RealTimeCAGGAllowed(name) {
			out = append(out, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: OpenBucketCAGGs rows: %w", err)
	}
	return out, nil
}

package timescale

import (
	"context"
	"fmt"
)

// realTimeCAGGs are the only continuous aggregates allowed to serve their
// open bucket (real-time aggregation, migrations 0069 / 0076). Both are
// volume counters, not prices. Every other CAGG must be materialized_only:
// that property is ADR-0015's closed-bucket guard for the many readers
// that carry no `bucket <= now() - INTERVAL` predicate of their own.
var realTimeCAGGs = map[string]bool{
	"source_volume_1h":    true,
	"pools_per_source_1h": true,
}

// RealTimeCAGGAllowed reports whether view may run with real-time aggregation.
func RealTimeCAGGAllowed(view string) bool { return realTimeCAGGs[view] }

// OpenBucketCAGGs returns, sorted, every public continuous aggregate that
// serves its in-progress bucket (materialized_only = false) and is not in
// the real-time allowlist. Empty means the closed-bucket invariant holds.
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

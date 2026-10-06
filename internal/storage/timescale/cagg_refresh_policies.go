// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CAGGRefreshWindow is one continuous aggregate's refresh-policy
// lookback: how far behind now() a refresh still re-aggregates buckets.
// A row landing with a timestamp older than StartOffset is never
// materialized into the view by the policy.
type CAGGRefreshWindow struct {
	View string
	// HasPolicy is false when the view has no refresh policy job at all.
	HasPolicy bool
	// Unbounded is true when the policy's start_offset is NULL, i.e.
	// every refresh reaches back to the start of the data.
	Unbounded   bool
	StartOffset time.Duration
	// ScheduleInterval is how often the policy runs; zero without one.
	ScheduleInterval time.Duration
	// Hypertable is the raw table the view aggregates; for an aggregate
	// built on another it is that parent's table, e.g. twap_1d -> trades.
	Hypertable string
}

// caggRefreshWindowsSelect lists every continuous aggregate with its
// refresh policy, if any. Refresh jobs name the view on TimescaleDB 2.26
// and its materialization hypertable on older 2.x; match either. The
// offset is read in seconds so no caller parses an interval's text form.
const caggRefreshWindowsSelect = `
	SELECT c.view_name,
	       c.hypertable_name,
	       c.materialization_hypertable_name,
	       j.job_id IS NOT NULL AS has_policy,
	       EXTRACT(EPOCH FROM (j.config->>'start_offset')::interval)::bigint AS start_offset_seconds,
	       COALESCE(EXTRACT(EPOCH FROM j.schedule_interval)::bigint, 0) AS schedule_interval_seconds
	  FROM timescaledb_information.continuous_aggregates c
	  LEFT JOIN timescaledb_information.jobs j
	    ON j.proc_name = 'policy_refresh_continuous_aggregate'
	   AND j.hypertable_name IN (c.view_name, c.materialization_hypertable_name)
	 ORDER BY c.view_name
`

// CAGGRefreshWindows returns the refresh-policy lookback of every
// continuous aggregate in the database, ordered by view name.
func (s *Store) CAGGRefreshWindows(ctx context.Context) ([]CAGGRefreshWindow, error) {
	rows, err := s.db.QueryContext(ctx, caggRefreshWindowsSelect)
	if err != nil {
		return nil, fmt.Errorf("timescale: list cagg refresh policies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CAGGRefreshWindow
	parentOf := map[string]string{} // materialization hypertable -> the table its view reads
	for rows.Next() {
		var (
			w            CAGGRefreshWindow
			materialized string
			seconds      sql.NullInt64
			schedule     int64
		)
		if err := rows.Scan(&w.View, &w.Hypertable, &materialized, &w.HasPolicy, &seconds, &schedule); err != nil {
			return nil, fmt.Errorf("timescale: scan cagg refresh policy: %w", err)
		}
		w.Unbounded = w.HasPolicy && !seconds.Valid
		w.StartOffset = time.Duration(seconds.Int64) * time.Second
		w.ScheduleInterval = time.Duration(schedule) * time.Second
		parentOf[materialized] = w.Hypertable
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: list cagg refresh policies: %w", err)
	}
	for i := range out {
		out[i].Hypertable = rootHypertable(out[i].Hypertable, parentOf)
	}
	return out, nil
}

// rootHypertable follows an aggregate-on-aggregate chain down to the raw
// table. The hop cap only guards a malformed catalogue against looping.
func rootHypertable(table string, parentOf map[string]string) string {
	for range len(parentOf) {
		parent, ok := parentOf[table]
		if !ok {
			break
		}
		table = parent
	}
	return table
}

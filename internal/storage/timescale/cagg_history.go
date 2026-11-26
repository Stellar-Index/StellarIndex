package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// CAGGHistoryStart is one continuous aggregate's materialised history measured against the history its source holds.
type CAGGHistoryStart struct {
	View string
	// Source is the hypertable the view aggregates, or the continuous aggregate a hierarchical view is built on.
	Source string
	// MaterialisedStart is the earliest bucket in the view's materialisation hypertable (never the view, whose
	// real-time tail would read as history); zero when nothing is materialised.
	MaterialisedStart time.Time
	// SourceStart is the source's earliest row, or the parent aggregate's ExpectedStart; zero when the source is empty.
	SourceStart time.Time
	// ExpectedStart is where the view's history should begin: SourceStart, raised by the spec's Floor and by any
	// armed retention on the view or its source. Zero when the source is empty.
	ExpectedStart time.Time
	// Gap is how far MaterialisedStart trails the bucket holding ExpectedStart; now - ExpectedStart when empty.
	Gap time.Duration
	// TradesRetentionPresent reports a retention policy on trades. It never moves ExpectedStart: trades is kept
	// forever (AGENTS.md invariant 8), so such a policy is the defect, not the floor.
	TradesRetentionPresent bool
}

// caggHistorySpec is a registered continuous aggregate with the source its migration builds it from.
type caggHistorySpec struct {
	CAGGSpec
	source string
}

// caggHistorySpecs is every registered continuous aggregate, parents before the views built on them.
func caggHistorySpecs() []caggHistorySpec {
	out := make([]caggHistorySpec, 0, len(TradesCAGGs)+len(OracleCAGGs)+1)
	for _, c := range TradesCAGGs {
		src := "trades"
		if slices.Contains(CAGGsOnPrices1m, c.Name) {
			src = "prices_1m"
		}
		out = append(out, caggHistorySpec{c, src})
	}
	for _, c := range OracleCAGGs {
		out = append(out, caggHistorySpec{c, "oracle_updates"})
	}
	return append(out, caggHistorySpec{SupplyCAGG, "asset_supply_history"})
}

// caggHistorySources are the hypertables a registered view reads; anything else is refused as a min() target.
var caggHistorySources = map[string]bool{"trades": true, "oracle_updates": true, "asset_supply_history": true}

var materialisationTable = regexp.MustCompile(`^_materialized_hypertable_[0-9]+$`)

// timeColumn is a hypertable's first (time) dimension as the catalog reports it.
type timeColumn struct{ schema, table, column string }

// validate refuses any identifier the catalog returned that is not the shape Timescale creates, before it is quoted into SQL.
func (c timeColumn) validate(owner string) error {
	if c.column == "" || c.table == "" || c.schema == "" {
		return fmt.Errorf("timescale: %s: no time dimension in the catalog", owner)
	}
	if caggHistorySources[owner] && c.table == owner {
		return nil
	}
	if allowedCAGGViews[owner] && c.schema == "_timescaledb_internal" && materialisationTable.MatchString(c.table) {
		return nil
	}
	return fmt.Errorf("timescale: %s: refusing unregistered relation %q.%q", owner, c.schema, c.table)
}

// CAGGHistoryStarts measures every registered continuous aggregate's materialised start against its expected start.
func (s *Store) CAGGHistoryStarts(ctx context.Context) ([]CAGGHistoryStart, error) {
	cols, err := s.caggHistoryTimeColumns(ctx)
	if err != nil {
		return nil, err
	}
	retention, tradesRetention, err := s.retentionDropAfter(ctx)
	if err != nil {
		return nil, err
	}
	var now time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		return nil, fmt.Errorf("timescale: CAGGHistoryStarts: now: %w", err)
	}
	sourceStart := map[string]time.Time{}
	out := make([]CAGGHistoryStart, 0, len(cols))
	for _, spec := range caggHistorySpecs() {
		r := CAGGHistoryStart{View: spec.Name, Source: spec.source, TradesRetentionPresent: tradesRetention}
		if r.MaterialisedStart, err = s.minTime(ctx, spec.Name, cols[spec.Name]); err != nil {
			return nil, err
		}
		src, ok := sourceStart[spec.source]
		if !ok {
			if src, err = s.minTime(ctx, spec.source, cols[spec.source]); err != nil {
				return nil, err
			}
			sourceStart[spec.source] = src
		}
		r.SourceStart = src
		if !src.IsZero() {
			r.ExpectedStart = expectedHistoryStart(src, now, spec, retention)
			if r.Gap, err = s.historyGap(ctx, spec.CAGGSpec, r.MaterialisedStart, r.ExpectedStart, now); err != nil {
				return nil, err
			}
		}
		// A view built on this one judges against where this one SHOULD start, not where it does: a parent emptied by
		// the same migration would otherwise make its children read whole.
		sourceStart[spec.Name] = r.ExpectedStart
		out = append(out, r)
	}
	return out, nil
}

// expectedHistoryStart raises src by the spec's Floor and any armed drop_after on the view or its source, except trades.
func expectedHistoryStart(src, now time.Time, spec caggHistorySpec, retention map[string]time.Duration) time.Time {
	start := src
	raise := func(d time.Duration) {
		if d > 0 && now.Add(-d).After(start) {
			start = now.Add(-d)
		}
	}
	raise(spec.Floor)
	raise(retention[spec.Name])
	if spec.source != "trades" {
		raise(retention[spec.source])
	}
	return start
}

// historyGap is how far mat trails the bucket holding expected, computed with the view's own time_bucket.
func (s *Store) historyGap(ctx context.Context, spec CAGGSpec, mat, expected, now time.Time) (time.Duration, error) {
	if mat.IsZero() {
		return max(0, now.Sub(expected)), nil
	}
	floor := expected
	var err error
	switch {
	case spec.Bucket == MonthBucket:
		err = s.db.QueryRowContext(ctx, `SELECT time_bucket('1 month', $1::timestamptz, 'UTC')`, expected).Scan(&floor)
	case spec.Bucket > 0:
		err = s.db.QueryRowContext(ctx, `SELECT time_bucket($1::interval, $2::timestamptz)`,
			fmt.Sprintf("%d seconds", int64(spec.Bucket/time.Second)), expected).Scan(&floor)
	}
	if err != nil {
		return 0, fmt.Errorf("timescale: CAGGHistoryStarts(%s): bucket floor: %w", spec.Name, err)
	}
	return max(0, mat.Sub(floor)), nil
}

// minTime is the earliest value of owner's time dimension, zero when empty; min() on it rides the chunk ordering.
func (s *Store) minTime(ctx context.Context, owner string, c timeColumn) (time.Time, error) {
	if err := c.validate(owner); err != nil {
		return time.Time{}, err
	}
	var t sql.NullTime
	//nolint:gosec // validated above and quoted with pgx.Identifier; a relation cannot be a placeholder
	q := fmt.Sprintf(`SELECT min(%s) FROM %s`, pgx.Identifier{c.column}.Sanitize(), pgx.Identifier{c.schema, c.table}.Sanitize())
	if err := s.db.QueryRowContext(ctx, q).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("timescale: CAGGHistoryStarts(%s): %w", owner, err)
	}
	return t.Time, nil
}

// caggHistoryTimeColumns resolves each registered view to its materialisation hypertable's time column and each
// source hypertable to its own; a registered view missing from the catalog is an error, not a skip.
func (s *Store) caggHistoryTimeColumns(ctx context.Context) (map[string]timeColumn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ca.view_name, d.hypertable_schema, d.hypertable_name, d.column_name
		  FROM timescaledb_information.continuous_aggregates ca
		  JOIN timescaledb_information.dimensions d
		    ON d.hypertable_schema = ca.materialization_hypertable_schema
		   AND d.hypertable_name = ca.materialization_hypertable_name
		   AND d.dimension_number = 1
		 WHERE ca.view_schema = current_schema()
		UNION ALL
		SELECT d.hypertable_name, d.hypertable_schema, d.hypertable_name, d.column_name
		  FROM timescaledb_information.dimensions d
		 WHERE d.hypertable_schema = current_schema()
		   AND d.dimension_number = 1
		   AND d.hypertable_name IN ('trades', 'oracle_updates', 'asset_supply_history')`)
	if err != nil {
		return nil, fmt.Errorf("timescale: CAGGHistoryStarts: catalog: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]timeColumn{}
	for rows.Next() {
		var name string
		var c timeColumn
		if err := rows.Scan(&name, &c.schema, &c.table, &c.column); err != nil {
			return nil, fmt.Errorf("timescale: CAGGHistoryStarts: catalog: %w", err)
		}
		out[name] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: CAGGHistoryStarts: catalog: %w", err)
	}
	for _, spec := range caggHistorySpecs() {
		if _, ok := out[spec.Name]; !ok {
			return nil, fmt.Errorf("timescale: CAGGHistoryStarts: registered view %s is not a continuous aggregate in this schema", spec.Name)
		}
	}
	return out, nil
}

// retentionDropAfter returns each relation's ARMED retention drop_after, and whether trades has a policy at all.
func (s *Store) retentionDropAfter(ctx context.Context) (map[string]time.Duration, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT hypertable_name, scheduled,
		       coalesce(extract(epoch FROM (config->>'drop_after')::interval), 0)::bigint
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention'`)
	if err != nil {
		return nil, false, fmt.Errorf("timescale: CAGGHistoryStarts: retention: %w", err)
	}
	defer func() { _ = rows.Close() }()
	armed := map[string]time.Duration{}
	trades := false
	for rows.Next() {
		var name string
		var scheduled bool
		var secs int64
		if err := rows.Scan(&name, &scheduled, &secs); err != nil {
			return nil, false, fmt.Errorf("timescale: CAGGHistoryStarts: retention: %w", err)
		}
		trades = trades || name == "trades"
		// The shortest armed drop_after is the one that binds.
		if d := time.Duration(secs) * time.Second; scheduled && d > 0 && (armed[name] == 0 || d < armed[name]) {
			armed[name] = d
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("timescale: CAGGHistoryStarts: retention: %w", err)
	}
	return armed, trades, nil
}

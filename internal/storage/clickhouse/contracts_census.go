package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// Census rollup (deploy/clickhouse/contracts_census_daily.sql): plain
// per-day per-contract event counts, recomputed a whole day at a time
// and swapped in with REPLACE PARTITION so re-runs are idempotent (the
// migration-0059 Summing double-count class cannot arise — there is no
// MV and no incremental addition).

// censusDayQuery computes one day's census into the staging table. The
// close_time day predicate prunes on the source's partition/ordering
// via ledger_seq is not available day-wise, so the day filter rides on
// close_time directly; ClickHouse prunes contract_events parts by its
// close_time minmax index. uniqExact over the PK matches the legacy
// census exactly.
const censusDayQuery = `
	INSERT INTO stellar.contracts_census_daily_staging
	SELECT
		toDate(close_time) AS day,
		contract_id,
		toUInt64(uniqExact((ledger_seq, tx_hash, op_index, event_index))) AS events,
		max(ledger_seq)  AS last_ledger,
		max(close_time)  AS last_seen
	FROM stellar.contract_events
	WHERE close_time >= ? AND close_time < ?
	GROUP BY day, contract_id
	SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	         max_bytes_before_external_group_by = 4000000000, max_execution_time = 1800`

// CensusMaxDay returns the newest day present in the census table and
// whether the table has any rows at all.
func CensusMaxDay(ctx context.Context, addr string) (time.Time, bool, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = conn.Close() }()
	rows, err := conn.Query(ctx, `SELECT max(day), count() FROM stellar.contracts_census_daily`)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("clickhouse: census max day: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return time.Time{}, false, rows.Err()
	}
	var maxDay time.Time
	var n uint64
	if err := rows.Scan(&maxDay, &n); err != nil {
		return time.Time{}, false, err
	}
	return maxDay, n > 0, rows.Err()
}

// RunCensusDay recomputes exactly one UTC day of the census and swaps
// it in atomically. Safe to re-run for any day (idempotent replace).
func RunCensusDay(ctx context.Context, addr string, day time.Time, logf func(format string, args ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	dayUTC := day.UTC().Truncate(24 * time.Hour)
	next := dayUTC.Add(24 * time.Hour)
	start := time.Now()

	// Clear any stale staging leftovers for this day, compute, swap.
	// ALTER partition clauses don't take bound parameters on the native
	// protocol; the literal is a Format-produced date, not user input.
	partition := dayUTC.Format("2006-01-02")
	if err := conn.Exec(ctx, fmt.Sprintf(
		"ALTER TABLE stellar.contracts_census_daily_staging DROP PARTITION '%s'", partition)); err != nil {
		return fmt.Errorf("clickhouse: census staging drop %s: %w", partition, err)
	}
	if err := conn.Exec(ctx, censusDayQuery, dayUTC, next); err != nil {
		return fmt.Errorf("clickhouse: census day %s compute: %w", partition, err)
	}
	if err := conn.Exec(ctx, fmt.Sprintf(
		"ALTER TABLE stellar.contracts_census_daily REPLACE PARTITION '%s' FROM stellar.contracts_census_daily_staging", partition)); err != nil {
		return fmt.Errorf("clickhouse: census day %s replace: %w", partition, err)
	}
	logf("census day %s done in %s", partition, time.Since(start).Round(time.Second))
	return nil
}

// EarliestEventDay returns the UTC day of the first contract event in
// the lake — the backfill floor.
func EarliestEventDay(ctx context.Context, addr string) (time.Time, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = conn.Close() }()
	rows, err := conn.Query(ctx, `SELECT toDate(min(close_time)) FROM stellar.contract_events`)
	if err != nil {
		return time.Time{}, fmt.Errorf("clickhouse: earliest event day: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return time.Time{}, rows.Err()
	}
	var d time.Time
	if err := rows.Scan(&d); err != nil {
		return time.Time{}, err
	}
	return d, rows.Err()
}

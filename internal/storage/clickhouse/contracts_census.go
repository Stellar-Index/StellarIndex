package clickhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Census rollup (deploy/clickhouse/contracts_census_daily.sql): plain
// per-day per-contract event counts, recomputed a whole day at a time
// and swapped in with REPLACE PARTITION so re-runs are idempotent (the
// migration-0059 Summing double-count class cannot arise — there is no
// MV and no incremental addition).

// censusExecConn is the subset of driver.Conn RunCensusDay needs. It is
// factored out so the DROP-free CREATE/INSERT/REPLACE/DROP critical
// section can be unit-tested (two goroutines contending) without a live
// ClickHouse — see contracts_census_test.go.
type censusExecConn interface {
	Exec(ctx context.Context, query string, args ...any) error
}

// censusDayInsert computes one day's census into the given PRIVATE
// staging table. The close_time day predicate prunes on the source's
// partition/ordering via ledger_seq is not available day-wise, so the day
// filter rides on close_time directly; ClickHouse prunes contract_events
// parts by its close_time minmax index. uniqExact over the PK matches the
// legacy census exactly. The staging name is a crypto-random suffix minted
// per run (privateStagingTable), never user input, so the Format-built
// identifier is safe.
func censusDayInsert(staging string) string {
	return fmt.Sprintf(`
	INSERT INTO stellar.%s
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
	         max_bytes_before_external_group_by = 4000000000, max_execution_time = 1800`, staging)
}

// privateStagingTable mints a per-run staging table name with a
// crypto-random suffix, so two concurrent census runs (the 30-min timer
// and a manual `ch-census-rollup -backfill`, which are separate processes
// and cannot share an in-process lock) never write to the same staging
// table. This is the W1-chrollup-4 isolation.
func privateStagingTable() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "contracts_census_daily_staging_" + hex.EncodeToString(b[:]), nil
}

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
// it in atomically. Safe to re-run for any day (idempotent replace) AND
// safe to run concurrently with another census run on the same day: each
// run computes into its OWN private staging table, so the 30-min timer
// and a manual `ch-census-rollup -backfill` (a separate process, both
// reaching `today`) can never interleave a DROP/INSERT/REPLACE against a
// shared staging partition. See W1-chrollup-4.
func RunCensusDay(ctx context.Context, addr string, day time.Time, logf func(format string, args ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return runCensusDayConn(ctx, conn, day, logf)
}

// runCensusDayConn is the DDL body of RunCensusDay, split from connection
// setup so the concurrency-isolation property is unit-testable.
func runCensusDayConn(ctx context.Context, conn censusExecConn, day time.Time, logf func(format string, args ...any)) error {
	dayUTC := day.UTC().Truncate(24 * time.Hour)
	next := dayUTC.Add(24 * time.Hour)
	start := time.Now()

	// Compute into a fresh PRIVATE staging table, then atomically swap the
	// day in. Because the staging table is per-run there is no cross-process
	// contention on it; and REPLACE PARTITION into the shared live table is
	// itself atomic (serialized by ClickHouse's per-table alter lock), so two
	// runs recomputing the same day each swap in a COMPLETE partition —
	// last-writer-wins on identical (idempotent) data, and the live partition
	// is never momentarily empty. ALTER/DDL clauses don't take bound
	// parameters on the native protocol; every literal below is either a
	// Format-produced date or a crypto-random staging name, not user input.
	partition := dayUTC.Format("2006-01-02")
	staging, err := privateStagingTable()
	if err != nil {
		return fmt.Errorf("clickhouse: census staging name: %w", err)
	}
	if err := conn.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE stellar.%s AS stellar.contracts_census_daily", staging)); err != nil {
		return fmt.Errorf("clickhouse: census staging create %s: %w", staging, err)
	}
	// Always drop the private staging table, even on error/cancellation, so a
	// crashed run leaves at most one small empty orphan. A detached context
	// makes the cleanup fire even when the parent ctx is already cancelled.
	defer func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if derr := conn.Exec(dropCtx, fmt.Sprintf(
			"DROP TABLE IF EXISTS stellar.%s", staging)); derr != nil {
			logf("census day %s staging cleanup %s failed: %v", partition, staging, derr)
		}
	}()

	if err := conn.Exec(ctx, censusDayInsert(staging), dayUTC, next); err != nil {
		return fmt.Errorf("clickhouse: census day %s compute: %w", partition, err)
	}
	if err := conn.Exec(ctx, fmt.Sprintf(
		"ALTER TABLE stellar.contracts_census_daily REPLACE PARTITION '%s' FROM stellar.%s", partition, staging)); err != nil {
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

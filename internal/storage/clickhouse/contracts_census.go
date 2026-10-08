package clickhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Census rollup (deploy/clickhouse/contracts_census_daily.sql): plain
// per-day per-contract event counts, recomputed a whole day at a time and
// swapped in with REPLACE PARTITION so re-runs are idempotent (no MV, no
// incremental addition, so no Summing double-count).

// censusExecConn is the subset of driver.Conn RunCensusDay needs, so the
// staging/swap critical section can be unit-tested without ClickHouse.
type censusExecConn interface {
	Exec(ctx context.Context, query string, args ...any) error
	QueryRow(ctx context.Context, query string, args ...any) driver.Row
}

// ErrCensusShrink marks a recompute that produced fewer rows or events than
// the live partition holds; the partition is left untouched.
var ErrCensusShrink = errors.New("census day recompute is smaller than the live partition")

// censusDayInsert computes one day's census into the given PRIVATE staging
// table. A day is not expressible in ledger_seq (the sort key), so the filter
// rides on close_time and pruning depends on contract_events' idx_ce_close_time
// minmax index (deploy/clickhouse/tier1_schema.sql); without it the answer is
// still right, from a full scan. The staging name is crypto-random per run
// (privateStagingTable), never user input, so the Format-built identifier is safe.
func censusDayInsert(staging string) string {
	return fmt.Sprintf(`
	INSERT INTO stellar.%s (day, contract_id, events, last_ledger, last_seen)
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

// privateStagingTable mints a per-run staging table name with a crypto-random
// suffix: the 30-min timer and a manual `ch-census-rollup -backfill` are
// separate processes and must never share a staging table.
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

// ContiguousThroughDay returns the UTC day holding the highest ledger the
// lake has with no hole from the start of fromDay: the last day a census
// recompute may read (that day only up to its hole, so it stays the resume
// point). ok is false when fromDay's own first ledger is missing or the
// lake has not reached fromDay.
func ContiguousThroughDay(ctx context.Context, addr string, fromDay time.Time) (time.Time, bool, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = conn.Close() }()

	// Start one past the last ledger closed before fromDay, so a hole at the
	// day boundary is seen; a lake that begins on fromDay starts at its first.
	var before, first uint64
	if err := conn.QueryRow(ctx, `SELECT
		toUInt64(ifNull((SELECT max(ledger_seq) FROM stellar.ledgers WHERE close_time < ?), 0)),
		toUInt64(ifNull((SELECT min(ledger_seq) FROM stellar.ledgers WHERE close_time >= ?), 0))`,
		fromDay, fromDay).Scan(&before, &first); err != nil {
		return time.Time{}, false, fmt.Errorf("clickhouse: census day %s first ledger: %w", fromDay.Format("2006-01-02"), err)
	}
	start := uint32(first) // ledger sequences fit uint32
	if before > 0 {
		start = uint32(before) + 1
	}
	if start == 0 {
		return time.Time{}, false, nil
	}
	tip, err := contiguousWatermarkOn(ctx, conn, start)
	if err != nil || tip < start {
		return time.Time{}, false, err
	}
	var closeTime time.Time
	if err := conn.QueryRow(ctx, `SELECT close_time FROM stellar.ledgers WHERE ledger_seq = ? LIMIT 1`, tip).
		Scan(&closeTime); err != nil {
		return time.Time{}, false, fmt.Errorf("clickhouse: close time of contiguous tip %d: %w", tip, err)
	}
	return closeTime.UTC().Truncate(24 * time.Hour), true, nil
}

// RunCensusDay recomputes exactly one UTC day of the census and swaps it in
// atomically. Idempotent, and safe to run concurrently with another run on
// the same day: each computes into its OWN private staging table.
//
// A recompute smaller than the live partition (fewer contracts or fewer
// events) is refused with ErrCensusShrink unless shrinkOK: the lake only
// grows for a past day, so a shrink means it lost data under the read.
func RunCensusDay(ctx context.Context, addr string, day time.Time, shrinkOK bool, logf func(format string, args ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return runCensusDayConn(ctx, conn, day, shrinkOK, logf)
}

// censusShrinkCheck compares the staging recompute with the live partition
// and fails with ErrCensusShrink when either count went down.
func censusShrinkCheck(ctx context.Context, conn censusExecConn, staging string, day time.Time) error {
	var newRows, newEvents, liveRows, liveEvents uint64
	if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT
		(SELECT count() FROM stellar.%s),
		(SELECT toUInt64(sum(events)) FROM stellar.%s),
		(SELECT count() FROM stellar.contracts_census_daily WHERE day = ?),
		(SELECT toUInt64(sum(events)) FROM stellar.contracts_census_daily WHERE day = ?)`, staging, staging),
		day, day).Scan(&newRows, &newEvents, &liveRows, &liveEvents); err != nil {
		return fmt.Errorf("clickhouse: census day %s size check: %w", day.Format("2006-01-02"), err)
	}
	if newRows < liveRows || newEvents < liveEvents {
		return fmt.Errorf("%w: day %s recomputed %d contract(s) / %d event(s), live has %d / %d — "+
			"the lake lost data under this day; pass -shrink-ok only if that is intended",
			ErrCensusShrink, day.Format("2006-01-02"), newRows, newEvents, liveRows, liveEvents)
	}
	return nil
}

// runCensusDayConn is the DDL body of RunCensusDay, split from connection
// setup so the concurrency-isolation property is unit-testable.
func runCensusDayConn(ctx context.Context, conn censusExecConn, day time.Time, shrinkOK bool, logf func(format string, args ...any)) error {
	dayUTC := day.UTC().Truncate(24 * time.Hour)
	next := dayUTC.Add(24 * time.Hour)
	start := time.Now()

	// Compute into a fresh PRIVATE staging table, then swap the day in.
	// REPLACE PARTITION is atomic (per-table alter lock), so concurrent runs of
	// the same day each swap in a COMPLETE partition. DDL takes no bound
	// parameters on the native protocol; every literal below is a
	// Format-produced date or a crypto-random staging name.
	partition := dayUTC.Format("2006-01-02")
	staging, err := privateStagingTable()
	if err != nil {
		return fmt.Errorf("clickhouse: census staging name: %w", err)
	}
	if err := conn.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE stellar.%s AS stellar.contracts_census_daily", staging)); err != nil {
		return fmt.Errorf("clickhouse: census staging create %s: %w", staging, err)
	}
	// Always drop the staging table, even on error; a detached context lets
	// the cleanup fire after the parent ctx is cancelled.
	defer func() { //nolint:contextcheck // detached cleanup ctx: must fire even when the parent ctx is already cancelled
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
	if !shrinkOK {
		if err := censusShrinkCheck(ctx, conn, staging, dayUTC); err != nil {
			return err
		}
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

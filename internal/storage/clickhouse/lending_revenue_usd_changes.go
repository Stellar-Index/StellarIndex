package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
)

// LendingInterestDay is one (pool, asset, UTC day) of lending_interest_hourly: its newest derived_at and, from a pool
// read, the hour rows of another derive revision (counted as the revenue read counts them, after FINAL).
type LendingInterestDay struct {
	Pool, Asset string
	Day         time.Time
	DerivedAt   time.Time
	StaleRows   uint64
}

// LendingInterestChangedDaysQuery pages through the days by (newest derived_at, contract_id, asset, day), after a cursor,
// so one derive writing more days than a page still moves the cursor. No FINAL: max(derived_at) over every part is the
// newest derive's either way, so the scan reads derived_at and the key columns only.
const LendingInterestChangedDaysQuery = `SELECT contract_id, asset, toDate(hour) AS day, max(derived_at) AS newest
	FROM stellar.lending_interest_hourly
	WHERE derived_at >= fromUnixTimestamp64Milli(?)
	GROUP BY contract_id, asset, day
	HAVING (newest, contract_id, asset, day) > (fromUnixTimestamp64Milli(?), ?, ?, toDate(?))
	ORDER BY newest, contract_id, asset, day
	LIMIT ?`

// LendingInterestChangedDays reads at most limit days ordered after the cursor after (its DerivedAt, Pool, Asset, Day;
// derived_at is stored in ms), oldest derive first.
func (r *ExplorerReader) LendingInterestChangedDays(ctx context.Context, after LendingInterestDay, limit int) ([]LendingInterestDay, error) {
	at := after.DerivedAt.UnixMilli()
	rows, err := r.conn.Query(ctx, LendingInterestChangedDaysQuery, at, at, after.Pool, after.Asset, after.Day.UTC().Format(time.DateOnly), limit)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest changed days: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingInterestDay
	for rows.Next() {
		var d LendingInterestDay
		if err := rows.Scan(&d.Pool, &d.Asset, &d.Day, &d.DerivedAt); err != nil {
			return nil, fmt.Errorf("clickhouse: scan lending interest changed day: %w", err)
		}
		d.Day, d.DerivedAt = d.Day.UTC(), d.DerivedAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// lendingInterestPoolDaysQuery is one pool's days over [from, before): FINAL as the revenue read, so derived_at is the
// surviving derive's and the stale count is the revenue read's countIf(derive_rev != rev).
const lendingInterestPoolDaysQuery = `SELECT asset, toDate(hour) AS day, max(derived_at), countIf(derive_rev != ?)
	FROM (
		SELECT asset, hour, derived_at, derive_rev FROM stellar.lending_interest_hourly FINAL
		WHERE contract_id = ? AND hour >= toDateTime(?, 'UTC') AND hour < toDateTime(?, 'UTC')
	)
	GROUP BY asset, day
	ORDER BY asset, day
	SETTINGS optimize_move_to_prewhere_if_final = 0`

// LendingInterestPoolDays reads pool's interest days over [from, before). Read it BEFORE the rows it describes: a derive
// landing in between is then newer than what is recorded, and seen again.
func (r *ExplorerReader) LendingInterestPoolDays(ctx context.Context, pool string, from, before time.Time) ([]LendingInterestDay, error) {
	rows, err := r.conn.Query(ctx, lendingInterestPoolDaysQuery, lendinginterest.DeriveRev, pool, from.Unix(), before.Unix())
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest days %s: %w", pool, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingInterestDay
	for rows.Next() {
		d := LendingInterestDay{Pool: pool}
		if err := rows.Scan(&d.Asset, &d.Day, &d.DerivedAt, &d.StaleRows); err != nil {
			return nil, fmt.Errorf("clickhouse: scan lending interest day %s: %w", pool, err)
		}
		d.Day, d.DerivedAt = d.Day.UTC(), d.DerivedAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// LendingInterestPools are the pools with an interest coverage row.
func (r *ExplorerReader) LendingInterestPools(ctx context.Context) ([]string, error) {
	rows, err := r.conn.Query(ctx, `SELECT DISTINCT name FROM stellar.entry_history_watermark
		WHERE startsWith(name, ?) ORDER BY name`, lendingInterestPoolPrefix)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest pools: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("clickhouse: scan lending interest pool: %w", err)
		}
		out = append(out, strings.TrimPrefix(name, lendingInterestPoolPrefix))
	}
	return out, rows.Err()
}

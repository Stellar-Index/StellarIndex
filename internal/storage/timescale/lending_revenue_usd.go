package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// LendingRevenueRollupLockName is the advisory lock the API's lending revenue rollup worker holds while it writes, so one
// process writes lending_revenue_usd_daily however many API instances share the database.
const LendingRevenueRollupLockName = "instance:stellarindex-api:lending-revenue-rollup"

// LendingRevenueUSDDay is one lending_revenue_usd_daily row. Legs are exact decimal strings, all nil when no hour was
// priced; BasisHours counts priced hours per price basis.
type LendingRevenueUSDDay struct {
	Pool, Asset                     string
	Day                             time.Time
	InterestDeriveRev, ValuationRev int16
	InterestDerivedAt               time.Time
	Legs                            [7]*string
	HoursValued, HoursUnvalued      int
	HoursCarried                    int
	BasisHours                      map[string]int
	StaleRows, AccrualExcluded      int64
	UnexplainedLoss                 int64
	WalkReason                      *string
	ValuedAt                        time.Time
}

// LendingRevenueUSDCoverage is one lending_revenue_usd_coverage row; RedoFromDay is zero when no redo clips the pool.
type LendingRevenueUSDCoverage struct {
	Pool                            string
	FloorDay, ThroughDay, RedoFrom  time.Time
	InterestDeriveRev, ValuationRev int16
}

const lendingRevenueUSDDayCols = `pool, asset, day, interest_derive_rev, valuation_rev, interest_derived_at,
	interest_accrued_usd, supplier_share_usd, supplier_credited_usd, backstop_take_usd, protocol_fee_usd,
	backstop_drawn_usd, default_loss_usd, hours_valued, hours_unvalued, hours_carried, price_basis_hours,
	stale_rows, accrual_excluded_versions, unexplained_loss_versions, walk_reason, valued_at`

// LendingRevenueUSDDays reads a pool's stored days in [from, through], ascending.
func (s *Store) LendingRevenueUSDDays(ctx context.Context, pool string, from, through time.Time) ([]LendingRevenueUSDDay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+lendingRevenueUSDDayCols+`
		FROM lending_revenue_usd_daily WHERE pool = $1 AND day BETWEEN $2::date AND $3::date ORDER BY asset, day`,
		pool, from.UTC().Format(time.DateOnly), through.UTC().Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("timescale: lending revenue usd days %s: %w", pool, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingRevenueUSDDay
	for rows.Next() {
		var d LendingRevenueUSDDay
		var legs [7]sql.NullString
		var basis []byte
		var reason sql.NullString
		if err := rows.Scan(&d.Pool, &d.Asset, &d.Day, &d.InterestDeriveRev, &d.ValuationRev, &d.InterestDerivedAt,
			&legs[0], &legs[1], &legs[2], &legs[3], &legs[4], &legs[5], &legs[6], &d.HoursValued, &d.HoursUnvalued,
			&d.HoursCarried, &basis, &d.StaleRows, &d.AccrualExcluded, &d.UnexplainedLoss, &reason, &d.ValuedAt); err != nil {
			return nil, fmt.Errorf("timescale: scan lending revenue usd day: %w", err)
		}
		for i, l := range legs {
			if l.Valid {
				d.Legs[i] = &l.String
			}
		}
		if err := json.Unmarshal(basis, &d.BasisHours); err != nil {
			return nil, fmt.Errorf("timescale: lending revenue usd basis hours: %w", err)
		}
		if reason.Valid {
			d.WalkReason = &reason.String
		}
		d.Day, d.InterestDerivedAt = d.Day.UTC(), d.InterestDerivedAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpsertLendingRevenueUSDDays writes rows in one transaction; rewriting a day with the same values changes only valued_at.
func (s *Store) UpsertLendingRevenueUSDDays(ctx context.Context, rows []LendingRevenueUSDDay) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: begin lending revenue usd upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, d := range rows {
		basis, err := json.Marshal(d.BasisHours)
		if err != nil {
			return fmt.Errorf("timescale: lending revenue usd basis hours: %w", err)
		}
		if d.BasisHours == nil {
			basis = []byte("{}")
		}
		args := []any{d.Pool, d.Asset, d.Day.UTC().Format(time.DateOnly), d.InterestDeriveRev, d.ValuationRev, d.InterestDerivedAt.UTC()}
		for _, l := range d.Legs {
			args = append(args, l)
		}
		args = append(args, d.HoursValued, d.HoursUnvalued, d.HoursCarried, string(basis), d.StaleRows, d.AccrualExcluded,
			d.UnexplainedLoss, d.WalkReason)
		if _, err := tx.ExecContext(ctx, `INSERT INTO lending_revenue_usd_daily (`+lendingRevenueUSDDayCols+`)
			VALUES ($1, $2, $3::date, $4, $5, $6, $7::numeric, $8::numeric, $9::numeric, $10::numeric, $11::numeric,
				$12::numeric, $13::numeric, $14, $15, $16, $17::jsonb, $18, $19, $20, $21, now())
			ON CONFLICT (pool, asset, day) DO UPDATE SET
				interest_derive_rev = EXCLUDED.interest_derive_rev, valuation_rev = EXCLUDED.valuation_rev,
				interest_derived_at = EXCLUDED.interest_derived_at, interest_accrued_usd = EXCLUDED.interest_accrued_usd,
				supplier_share_usd = EXCLUDED.supplier_share_usd, supplier_credited_usd = EXCLUDED.supplier_credited_usd,
				backstop_take_usd = EXCLUDED.backstop_take_usd, protocol_fee_usd = EXCLUDED.protocol_fee_usd,
				backstop_drawn_usd = EXCLUDED.backstop_drawn_usd, default_loss_usd = EXCLUDED.default_loss_usd,
				hours_valued = EXCLUDED.hours_valued, hours_unvalued = EXCLUDED.hours_unvalued,
				hours_carried = EXCLUDED.hours_carried, price_basis_hours = EXCLUDED.price_basis_hours,
				stale_rows = EXCLUDED.stale_rows, accrual_excluded_versions = EXCLUDED.accrual_excluded_versions,
				unexplained_loss_versions = EXCLUDED.unexplained_loss_versions, walk_reason = EXCLUDED.walk_reason,
				valued_at = now()`, args...); err != nil {
			return fmt.Errorf("timescale: upsert lending revenue usd %s %s %s: %w", d.Pool, d.Asset, d.Day.Format(time.DateOnly), err)
		}
	}
	return tx.Commit()
}

// LendingRevenueUSDStaleRevDays are the (pool, day) pairs holding a row of another valuation or derive revision, oldest
// first, at most limit.
func (s *Store) LendingRevenueUSDStaleRevDays(ctx context.Context, valuationRev, deriveRev int16, limit int) (map[string][]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT pool, day FROM lending_revenue_usd_daily
		WHERE valuation_rev <> $1 OR interest_derive_rev <> $2 ORDER BY day, pool LIMIT $3`, valuationRev, deriveRev, limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: lending revenue usd stale revisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]time.Time{}
	for rows.Next() {
		var pool string
		var day time.Time
		if err := rows.Scan(&pool, &day); err != nil {
			return nil, fmt.Errorf("timescale: scan lending revenue usd stale revision: %w", err)
		}
		out[pool] = append(out[pool], day.UTC())
	}
	return out, rows.Err()
}

// LendingRevenueUSDNewestDerivedAt is the newest interest_derived_at stored, zero when the table is empty.
func (s *Store) LendingRevenueUSDNewestDerivedAt(ctx context.Context) (time.Time, error) {
	var at sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT max(interest_derived_at) FROM lending_revenue_usd_daily`).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("timescale: lending revenue usd newest derived_at: %w", err)
	}
	return at.Time.UTC(), nil
}

// LendingRevenueUSDCoverages reads every pool's coverage row.
func (s *Store) LendingRevenueUSDCoverages(ctx context.Context) (map[string]LendingRevenueUSDCoverage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pool, floor_day, through_day, redo_from_day, interest_derive_rev, valuation_rev
		FROM lending_revenue_usd_coverage`)
	if err != nil {
		return nil, fmt.Errorf("timescale: lending revenue usd coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]LendingRevenueUSDCoverage{}
	for rows.Next() {
		var c LendingRevenueUSDCoverage
		var redo sql.NullTime
		if err := rows.Scan(&c.Pool, &c.FloorDay, &c.ThroughDay, &redo, &c.InterestDeriveRev, &c.ValuationRev); err != nil {
			return nil, fmt.Errorf("timescale: scan lending revenue usd coverage: %w", err)
		}
		c.FloorDay, c.ThroughDay = c.FloorDay.UTC(), c.ThroughDay.UTC()
		if redo.Valid {
			c.RedoFrom = redo.Time.UTC()
		}
		out[c.Pool] = c
	}
	return out, rows.Err()
}

// UpsertLendingRevenueUSDCoverage writes one pool's coverage row.
func (s *Store) UpsertLendingRevenueUSDCoverage(ctx context.Context, c LendingRevenueUSDCoverage) error {
	var redo any
	if !c.RedoFrom.IsZero() {
		redo = c.RedoFrom.UTC().Format(time.DateOnly)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO lending_revenue_usd_coverage
		(pool, floor_day, through_day, redo_from_day, interest_derive_rev, valuation_rev, updated_at)
		VALUES ($1, $2::date, $3::date, $4::date, $5, $6, now())
		ON CONFLICT (pool) DO UPDATE SET floor_day = EXCLUDED.floor_day, through_day = EXCLUDED.through_day,
			redo_from_day = EXCLUDED.redo_from_day, interest_derive_rev = EXCLUDED.interest_derive_rev,
			valuation_rev = EXCLUDED.valuation_rev, updated_at = now()`,
		c.Pool, c.FloorDay.UTC().Format(time.DateOnly), c.ThroughDay.UTC().Format(time.DateOnly), redo, c.InterestDeriveRev, c.ValuationRev)
	if err != nil {
		return fmt.Errorf("timescale: upsert lending revenue usd coverage %s: %w", c.Pool, err)
	}
	return nil
}

// SourceExclusionWindowsChangedSince are the [from, until) windows of source exclusions created or lifted after since;
// until is zero for an open-ended one.
func (s *Store) SourceExclusionWindowsChangedSince(ctx context.Context, since time.Time) ([][2]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT from_ts, until_ts FROM source_exclusions
		WHERE created_at > $1 OR lifted_at > $1 ORDER BY from_ts`, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("timescale: source exclusions changed since %s: %w", since, err)
	}
	defer func() { _ = rows.Close() }()
	var out [][2]time.Time
	for rows.Next() {
		var from time.Time
		var until sql.NullTime
		if err := rows.Scan(&from, &until); err != nil {
			return nil, fmt.Errorf("timescale: scan source exclusion window: %w", err)
		}
		out = append(out, [2]time.Time{from.UTC(), until.Time.UTC()})
	}
	return out, rows.Err()
}

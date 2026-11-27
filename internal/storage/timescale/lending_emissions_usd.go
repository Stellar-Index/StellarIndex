package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// LendingEmissionsRollupLockName is the advisory lock the API's lending emissions rollup worker holds while it writes, so
// one process writes lending_emissions_usd_daily however many API instances share the database.
const LendingEmissionsRollupLockName = "instance:stellarindex-api:lending-emissions-rollup"

// LendingEmissionsUSDDay is one lending_emissions_usd_daily row: one stream line's emission tokens on one UTC day and
// their exact USD, nil exactly when USDReason says why.
type LendingEmissionsUSDDay struct {
	Pool                   string
	Day                    time.Time
	Stream, Line, Protocol string
	EmissionToken          *string
	EmissionTokenDecimals  *int
	Asset, Side            *string
	Events                 *int64
	Amount                 string
	AmountUSD              *string
	HoursValued            int
	HoursUnvalued          int
	HoursCarried           int
	PriceBasis             *string
	BasisHours             map[string]int
	LowerBound             bool
	StreamLowerBound       bool
	Excluded               []string
	Approximate            bool
	ApproximateReason      *string
	USDReason              *string
	ValuationRev           int16
	ValuedAt               time.Time
}

// LendingEmissionsUSDCoverage is one lending_emissions_usd_coverage row; Unavailable maps each stream the pool does not
// serve to its reason.
type LendingEmissionsUSDCoverage struct {
	Pool, Protocol       string
	FloorDay, ThroughDay time.Time
	Unavailable          map[string]string
	ValuationRev         int16
}

const lendingEmissionsUSDDayCols = `pool, day, stream, line, protocol, emission_token, emission_token_decimals, asset, side,
	events, amount::text, amount_usd::text, hours_valued, hours_unvalued, hours_carried, price_basis, price_basis_hours,
	lower_bound, stream_lower_bound, excluded, approximate, approximate_reason, usd_reason, valuation_rev, valued_at`

// LendingEmissionsUSDDays reads a pool's stored rows for the days in [from, through], by day, stream and line.
func (s *Store) LendingEmissionsUSDDays(ctx context.Context, pool string, from, through time.Time) ([]LendingEmissionsUSDDay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+lendingEmissionsUSDDayCols+`
		FROM lending_emissions_usd_daily WHERE pool = $1 AND day BETWEEN $2::date AND $3::date ORDER BY day, stream, line`,
		pool, from.UTC().Format(time.DateOnly), through.UTC().Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("timescale: lending emissions usd days %s: %w", pool, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingEmissionsUSDDay
	for rows.Next() {
		d, err := scanLendingEmissionsUSDDay(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanLendingEmissionsUSDDay(rows *sql.Rows) (LendingEmissionsUSDDay, error) {
	var d LendingEmissionsUSDDay
	var token, asset, side, usd, basis, approxReason, reason sql.NullString
	var decimals sql.NullInt32
	var events sql.NullInt64
	var basisHours, excluded []byte
	if err := rows.Scan(&d.Pool, &d.Day, &d.Stream, &d.Line, &d.Protocol, &token, &decimals, &asset, &side, &events,
		&d.Amount, &usd, &d.HoursValued, &d.HoursUnvalued, &d.HoursCarried, &basis, &basisHours, &d.LowerBound,
		&d.StreamLowerBound, &excluded, &d.Approximate, &approxReason, &reason, &d.ValuationRev, &d.ValuedAt); err != nil {
		return d, fmt.Errorf("timescale: scan lending emissions usd day: %w", err)
	}
	d.EmissionToken, d.Asset, d.Side = nullStringPtr(token), nullStringPtr(asset), nullStringPtr(side)
	d.AmountUSD, d.PriceBasis = nullStringPtr(usd), nullStringPtr(basis)
	d.ApproximateReason, d.USDReason = nullStringPtr(approxReason), nullStringPtr(reason)
	if decimals.Valid {
		n := int(decimals.Int32)
		d.EmissionTokenDecimals = &n
	}
	if events.Valid {
		d.Events = &events.Int64
	}
	if err := json.Unmarshal(basisHours, &d.BasisHours); err != nil {
		return d, fmt.Errorf("timescale: lending emissions usd basis hours: %w", err)
	}
	if err := json.Unmarshal(excluded, &d.Excluded); err != nil {
		return d, fmt.Errorf("timescale: lending emissions usd excluded: %w", err)
	}
	d.Day = d.Day.UTC()
	return d, nil
}

// ReplaceLendingEmissionsUSDDays makes rows the whole stored state of the pool's days, in one transaction: a line no
// longer in a day is deleted, so a stored day never keeps tokens its source no longer has.
func (s *Store) ReplaceLendingEmissionsUSDDays(ctx context.Context, pool string, days []time.Time, rows []LendingEmissionsUSDDay) error {
	if len(days) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: begin lending emissions usd replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	dates := make([]string, len(days))
	for i, d := range days {
		dates[i] = d.UTC().Format(time.DateOnly)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM lending_emissions_usd_daily WHERE pool = $1 AND day = ANY($2::text[]::date[])`,
		pool, dates); err != nil {
		return fmt.Errorf("timescale: clear lending emissions usd days %s: %w", pool, err)
	}
	for _, d := range rows {
		if d.Pool != pool {
			return fmt.Errorf("timescale: lending emissions usd row of %s in a replace of %s", d.Pool, pool)
		}
		basis := []byte("{}")
		if d.BasisHours != nil {
			if basis, err = json.Marshal(d.BasisHours); err != nil {
				return fmt.Errorf("timescale: lending emissions usd basis hours: %w", err)
			}
		}
		excluded := []byte("[]")
		if len(d.Excluded) > 0 {
			if excluded, err = json.Marshal(d.Excluded); err != nil {
				return fmt.Errorf("timescale: lending emissions usd excluded: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lending_emissions_usd_daily (pool, day, stream, line, protocol,
				emission_token, emission_token_decimals, asset, side, events, amount, amount_usd, hours_valued, hours_unvalued,
				hours_carried, price_basis, price_basis_hours, lower_bound, stream_lower_bound, excluded, approximate,
				approximate_reason, usd_reason, valuation_rev, valued_at)
			VALUES ($1, $2::date, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12::numeric, $13, $14, $15, $16,
				$17::jsonb, $18, $19, $20::jsonb, $21, $22, $23, $24, now())`,
			d.Pool, d.Day.UTC().Format(time.DateOnly), d.Stream, d.Line, d.Protocol, d.EmissionToken, d.EmissionTokenDecimals,
			d.Asset, d.Side, d.Events, d.Amount, d.AmountUSD, d.HoursValued, d.HoursUnvalued, d.HoursCarried, d.PriceBasis,
			string(basis), d.LowerBound, d.StreamLowerBound, string(excluded), d.Approximate, d.ApproximateReason, d.USDReason,
			d.ValuationRev); err != nil {
			return fmt.Errorf("timescale: insert lending emissions usd %s %s %s/%s: %w", d.Pool, d.Day.Format(time.DateOnly), d.Stream, d.Line, err)
		}
	}
	return tx.Commit()
}

// LendingEmissionsUSDStaleRevDays are the (pool, day) pairs holding a row of another valuation revision, oldest first,
// at most limit.
func (s *Store) LendingEmissionsUSDStaleRevDays(ctx context.Context, valuationRev int16, limit int) (map[string][]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT pool, day FROM lending_emissions_usd_daily
		WHERE valuation_rev <> $1 ORDER BY day, pool LIMIT $2`, valuationRev, limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: lending emissions usd stale revisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]time.Time{}
	for rows.Next() {
		var pool string
		var day time.Time
		if err := rows.Scan(&pool, &day); err != nil {
			return nil, fmt.Errorf("timescale: scan lending emissions usd stale revision: %w", err)
		}
		out[pool] = append(out[pool], day.UTC())
	}
	return out, rows.Err()
}

// LendingEmissionsUSDCoverages reads every pool's coverage row.
func (s *Store) LendingEmissionsUSDCoverages(ctx context.Context) (map[string]LendingEmissionsUSDCoverage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pool, protocol, floor_day, through_day, unavailable, valuation_rev
		FROM lending_emissions_usd_coverage`)
	if err != nil {
		return nil, fmt.Errorf("timescale: lending emissions usd coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]LendingEmissionsUSDCoverage{}
	for rows.Next() {
		var c LendingEmissionsUSDCoverage
		var unavailable []byte
		if err := rows.Scan(&c.Pool, &c.Protocol, &c.FloorDay, &c.ThroughDay, &unavailable, &c.ValuationRev); err != nil {
			return nil, fmt.Errorf("timescale: scan lending emissions usd coverage: %w", err)
		}
		if err := json.Unmarshal(unavailable, &c.Unavailable); err != nil {
			return nil, fmt.Errorf("timescale: lending emissions usd coverage unavailable: %w", err)
		}
		c.FloorDay, c.ThroughDay = c.FloorDay.UTC(), c.ThroughDay.UTC()
		out[c.Pool] = c
	}
	return out, rows.Err()
}

// UpsertLendingEmissionsUSDCoverage writes one pool's coverage row.
func (s *Store) UpsertLendingEmissionsUSDCoverage(ctx context.Context, c LendingEmissionsUSDCoverage) error {
	unavailable := []byte("{}")
	if c.Unavailable != nil {
		var err error
		if unavailable, err = json.Marshal(c.Unavailable); err != nil {
			return fmt.Errorf("timescale: lending emissions usd coverage unavailable: %w", err)
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO lending_emissions_usd_coverage
		(pool, protocol, floor_day, through_day, unavailable, valuation_rev, updated_at)
		VALUES ($1, $2, $3::date, $4::date, $5::jsonb, $6, now())
		ON CONFLICT (pool) DO UPDATE SET protocol = EXCLUDED.protocol, floor_day = EXCLUDED.floor_day,
			through_day = EXCLUDED.through_day, unavailable = EXCLUDED.unavailable, valuation_rev = EXCLUDED.valuation_rev,
			updated_at = now()`,
		c.Pool, c.Protocol, c.FloorDay.UTC().Format(time.DateOnly), c.ThroughDay.UTC().Format(time.DateOnly), string(unavailable), c.ValuationRev)
	if err != nil {
		return fmt.Errorf("timescale: upsert lending emissions usd coverage %s: %w", c.Pool, err)
	}
	return nil
}

// LendingEmissionPools is every pool an emission event names, with its first such event's close time: Blend's pool
// claims and schedule updates and its backstop's allocations, and BLNT's pool and backstop events naming it.
func (s *Store) LendingEmissionPools(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pool, min(t) FROM (
		SELECT pool, min(ledger_close_time) AS t FROM blend_emissions
		 WHERE event_kind IN ('claim', 'reserve_emission_update') GROUP BY pool
		UNION ALL
		SELECT contract_id, min(ledger_close_time) FROM blnt_events
		 WHERE contract_role = 'pool' AND event_kind IN ('claim', 'reserve_emission_update') GROUP BY contract_id
		UNION ALL
		SELECT pool, min(ledger_close_time) FROM blend_backstop_events
		 WHERE event_kind = 'gulp_emissions' AND pool IS NOT NULL GROUP BY pool
		UNION ALL
		SELECT pool, min(ledger_close_time) FROM blnt_events
		 WHERE contract_role = 'backstop' AND event_kind IN ('gulp_emissions', 'claim') AND pool IS NOT NULL GROUP BY pool
	) e GROUP BY pool`)
	if err != nil {
		return nil, fmt.Errorf("timescale: lending emission pools: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var pool string
		var first time.Time
		if err := rows.Scan(&pool, &first); err != nil {
			return nil, fmt.Errorf("timescale: scan lending emission pool: %w", err)
		}
		out[pool] = first.UTC()
	}
	return out, rows.Err()
}

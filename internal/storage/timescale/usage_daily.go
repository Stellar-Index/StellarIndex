package timescale

import (
	"context"
	"fmt"
	"time"

	"github.com/StellarIndex/stellar-index/internal/usage"
)

// UpsertUsageDaily merges a batch of per-(day, subject, endpoint)
// usage aggregates into the `usage_daily` hypertable (migration
// 0071). Satisfies [usage.RollupSink] — the API binary's rollup
// worker calls this every sweep with CUMULATIVE per-day counters.
//
// GREATEST()-merge, not overwrite: the Redis counters only grow
// within a day, so re-sweeping the same window is a no-op, and a
// mid-day Redis flush (counter reset to a lower value) can never
// regress an already-persisted row. One transaction per batch so a
// partial failure leaves the previous sweep's state intact.
func (s *Store) UpsertUsageDaily(ctx context.Context, rows []usage.RollupRow) error {
	if len(rows) == 0 {
		return nil
	}
	const q = `
        INSERT INTO usage_daily (
            day, subject, endpoint,
            ok_count, client_error_count, server_error_count, throttled_count
        ) VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (day, subject, endpoint) DO UPDATE SET
            ok_count           = GREATEST(usage_daily.ok_count,           EXCLUDED.ok_count),
            client_error_count = GREATEST(usage_daily.client_error_count, EXCLUDED.client_error_count),
            server_error_count = GREATEST(usage_daily.server_error_count, EXCLUDED.server_error_count),
            throttled_count    = GREATEST(usage_daily.throttled_count,    EXCLUDED.throttled_count),
            updated_at         = now()
    `
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: UpsertUsageDaily: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("timescale: UpsertUsageDaily: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range rows {
		if r.Day == "" || r.Subject == "" || r.Endpoint == "" {
			return fmt.Errorf("timescale: UpsertUsageDaily: incomplete row %+v", r)
		}
		if _, err := stmt.ExecContext(ctx,
			r.Day, r.Subject, r.Endpoint,
			r.OK, r.ClientErrors, r.ServerErrors, r.Throttled,
		); err != nil {
			return fmt.Errorf("timescale: UpsertUsageDaily: %s/%s/%s: %w",
				r.Day, r.Subject, r.Endpoint, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: UpsertUsageDaily: commit: %w", err)
	}
	return nil
}

// UsageDailyRow is one persisted (day, subject, endpoint) aggregate
// read back from `usage_daily`. Counts mirror the table columns —
// the API layer derives requests = OK + ClientErrors + ServerErrors
// and errors = ClientErrors + ServerErrors.
type UsageDailyRow struct {
	Day          string // YYYY-MM-DD UTC
	Subject      string
	Endpoint     string
	OK           int64
	ClientErrors int64
	ServerErrors int64
	Throttled    int64
}

// ReadUsageDaily returns the subject's per-endpoint rollups for the
// trailing `days` window (inclusive of today), oldest day first,
// endpoints alphabetical within a day. Backs /v1/account/usage via
// the main.go adapter onto the v1.UsageRollupReader seam.
func (s *Store) ReadUsageDaily(ctx context.Context, subject string, days int) ([]UsageDailyRow, error) {
	if subject == "" || days <= 0 {
		return nil, nil
	}
	const q = `
        SELECT day, endpoint,
               ok_count, client_error_count, server_error_count, throttled_count
          FROM usage_daily
         WHERE subject = $1
           AND day > (now() AT TIME ZONE 'utc')::date - $2::int
         ORDER BY day ASC, endpoint ASC
    `
	rows, err := s.db.QueryContext(ctx, q, subject, days)
	if err != nil {
		return nil, fmt.Errorf("timescale: ReadUsageDaily: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageDailyRow
	for rows.Next() {
		var (
			day time.Time
			r   UsageDailyRow
		)
		if err := rows.Scan(&day, &r.Endpoint,
			&r.OK, &r.ClientErrors, &r.ServerErrors, &r.Throttled); err != nil {
			return nil, fmt.Errorf("timescale: ReadUsageDaily: scan: %w", err)
		}
		r.Day = day.UTC().Format("2006-01-02")
		r.Subject = subject
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ReadUsageDaily: rows: %w", err)
	}
	return out, nil
}

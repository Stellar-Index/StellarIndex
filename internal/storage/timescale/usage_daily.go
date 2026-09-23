package timescale

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// usageDailyUpsertChunk bounds one multi-row upsert statement. At 7
// bind parameters per row a chunk stays far below the wire protocol's
// 65535-parameter cap while amortising the per-statement round-trip
// over hundreds of rows.
const usageDailyUpsertChunk = 500

// usageDailyColumns is the bind-parameter width of one usage_daily row.
const usageDailyColumns = 7

// UpsertUsageDaily merges a batch of per-(day, subject, endpoint)
// usage aggregates into the `usage_daily` hypertable (migration
// 0071). Satisfies [usage.RollupSink] — the API binary's rollup
// worker calls this every sweep with CUMULATIVE per-day counters.
//
// GREATEST()-merge, not overwrite: the Redis counters only grow
// within a day, so re-sweeping the same window is a no-op, and a
// mid-day Redis flush (counter reset to a lower value) can never
// regress an already-persisted row.
//
// Rows go to Postgres as multi-row INSERTs of at most
// usageDailyUpsertChunk rows, one autocommitted statement per chunk,
// so the round-trip count and the transaction size are both bounded
// by the chunk, not by the batch. A failure mid-batch leaves the
// earlier chunks committed: every row is an independently valid
// cumulative value and the next sweep re-sends whatever did not land,
// so a partial batch is never a wrong state, only an incomplete one.
// The batch is validated whole before the first statement is sent.
func (s *Store) UpsertUsageDaily(ctx context.Context, rows []usage.RollupRow) error {
	rows, err := foldUsageDailyRows(rows)
	if err != nil {
		return err
	}
	for start := 0; start < len(rows); start += usageDailyUpsertChunk {
		end := min(start+usageDailyUpsertChunk, len(rows))
		if err := s.upsertUsageDailyChunk(ctx, rows[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// foldUsageDailyRows rejects an incomplete row and folds duplicate
// (day, subject, endpoint) keys into one row holding the per-column
// maximum — the same result the GREATEST merge would reach applying
// them one at a time, and necessary because Postgres refuses to touch
// the same row twice in one ON CONFLICT statement. First-seen order
// is kept.
func foldUsageDailyRows(rows []usage.RollupRow) ([]usage.RollupRow, error) {
	type key struct{ day, subject, endpoint string }
	index := make(map[key]int, len(rows))
	out := make([]usage.RollupRow, 0, len(rows))
	for _, r := range rows {
		if r.Day == "" || r.Subject == "" || r.Endpoint == "" {
			return nil, fmt.Errorf("timescale: UpsertUsageDaily: incomplete row %+v", r)
		}
		k := key{r.Day, r.Subject, r.Endpoint}
		i, dup := index[k]
		if !dup {
			index[k] = len(out)
			out = append(out, r)
			continue
		}
		out[i].OK = max(out[i].OK, r.OK)
		out[i].ClientErrors = max(out[i].ClientErrors, r.ClientErrors)
		out[i].ServerErrors = max(out[i].ServerErrors, r.ServerErrors)
		out[i].Throttled = max(out[i].Throttled, r.Throttled)
	}
	return out, nil
}

// upsertUsageDailyChunk sends one chunk as a single multi-row upsert.
func (s *Store) upsertUsageDailyChunk(ctx context.Context, rows []usage.RollupRow) error {
	var q strings.Builder
	q.WriteString(`
        INSERT INTO usage_daily (
            day, subject, endpoint,
            ok_count, client_error_count, server_error_count, throttled_count
        ) VALUES `)
	args := make([]any, 0, len(rows)*usageDailyColumns)
	for i, r := range rows {
		if i > 0 {
			q.WriteString(", ")
		}
		q.WriteByte('(')
		for c := 0; c < usageDailyColumns; c++ {
			if c > 0 {
				q.WriteString(", ")
			}
			q.WriteByte('$')
			q.WriteString(strconv.Itoa(len(args) + c + 1))
		}
		q.WriteByte(')')
		args = append(args,
			r.Day, r.Subject, r.Endpoint,
			r.OK, r.ClientErrors, r.ServerErrors, r.Throttled)
	}
	q.WriteString(`
        ON CONFLICT (day, subject, endpoint) DO UPDATE SET
            ok_count           = GREATEST(usage_daily.ok_count,           EXCLUDED.ok_count),
            client_error_count = GREATEST(usage_daily.client_error_count, EXCLUDED.client_error_count),
            server_error_count = GREATEST(usage_daily.server_error_count, EXCLUDED.server_error_count),
            throttled_count    = GREATEST(usage_daily.throttled_count,    EXCLUDED.throttled_count),
            updated_at         = now()
    `)
	if _, err := s.db.ExecContext(ctx, q.String(), args...); err != nil {
		first := rows[0]
		return fmt.Errorf("timescale: UpsertUsageDaily: %d rows from %s/%s/%s: %w",
			len(rows), first.Day, first.Subject, first.Endpoint, err)
	}
	return nil
}

// UsageDailyRow is one persisted (day, subject, endpoint) aggregate
// read back from `usage_daily`. Counts mirror the table columns —
// the API layer derives requests = OK + ClientErrors + ServerErrors,
// billable = OK + ClientErrors and errors = ClientErrors + ServerErrors.
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

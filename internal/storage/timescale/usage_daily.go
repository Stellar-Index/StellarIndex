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

// UpsertUsageDaily merges per-(day, subject, endpoint) usage aggregates into usage_daily for usage.RollupSink; the merge is GREATEST(), not overwrite, because the Redis counters only grow within a day.
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

// foldUsageDailyRows rejects an incomplete row and folds duplicate (day, subject, endpoint) keys
// into one row of per-column maxima, matching what the GREATEST merge would reach row by row.
// Folding is required because Postgres refuses to touch the same row twice in one ON CONFLICT
// statement. First-seen order is kept.
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

// UsageDailyRow is one persisted (day, subject, endpoint) aggregate read back from `usage_daily`.
type UsageDailyRow struct {
	Day          string // YYYY-MM-DD UTC
	Subject      string
	Endpoint     string
	OK           int64
	ClientErrors int64
	ServerErrors int64
	Throttled    int64
}

// ReadUsageDaily returns the subject's per-endpoint rollups for the trailing `days` window (inclusive of today), oldest day first, backing /v1/account/usage.
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

// BillableByDay implements usage.DailyBillableReader: the subject's billable units (ok + 4xx, the quota-counted classes) per UTC day in [from, to], the durable side the month-to-date meter reconciles against.
func (s *Store) BillableByDay(ctx context.Context, subject, from, to string) (map[string]int64, error) {
	const q = `
        SELECT day, SUM(ok_count + client_error_count)::bigint
          FROM usage_daily
         WHERE subject = $1
           AND day >= $2::date
           AND day <= $3::date
         GROUP BY day
    `
	rows, err := s.db.QueryContext(ctx, q, subject, from, to)
	if err != nil {
		return nil, fmt.Errorf("timescale: BillableByDay: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int64)
	for rows.Next() {
		var (
			day time.Time
			n   int64
		)
		if err := rows.Scan(&day, &n); err != nil {
			return nil, fmt.Errorf("timescale: BillableByDay: scan: %w", err)
		}
		out[day.UTC().Format("2006-01-02")] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: BillableByDay: rows: %w", err)
	}
	return out, nil
}

var _ usage.DailyBillableReader = (*Store)(nil)

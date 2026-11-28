package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errLendingRateDays is a day-series read whose range is not whole UTC days opening before it closes.
var errLendingRateDays = errors.New("clickhouse: lending rate days: from and to must be UTC midnights, from before to")

// LendingRateDaysSlack is how long before a day opens a state may have closed and still be that day's base: the base
// slack of a one-day window.
var LendingRateDaysSlack = LendingRateBaseSlack(24 * time.Hour)

// LendingRateDay is the first and last ResData state of a reserve closing in one UTC day. A side that exists but fails
// to decode is nil with its Undecodable flag set.
type LendingRateDay struct {
	Day                               time.Time
	First, Last                       *LendingRateState
	FirstUndecodable, LastUndecodable bool
}

// LendingRateDays is a reserve's ResData day by day over [from, to), read from the base slack before from and below
// the watermark's hour. Days holds only days with a state, ascending, so the day before from appears when a state
// closed in its last slack. Prior is true when a state closed before from less the slack.
type LendingRateDays struct {
	Days  []LendingRateDay
	Prior bool
}

// lendingRateDaysQuery is, per UTC day of close_time, the first and last row of one key after FINAL collapses each hour,
// plus the prior probe of lendingRatePriorSelect's shape (kind 1). Args: contract, key, lo, to, the watermark hour,
// lo, to, then contract, key, lo, the watermark hour.
const lendingRateDaysQuery = `SELECT * FROM (
	SELECT 0 AS kind, toStartOfDay(close_time, 'UTC') AS day,
		argMin(protocol, hour), argMin(ledger_seq, hour), argMin(close_time, hour), argMin(entry_xdr, hour),
		argMax(protocol, hour), argMax(ledger_seq, hour), argMax(close_time, hour), argMax(entry_xdr, hour)
	FROM (
		SELECT protocol, hour, ledger_seq, close_time, entry_xdr
		FROM stellar.lending_state_hourly FINAL
		WHERE contract_id = ? AND key_xdr = ? AND hour >= toStartOfHour(toDateTime(?, 'UTC'))
			AND hour < toDateTime(?, 'UTC') AND hour < toDateTime(?, 'UTC')
	) WHERE close_time >= toDateTime(?, 'UTC') AND close_time < toDateTime(?, 'UTC')
	GROUP BY day
	UNION ALL
	SELECT 1 AS kind, toStartOfDay(close_time, 'UTC') AS day, protocol, ledger_seq, close_time, '', protocol, ledger_seq, close_time, ''
	FROM (
		SELECT protocol, ledger_seq, close_time FROM stellar.lending_state_hourly
		WHERE contract_id = ? AND key_xdr = ? AND hour <= toStartOfHour(toDateTime(?, 'UTC')) AND hour < toDateTime(?, 'UTC')
		ORDER BY hour DESC
		LIMIT 1
	)
	) ORDER BY kind, day
	SETTINGS optimize_move_to_prewhere_if_final = 0`

// LendingRateDays reads one reserve's day series in one query: per day two rows of one primary-key prefix range, so the
// read is bounded by the range's hours, not the reserve's whole history.
func (r *ExplorerReader) LendingRateDays(ctx context.Context, pool, asset string, from, to time.Time, cov LendingHistoryCoverage) (LendingRateDays, error) {
	if !from.Before(to) || !from.Equal(from.UTC().Truncate(24*time.Hour)) || !to.Equal(to.UTC().Truncate(24*time.Hour)) {
		return LendingRateDays{}, errLendingRateDays
	}
	_, keys, err := rateStateKeys(pool, []string{asset})
	if err != nil {
		return LendingRateDays{}, err
	}
	lo, t, before := from.Add(-LendingRateDaysSlack).Unix(), to.Unix(), cov.servedBefore()
	rows, err := r.conn.Query(ctx, lendingRateDaysQuery, pool, keys[0], lo, t, before, lo, t, pool, keys[0], lo, before)
	if err != nil {
		return LendingRateDays{}, fmt.Errorf("clickhouse: lending rate days: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out LendingRateDays
	for rows.Next() {
		var kind uint8
		var day time.Time
		var first, last stateRow
		if err := rows.Scan(&kind, &day, &first.protocol, &first.ledger, &first.close, &first.entry,
			&last.protocol, &last.ledger, &last.close, &last.entry); err != nil {
			return LendingRateDays{}, fmt.Errorf("clickhouse: scan lending rate days: %w", err)
		}
		if kind == 1 {
			out.Prior = first.close.Before(time.Unix(lo, 0))
			continue
		}
		d := LendingRateDay{Day: day.UTC()}
		d.First, d.FirstUndecodable = decodeRateDaySide(first)
		d.Last, d.LastUndecodable = decodeRateDaySide(last)
		out.Days = append(out.Days, d)
	}
	if err := rows.Err(); err != nil {
		return LendingRateDays{}, fmt.Errorf("clickhouse: lending rate days rows: %w", err)
	}
	return out, nil
}

func decodeRateDaySide(row stateRow) (*LendingRateState, bool) {
	st, ok := decodeRateState(row)
	return st, !ok
}

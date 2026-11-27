package timescale

import (
	"context"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TradesInMinutesQuery is the SQL of [Store.TradesInMinutes]; exported for the plan-shape integration test. Each direction is driven
// from the minute list: one trades_pair_ts_idx range probe per minute, its own ORDER BY … LIMIT keeping the planner from flattening
// the lateral into a join over the whole span (each minute's newest $6 hold the overall newest $6). The constant [$4, $5) bounds let
// Timescale exclude chunks. The pair reaches the probe as columns of the MATERIALIZED minute list, not as constants: against a
// constant the planner prices the probe from that asset's own frequency, and for a heavily traded asset (XLM base, USDC quote) a
// time-only index plus a filter costs the same, so it reads every market's rows in the minute.
const TradesInMinutesQuery = `
        WITH m(b, x, y) AS MATERIALIZED (SELECT DISTINCT unnest($3::timestamptz[]), $1::text, $2::text)
        (SELECT t.* FROM m CROSS JOIN LATERAL (
            SELECT source, ledger, tx_hash, op_index, ts,
                   base_asset, quote_asset,
                   base_amount, quote_amount,
                   COALESCE(maker, '')      AS maker,
                   COALESCE(taker, '')      AS taker,
                   COALESCE(routed_via, '') AS routed_via,
                   COALESCE(usd_volume >= $8::numeric AND base_amount > 0 AND quote_amount > 0, false) AS clears_floor
              FROM trades
             WHERE base_asset  = m.x
               AND quote_asset = m.y
               AND ts         >= m.b
               AND ts          < m.b + INTERVAL '1 minute'
               AND ts         >= $4::timestamptz
               AND ts          < $5::timestamptz
               AND NOT (source = ANY(string_to_array($7::text, ',')))
             ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
             LIMIT $6::bigint) AS t
          ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
          LIMIT $6::bigint)
        UNION ALL
        (SELECT t.* FROM m CROSS JOIN LATERAL (
            SELECT source, ledger, tx_hash, op_index, ts,
                   base_asset, quote_asset,
                   base_amount, quote_amount,
                   COALESCE(maker, '')      AS maker,
                   COALESCE(taker, '')      AS taker,
                   COALESCE(routed_via, '') AS routed_via,
                   COALESCE(usd_volume >= $8::numeric AND base_amount > 0 AND quote_amount > 0, false) AS clears_floor
              FROM trades
             WHERE base_asset  = m.y
               AND quote_asset = m.x
               AND ts         >= m.b
               AND ts          < m.b + INTERVAL '1 minute'
               AND ts         >= $4::timestamptz
               AND ts          < $5::timestamptz
               AND NOT (source = ANY(string_to_array($7::text, ',')))
             ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
             LIMIT $6::bigint) AS t
          ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
          LIMIT $6::bigint)
        ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
        LIMIT $6::bigint
    `

// TradesInMinutes is [Store.TradesInRange] over the given whole minutes only, in one query: the newest `limit` rows whose ts falls
// in any of them, ordered ascending.
func (s *Store) TradesInMinutes(ctx context.Context, p canonical.Pair, minutes []time.Time, limit int) ([]canonical.Trade, error) {
	if len(minutes) == 0 {
		return nil, nil
	}
	limit = min(max(limit, 1), MaxTradesInRangeLimit)
	// A repeated minute would join its trades twice.
	seen := make(map[int64]bool, len(minutes))
	ms := make([]time.Time, 0, len(minutes))
	lo, hi := minutes[0].UTC(), minutes[0].UTC()
	for _, m := range minutes {
		if !m.Equal(m.Truncate(time.Minute)) {
			return nil, fmt.Errorf("timescale: TradesInMinutes: %v is not a whole minute", m)
		}
		if seen[m.Unix()] {
			continue
		}
		seen[m.Unix()] = true
		ms = append(ms, m.UTC())
		if m.Before(lo) {
			lo = m.UTC()
		}
		if m.After(hi) {
			hi = m.UTC()
		}
	}
	rows, err := s.db.QueryContext(ctx, TradesInMinutesQuery,
		p.Base.String(), p.Quote.String(), ms, lo, hi.Add(time.Minute), limit, nonVWAPSourcesArg(), bridgeLegMinUSDVolume)
	if err != nil {
		return nil, fmt.Errorf("timescale: TradesInMinutes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []canonical.Trade
	for rows.Next() {
		var clearsFloor bool
		t, err := scanTradeOriented(rows, p, "TradesInMinutes", &clearsFloor)
		if err != nil {
			return nil, err
		}
		t.ClearsNotionalFloor = clearsFloor
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TradesInMinutes rows: %w", err)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

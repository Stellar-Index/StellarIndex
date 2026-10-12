package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Router attribution: joins persisted router invocations (soroswap_router_swaps) to the `trades`
// rows they drove and stamps trades.routed_via with the router's registry name.
//
//   - FIRST-WINS: only rows with routed_via IS NULL are updated, so a tag is never overwritten and
//     re-running a window is idempotent.
//   - SOURCE-SCOPED: a composed tx can carry unrelated trades (Phoenix AND the Soroswap router), so
//     only trades whose `source` matches the router's underlying venue are tagged.
//   - TIME-BOUNDED: both join sides carry a time predicate so TimescaleDB prunes chunks. The trades
//     bound is widened by routedViaTsSlack for second-vs-millisecond skew; correctness comes from
//     the exact (ledger, tx_hash) equality, the ts bound is pruning only.
//   - CALL-PATH ATTRIBUTED (ADR-0052): call_path[1] (Postgres 1-indexed; Go CallPath[0]) is the
//     outermost invoking contract. When it is a registered 'router'-kind entry the trade is tagged
//     with that name so /v1/aggregators can report volume per wrapper; otherwise it falls back to
//     routerName (direct calls, unregistered wrappers, legacy NULL call_path), so the fallback never
//     claims a wrapper it lacks evidence for.

// routedViaTsSlack widens the trades.ts chunk-pruning bound relative
// to the router-swap window. Generous (well beyond any close-time
// precision skew) while still confining the UPDATE to the window's
// chunks.
const routedViaTsSlack = 5 * time.Minute

// TagTradesRoutedVia back-tags trades.routed_via for every trade sharing (ledger, tx_hash) with a soroswap_router_swaps row in [from, to) and returns the count; routerName is the fallback when the swap cannot be attributed more specifically.
func (s *Store) TagTradesRoutedVia(ctx context.Context, routerName, tradeSource string, from, to time.Time) (int64, error) {
	if routerName == "" {
		return 0, errors.New("timescale: TagTradesRoutedVia: routerName is empty")
	}
	if tradeSource == "" {
		return 0, errors.New("timescale: TagTradesRoutedVia: tradeSource is empty")
	}
	if !to.After(from) {
		return 0, fmt.Errorf("timescale: TagTradesRoutedVia: to %v must be after from %v", to, from)
	}
	const q = `
        UPDATE trades t
           SET routed_via = COALESCE(wrapper.name, $1)
          FROM soroswap_router_swaps r
          LEFT JOIN routers wrapper
            ON wrapper.kind        = 'router'
           AND r.call_kind         = 'sub_invocation'
           AND r.call_path         IS NOT NULL
           AND wrapper.contract_id = r.call_path[1]
         WHERE r.ledger_close_time >= $3
           AND r.ledger_close_time <  $4
           AND t.ts >= $5
           AND t.ts <  $6
           AND t.ledger  = r.ledger
           AND t.tx_hash = r.tx_hash
           AND t.source  = $2
           AND t.routed_via IS NULL
    `
	res, err := s.db.ExecContext(ctx, q,
		routerName, tradeSource,
		from.UTC(), to.UTC(),
		from.UTC().Add(-routedViaTsSlack), to.UTC().Add(routedViaTsSlack),
	)
	if err != nil {
		return 0, fmt.Errorf("timescale: TagTradesRoutedVia [%s → %s): %w", from.UTC(), to.UTC(), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("timescale: TagTradesRoutedVia rows-affected: %w", err)
	}
	return n, nil
}

// RouterSwapLedgerBounds returns the (min, max) ledger present in soroswap_router_swaps. ok=false when the table is empty.
func (s *Store) RouterSwapLedgerBounds(ctx context.Context) (minLedger, maxLedger uint32, ok bool, err error) {
	var lo, hi sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT min(ledger), max(ledger) FROM soroswap_router_swaps`,
	).Scan(&lo, &hi); err != nil {
		return 0, 0, false, fmt.Errorf("timescale: RouterSwapLedgerBounds: %w", err)
	}
	if !lo.Valid || !hi.Valid {
		return 0, 0, false, nil
	}
	return uint32(lo.Int64), uint32(hi.Int64), true, nil
}

// RouterSwapTimeBounds returns the (min, max) ledger_close_time of soroswap_router_swaps in the inclusive ledger range, ok=false when empty so the caller skips the window without touching trades.
func (s *Store) RouterSwapTimeBounds(ctx context.Context, fromLedger, toLedger uint32) (minTS, maxTS time.Time, ok bool, err error) {
	var lo, hi sql.NullTime
	if err := s.db.QueryRowContext(ctx,
		`SELECT min(ledger_close_time), max(ledger_close_time)
           FROM soroswap_router_swaps
          WHERE ledger >= $1 AND ledger <= $2`,
		int64(fromLedger), int64(toLedger),
	).Scan(&lo, &hi); err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("timescale: RouterSwapTimeBounds: %w", err)
	}
	if !lo.Valid || !hi.Valid {
		return time.Time{}, time.Time{}, false, nil
	}
	return lo.Time.UTC(), hi.Time.UTC(), true, nil
}

// AggregatorRollupRow is one /v1/aggregators row: a routers-registry entry joined with its routed-trade stats; Volume is a NUMERIC decimal string, nil when no routed trade carried a USD valuation.
type AggregatorRollupRow struct {
	ContractID     string
	Name           string
	Kind           string // 'router' | 'aggregator-vault'
	ProtocolSlug   string
	AutoDiscovered bool

	RoutedTrades int64
	RoutedVolume *string // SUM(usd_volume) over routed trades, decimal string
	LastRoutedAt *time.Time
}

// AggregatorRollup returns every routers-registry entry with its routed-trade rollup for trades with ts >= since; entries with no routed trades still appear (LEFT JOIN) with zero counts.
func (s *Store) AggregatorRollup(ctx context.Context, since time.Time) ([]AggregatorRollupRow, error) {
	const q = `
        SELECT r.contract_id, r.name, r.kind, r.protocol_slug, r.auto_discovered,
               COALESCE(t.routed_trades, 0),
               t.routed_volume_usd::text,
               t.last_routed_at
          FROM routers r
          LEFT JOIN (
                SELECT routed_via,
                       count(*)        AS routed_trades,
                       sum(usd_volume) AS routed_volume_usd,
                       max(ts)         AS last_routed_at
                  FROM trades
                 WHERE ts >= $1
                   AND routed_via IS NOT NULL
                 GROUP BY routed_via
               ) t ON t.routed_via = r.name
         ORDER BY r.kind, r.name
    `
	rows, err := s.db.QueryContext(ctx, q, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("timescale: AggregatorRollup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AggregatorRollupRow
	for rows.Next() {
		var (
			row    AggregatorRollupRow
			vol    sql.NullString
			lastAt sql.NullTime
		)
		if err := rows.Scan(
			&row.ContractID, &row.Name, &row.Kind, &row.ProtocolSlug, &row.AutoDiscovered,
			&row.RoutedTrades, &vol, &lastAt,
		); err != nil {
			return nil, fmt.Errorf("timescale: AggregatorRollup scan: %w", err)
		}
		if vol.Valid {
			v := vol.String
			row.RoutedVolume = &v
		}
		if lastAt.Valid {
			t := lastAt.Time.UTC()
			row.LastRoutedAt = &t
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: AggregatorRollup rows: %w", err)
	}
	return out, nil
}

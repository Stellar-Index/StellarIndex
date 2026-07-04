package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// This file is Phase B of router attribution (BACKLOG #29 /
// migration 0025): joining persisted router invocations
// (soroswap_router_swaps) to the per-pair `trades` rows they drove,
// and stamping trades.routed_via with the router's registry name.
//
// Policy (documented once, here):
//
//   - FIRST-WINS. The UPDATE only touches rows whose routed_via IS
//     NULL — an existing tag (same router or a different one) is
//     never overwritten. Re-running any window is therefore
//     idempotent: already-tagged rows match zero predicates and the
//     statement is a no-op for them.
//   - SOURCE-SCOPED. A tx can carry unrelated trades from other
//     protocols (a composed tx that swaps on Phoenix AND calls the
//     Soroswap router). Only trades whose `source` matches the
//     router's underlying venue are tagged — for soroswap-router
//     that is source='soroswap' (the router exclusively walks
//     Soroswap pair contracts).
//   - TIME-BOUNDED. Both sides of the join carry a time predicate so
//     TimescaleDB prunes chunks on both hypertables. trades.ts and
//     soroswap_router_swaps.ledger_close_time are the same ledger
//     close time, but the trades bound is widened by
//     routedViaTsSlack to survive any second-vs-millisecond
//     precision skew; correctness comes from the exact
//     (ledger, tx_hash) equality, the ts bound is pruning only.

// routedViaTsSlack widens the trades.ts chunk-pruning bound relative
// to the router-swap window. Generous (well beyond any close-time
// precision skew) while still confining the UPDATE to the window's
// chunks.
const routedViaTsSlack = 5 * time.Minute

// TagTradesRoutedVia back-tags trades.routed_via for every trade
// that shares (ledger, tx_hash) with a soroswap_router_swaps row
// whose ledger_close_time is in [from, to). Returns the number of
// trades rows tagged.
//
// routerName is the value written into routed_via (routers.name,
// e.g. soroswap_router.SourceName). tradeSource scopes which trades
// are eligible (see the source-scoped policy above). Both the live
// sweeper (internal/pipeline/routedvia.go) and the historical
// `stellarindex-ops tag-routed-via` pass call through here, so the
// tagging predicate cannot drift between the two.
//
// NOTE for historical passes: an UPDATE into compressed trades
// chunks (older than the 7-day compression horizon) decompresses the
// affected segments — run windowed via tag-routed-via, not as one
// giant statement.
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
           SET routed_via = $1
          FROM soroswap_router_swaps r
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

// RouterSwapLedgerBounds returns the (min, max) ledger present in
// soroswap_router_swaps. ok=false when the table is empty. Drives
// the default window bounds of `stellarindex-ops tag-routed-via`.
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

// RouterSwapTimeBounds returns the (min, max) ledger_close_time of
// soroswap_router_swaps rows in the inclusive ledger range
// [fromLedger, toLedger]. ok=false when the range holds no rows —
// the caller skips the window without touching trades. Uses the
// ledger column (plain integer, cheap btree range) so the historical
// pass can window by ledger while TagTradesRoutedVia stays
// time-bounded for chunk pruning.
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

// AggregatorRollupRow is one /v1/aggregators row: a routers-registry
// entry joined with its routed-trade stats since `since` (the
// handler passes now-24h). Volume is a NUMERIC decimal string
// (ADR-0003); nil when none of the routed trades carried a USD
// valuation (usd_volume is aggregator-backfilled and can lag).
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

// AggregatorRollup returns every routers-registry entry with its
// routed-trade rollup for trades whose ts >= since. Registry rows
// with no routed trades still appear (LEFT JOIN) with zero counts —
// aggregator-vault entries always look like that today because
// per-tx tagging only applies to kind='router'.
//
// The routed-trades scan is cheap: trades_routed_via_idx is a
// partial index over the (rare) non-NULL routed_via rows, and the
// ts bound prunes chunks. `since` is computed by the caller (not
// now() in SQL) so the statement stays parameter-sargable.
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

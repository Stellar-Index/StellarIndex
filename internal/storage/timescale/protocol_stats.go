package timescale

import (
	"context"
	"fmt"
)

// countRecentEventsQuery is the trailing-24h per-protocol event census behind
// GET /v1/protocols' events_24h column: one UNION ALL pass over every served
// protocol table, each leg labelled with the logical source name the API's
// protocol registry uses.
//
// It runs off the request path: the aggregator's protoeventsrollup worker runs
// it via [Store.RefreshProtocolEventCounts] into the protocol_events_24h rollup
// (migration 0086), and the handler reads that table via
// CountRecentEventsBySource.
//
// Several legs sum into one logical source (e.g. 'aquarius', 'blend', 'phoenix',
// 'defindex', 'sorocredit' add their non-trade tables to the trades leg), and
// some tables are their own logical source ('blend_backstop', 'blend_emitter',
// 'upshift', 'spectra', 'soroswap-router'). The trades and oracle_updates legs
// GROUP BY source and also yield CEX/FX/oracle sources; callers look up only the
// names they care about. The legs' own SELECTs name each table and timestamp
// column. Every leg counts a hypertable's most recent 24h of chunks, and the API
// fronts the result with a 60s cache.
const countRecentEventsQuery = `
	SELECT source, count(*) AS n
	  FROM trades
	 WHERE ts >= now() - interval '24 hours'
	 GROUP BY source
	UNION ALL
	SELECT 'blend', count(*) FROM blend_positions
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'blend', count(*) FROM blend_emissions
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'blend', count(*) FROM blend_admin
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'blend', count(*) FROM blend_auctions
	 WHERE ts >= now() - interval '24 hours'
	UNION ALL
	SELECT 'blend_backstop', count(*) FROM blend_backstop_events
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'blend_emitter', count(*) FROM blend_emitter_events
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'phoenix', count(*) FROM phoenix_liquidity
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'phoenix', count(*) FROM phoenix_stake_events
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'comet', count(*) FROM comet_liquidity
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'upshift', count(*) FROM upshift_vault_events
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'spectra', count(*) FROM spectra_events
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'soroswap', count(*) FROM soroswap_skim_events
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'defindex', count(*) FROM defindex_flows
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'defindex', count(*) FROM defindex_fees
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'defindex', count(*) FROM defindex_admin_events
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'sorocredit', count(*) FROM credit_positions
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'sorocredit', count(*) FROM credit_statements
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'sorocredit', count(*) FROM credit_settlements
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'sorocredit', count(*) FROM credit_events
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'aquarius', count(*) FROM aquarius_liquidity
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'aquarius', count(*) FROM aquarius_reserves
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'aquarius', count(*) FROM aquarius_reserves_sync
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'aquarius', count(*) FROM aquarius_rewards_events
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'aquarius', count(*) FROM aquarius_admin
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'aquarius', count(*) FROM aquarius_protocol_fee
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'aquarius', count(*) FROM aquarius_kill_switches
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT 'cctp', count(*) FROM cctp_events
	 WHERE ts >= now() - interval '24 hours'
	UNION ALL
	SELECT 'rozo', count(*) FROM rozo_events
	 WHERE ts >= now() - interval '24 hours'
	UNION ALL
	SELECT 'soroswap-router', count(*) FROM soroswap_router_swaps
	 WHERE ledger_close_time >= now() - interval '24 hours'
	UNION ALL
	SELECT source, count(*) FROM oracle_updates -- totality: includes unmapped
	 WHERE ts >= now() - interval '24 hours'
	 GROUP BY source
`

// readProtocolEventsRollup reads the pre-aggregated per-source
// trailing-24h event counts from the protocol_events_24h rollup
// (migration 0086). Plain PK-keyed table scan — no per-request census.
// The aggregator's protoeventsrollup worker maintains the rows via
// [Store.RefreshProtocolEventCounts].
const readProtocolEventsRollup = `SELECT source, events_24h FROM protocol_events_24h`

// CountRecentEventsBySource returns the trailing-24h decoded-event
// count per logical source name, read from the protocol_events_24h
// rollup (migration 0086). Multi-table sources (blend, phoenix,
// comet, soroswap) are summed across their tables by the worker, so
// the caller gets one number per source. The map also carries trades'
// off-chain sources (binance, kraken, …); protocol-scoped callers
// simply ignore the extra keys.
//
// This is a keyed-on-PK read; `countRecentEventsQuery`, a UNION ALL
// count(*) over ~17 hypertables, runs only in the worker. Empty rollup
// (aggregator worker has not run yet) → empty map → every protocol
// reads 0 (safe degradation, same posture as change_summary_5m).
func (s *Store) CountRecentEventsBySource(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, readProtocolEventsRollup)
	if err != nil {
		return nil, fmt.Errorf("timescale: CountRecentEventsBySource: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int64, 32)
	for rows.Next() {
		var (
			source string
			n      int64
		)
		if err := rows.Scan(&source, &n); err != nil {
			return nil, fmt.Errorf("timescale: CountRecentEventsBySource scan: %w", err)
		}
		out[source] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: CountRecentEventsBySource rows: %w", err)
	}
	return out, nil
}

// refreshProtocolEventsUpsert folds the trailing-24h census
// (countRecentEventsQuery) into one row per source and upserts them
// into protocol_events_24h. The census's multi-leg-per-source shape
// (blend appears 4×, phoenix 3×, …) is collapsed by the outer
// SUM(n) GROUP BY source, matching the reader's historical
// `out[source] += n` fold. computed_at is stamped to the transaction
// timestamp so the sibling prune can drop sources not touched this pass.
const refreshProtocolEventsUpsert = `
INSERT INTO protocol_events_24h AS t (source, events_24h, computed_at)
SELECT source, SUM(n)::bigint AS events_24h, now()
  FROM (` + countRecentEventsQuery + `) g
 GROUP BY source
ON CONFLICT (source) DO UPDATE
   SET events_24h  = EXCLUDED.events_24h,
       computed_at = EXCLUDED.computed_at`

// refreshProtocolEventsPrune deletes sources that fell out of the
// trailing-24h window (no leg counted them this pass, so their
// computed_at stayed at the previous run). Runs in the same
// transaction as the upsert, so now() is the shared transaction
// timestamp: just-upserted rows carry computed_at = now() (not < now())
// and survive; stale rows carry an older timestamp and are dropped.
const refreshProtocolEventsPrune = `DELETE FROM protocol_events_24h WHERE computed_at < now()`

// RefreshProtocolEventCounts recomputes the protocol_events_24h rollup
// from the live 24h census and atomically replaces its contents. Called
// on a slow cadence by the aggregator's protoeventsrollup worker so the
// /v1/protocols read path (CountRecentEventsBySource) never runs the
// multi-table census per request.
//
// Upsert + prune run in one transaction (row-level locks only — no
// ACCESS EXCLUSIVE table lock that would stall concurrent
// CountRecentEventsBySource reads on the customer-facing endpoint).
func (s *Store) RefreshProtocolEventCounts(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: RefreshProtocolEventCounts begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, refreshProtocolEventsUpsert); err != nil {
		return fmt.Errorf("timescale: RefreshProtocolEventCounts upsert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, refreshProtocolEventsPrune); err != nil {
		return fmt.Errorf("timescale: RefreshProtocolEventCounts prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: RefreshProtocolEventCounts commit: %w", err)
	}
	return nil
}

package timescale

import (
	"context"
	"fmt"
)

// countRecentEventsQuery is the trailing-24h per-protocol event census behind /v1/protocols' events_24h: one UNION ALL over every served
// table, each leg labelled with the API registry's source name. It runs only from [Store.RefreshProtocolEventCounts] into
// protocol_events_24h; the handler reads the rollup. Several legs sum into one source; the trades and oracle_updates legs GROUP BY source
// and also yield CEX/FX/oracle sources callers ignore.
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

// readProtocolEventsRollup reads the pre-aggregated per-source 24h counts: a plain scan of a
// small PK-keyed table, so no per-request census.
const readProtocolEventsRollup = `SELECT source, events_24h FROM protocol_events_24h`

// CountRecentEventsBySource returns the trailing-24h decoded-event count per logical source from the protocol_events_24h rollup, with multi-table sources pre-summed and off-chain trade sources as extra keys.
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

// refreshProtocolEventsUpsert folds the census to one row per source (outer SUM(n)); computed_at is the transaction timestamp so the prune
// can drop untouched sources.
const refreshProtocolEventsUpsert = `
INSERT INTO protocol_events_24h AS t (source, events_24h, computed_at)
SELECT source, SUM(n)::bigint AS events_24h, now()
  FROM (` + countRecentEventsQuery + `) g
 GROUP BY source
ON CONFLICT (source) DO UPDATE
   SET events_24h  = EXCLUDED.events_24h,
       computed_at = EXCLUDED.computed_at`

// refreshProtocolEventsPrune deletes sources outside the 24h window; same transaction as the upsert, so just-upserted rows (computed_at =
// now()) survive.
const refreshProtocolEventsPrune = `DELETE FROM protocol_events_24h WHERE computed_at < now()`

// RefreshProtocolEventCounts recomputes the protocol_events_24h rollup from the live 24h census and atomically replaces it, so /v1/protocols never runs the multi-table census per request; upsert and prune take row locks only.
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

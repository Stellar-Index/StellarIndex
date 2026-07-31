package timescale

// Split out of protocol_bespoke.go (2026-07-30) so per-category visual
// suites can be built in parallel without colliding on one file. Shared
// types (BespokeBlock/KPI/Series/Table/Breakdown) + the dispatcher + the
// scan helpers stay in protocol_bespoke.go.

// ─── Yield bespoke analytics (DeFindex) ──────────────────────────────────
//
// Built from defindex_flows (migration 0050), which records BOTH layers of
// every protocol flow — ground-truthed on r1 2026-07-30 over all 160,206
// rows:
//
//   - VAULT layer (who/when): actor is the end-user (G-strkey, occasionally
//     a routing C-strkey), contract_id is the vault. The amount lives in
//     amounts_vec — one entry per asset in the vault's basket. 78,665 of
//     78,671 vault rows carry a single-entry vector (single-asset vaults);
//     6 rows across 2 vaults carry two entries.
//   - STRATEGY layer (how much capital moved): actor is the vault contract,
//     contract_id the strategy, amount a single-asset scalar — the actual
//     capital deployed into/out of Blend.
//
// Amount honesty rules encoded below (the mixed-asset-sum trap):
//
//   1. Per-vault amounts are shown ONLY for vaults whose every window flow
//      carries a single-entry amounts_vec — those sums are the vault's OWN
//      asset in base units. A vault with any multi-asset (or missing)
//      vector shows '—' instead of a fabricated cross-asset sum.
//   2. Amounts are NEVER summed ACROSS vaults or strategies into one
//      number-with-a-unit: different vaults hold different assets. The
//      cross-vault headline numbers are therefore COUNTS; the existing
//      cross-strategy gross-volume KPIs keep their long-standing
//      "summed base units across strategies" caveat in the Notes.
//
// Every windowed query is bounded by ledger_close_time > now() -
// $1::interval; the one all-time read is a count over the small (~160k
// rows) table. Series follow the shared bridgeSeriesGrain rule: hourly
// buckets at the 24h window, daily otherwise, with window-stable names.

import (
	"context"
	"fmt"
)

// Window-stable series names (the grain lives in the point timestamps).
const (
	yieldSeriesNameVaultDeposits  = "Vault deposits"
	yieldSeriesNameVaultWithdraws = "Vault withdrawals"
	yieldSeriesNameStrategyVolume = "Strategy flow volume"
	yieldSeriesNameStrategyEvents = "Strategy events"
)

// yieldBreakdownTopN caps the flows-by-vault donut at the top vaults,
// folding the tail into one "Others" slice (counts only — see
// foldBreakdownRows' amount caveat).
const yieldBreakdownTopN = 10

// defindexWindowKPIQuery returns the windowed headline figures: gross
// strategy-layer deposit/withdraw volume (the long-standing capital KPIs),
// vault-layer deposit/withdraw counts, unique depositors, active vaults,
// and unique actors across both layers. $1 = interval.
func defindexWindowKPIQuery() string {
	return `
		SELECT
		  COALESCE(sum(amount) FILTER (WHERE direction = 'deposit'  AND layer = 'strategy'),0)::text,
		  COALESCE(sum(amount) FILTER (WHERE direction = 'withdraw' AND layer = 'strategy'),0)::text,
		  count(*) FILTER (WHERE layer = 'vault' AND direction = 'deposit')::text,
		  count(*) FILTER (WHERE layer = 'vault' AND direction = 'withdraw')::text,
		  count(DISTINCT actor) FILTER (WHERE layer = 'vault' AND direction = 'deposit')::text,
		  count(DISTINCT contract_id) FILTER (WHERE layer = 'vault')::text,
		  count(DISTINCT actor)::text
		FROM defindex_flows WHERE ledger_close_time > now() - $1::interval`
}

// defindexAllTimeKPIQuery returns the retained-history totals across both
// layers plus the first-observation date. Deliberately NOT window-bounded;
// defindex_flows carries no retention, but history begins at the source's
// first ingested event (2025-05-13 on r1), not vault genesis.
func defindexAllTimeKPIQuery() string {
	return `
		SELECT count(*)::text,
		  count(*) FILTER (WHERE layer = 'vault' AND direction = 'deposit')::text,
		  count(*) FILTER (WHERE layer = 'vault' AND direction = 'withdraw')::text,
		  COALESCE(min(ledger_close_time)::date::text, '—')
		FROM defindex_flows`
}

// defindexVaultCountSeriesQuery builds the (direction, bucket, count)
// vault-layer flow series at the window's grain — folded into the two
// deposit/withdraw lines by collectKeyedSeries. $1 = interval.
func defindexVaultCountSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT direction, to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), count(*)::text
		FROM defindex_flows
		WHERE ledger_close_time > now() - $1::interval AND layer = 'vault'` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		GROUP BY 1, 2 ORDER BY 1, 2 ASC`
}

// defindexStrategyVolumeSeriesQuery builds the gross strategy-layer flow
// volume series (summed scalar amounts, token base units across
// strategies — see the Notes caveat) at the window's grain. $1 = interval.
func defindexStrategyVolumeSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), COALESCE(sum(amount),0)::text
		FROM defindex_flows
		WHERE ledger_close_time > now() - $1::interval AND layer = 'strategy'` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		GROUP BY 1 ORDER BY 1 ASC`
}

// defindexStrategyEventsSeriesQuery builds the strategy-layer event-count
// series at the window's grain. $1 = interval.
func defindexStrategyEventsSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), count(*)::text
		FROM defindex_flows
		WHERE ledger_close_time > now() - $1::interval AND layer = 'strategy'` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		GROUP BY 1 ORDER BY 1 ASC`
}

// defindexVaultBreakdownQuery returns per-vault window flow COUNTS
// (vault layer), count-descending, labelled with the curated router name
// where one is seeded (migration 0033) and the vault contract id
// otherwise. Counts only — amounts across vaults are different assets.
// $1 = interval.
func defindexVaultBreakdownQuery() string {
	return `
		SELECT COALESCE(r.name, f.contract_id), count(*)::text, count(*)
		FROM defindex_flows f LEFT JOIN routers r ON r.contract_id = f.contract_id
		WHERE f.ledger_close_time > now() - $1::interval AND f.layer = 'vault'
		GROUP BY 1 ORDER BY count(*) DESC, 1 ASC`
}

// defindexVaultFlowsTableQuery returns the per-vault window drill-down:
// deposit/withdraw counts always, plus deposited/withdrawn amounts in the
// vault's OWN asset base units — but only when every one of the vault's
// window flows carries a single-entry amounts_vec (single-asset vault
// evidence); anything else shows '—' rather than a cross-asset sum.
// $1 = interval.
func defindexVaultFlowsTableQuery() string {
	return `
		SELECT COALESCE(r.name, f.contract_id),
		  count(*) FILTER (WHERE f.direction = 'deposit')::text,
		  count(*) FILTER (WHERE f.direction = 'withdraw')::text,
		  CASE WHEN max(cardinality(f.amounts_vec)) = 1 AND count(*) = count(f.amounts_vec)
		       THEN COALESCE(sum(f.amounts_vec[1]) FILTER (WHERE f.direction = 'deposit'),0)::text ELSE '—' END,
		  CASE WHEN max(cardinality(f.amounts_vec)) = 1 AND count(*) = count(f.amounts_vec)
		       THEN COALESCE(sum(f.amounts_vec[1]) FILTER (WHERE f.direction = 'withdraw'),0)::text ELSE '—' END
		FROM defindex_flows f LEFT JOIN routers r ON r.contract_id = f.contract_id
		WHERE f.ledger_close_time > now() - $1::interval AND f.layer = 'vault'
		GROUP BY COALESCE(r.name, f.contract_id)
		ORDER BY count(*) DESC LIMIT 25`
}

// defindexStrategyTableQuery returns the long-standing per-strategy net
// flow table (strategy-layer scalar amounts). $1 = interval.
func defindexStrategyTableQuery() string {
	return `SELECT contract_id,
		   COALESCE(sum(amount) FILTER (WHERE direction = 'deposit'  AND layer = 'strategy'),0)::text,
		   COALESCE(sum(amount) FILTER (WHERE direction = 'withdraw' AND layer = 'strategy'),0)::text,
		   COALESCE(sum(CASE WHEN layer = 'strategy' AND direction = 'deposit'  THEN amount
		                     WHEN layer = 'strategy' AND direction = 'withdraw' THEN -amount
		                     ELSE 0 END),0)::text,
		   count(*) FILTER (WHERE layer = 'strategy')::text
		 FROM defindex_flows
		 WHERE ledger_close_time > now() - $1::interval AND layer = 'strategy'
		 GROUP BY contract_id
		 ORDER BY count(*) DESC LIMIT 25`
}

// bespokeYield builds the DeFindex vault bespoke block from defindex_flows:
// window + retained-history KPIs across both layers, deposit-vs-withdraw
// and strategy series, the flows-by-vault breakdown, and the per-vault +
// per-strategy tables. See the file doc for the layer split and the
// amount-honesty rules.
func (s *Store) bespokeYield(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	if source != "defindex" {
		return nil, nil
	}
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "yield",
		Notes: []string{
			"Flow volume is gross summed defindex_flows.amount by direction over the window from the STRATEGY layer — the vault layer records who/when but carries the amount as a per-strategy vector (amounts_vec), so its scalar amount is NULL; the strategy-layer scalars are the actual capital deployed.",
			"Net flow (deposit − withdraw) is a window AUM proxy, not all-time TVL (the served tier is retention-scoped). Contract is a Soroban strategy contract id; amounts are summed token base units (per-asset decimals) across strategies.",
			"Per-vault deposited/withdrawn amounts are the vault's OWN asset in base units (single-entry amounts_vec) and are shown only for vaults whose every window flow is single-asset; multi-asset or vector-less vaults show '—'. Amounts are NEVER summed across vaults — different vaults hold different assets, so cross-vault figures here are counts.",
			"All-time totals cover every retained defindex_flows row (no retention) — history begins at the source's first ingested event, not vault genesis.",
		},
	}
	if err := s.yieldKPIs(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.yieldSeriesBlocks(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.yieldVaultBreakdown(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	return blk, s.yieldTables(ctx, blk, since)
}

// yieldKPIs fills the windowed and retained-history headline cards.
func (s *Store) yieldKPIs(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var depositVol, withdrawVol, vaultDeps, vaultWds, depositors, activeVaults, actors string
	err := s.db.QueryRowContext(ctx, defindexWindowKPIQuery(), since).
		Scan(&depositVol, &withdrawVol, &vaultDeps, &vaultWds, &depositors, &activeVaults, &actors)
	if err != nil {
		return fmt.Errorf("timescale: bespokeYield KPIs: %w", err)
	}
	var allFlows, allVaultDeps, allVaultWds, firstSeen string
	if err := s.db.QueryRowContext(ctx, defindexAllTimeKPIQuery()).
		Scan(&allFlows, &allVaultDeps, &allVaultWds, &firstSeen); err != nil {
		return fmt.Errorf("timescale: bespokeYield all-time KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Deposit volume (%dd)", windowDays), Value: depositVol, Unit: "token-units", Hint: "gross strategy-layer deposit amount (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Withdraw volume (%dd)", windowDays), Value: withdrawVol, Unit: "token-units", Hint: "gross strategy-layer withdraw amount (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Vault deposits / withdrawals (%dd)", windowDays), Value: vaultDeps + " / " + vaultWds, Hint: "vault-layer (end-user) flow event counts in the window"},
		BespokeKPI{Label: fmt.Sprintf("Unique depositors (%dd)", windowDays), Value: depositors, Hint: "distinct vault-layer depositor addresses (end-user G-strkeys, occasionally routing contracts)"},
		BespokeKPI{Label: fmt.Sprintf("Active vaults (%dd)", windowDays), Value: activeVaults, Hint: "distinct vault contracts with at least one end-user flow in the window"},
		BespokeKPI{Label: fmt.Sprintf("Unique actors (%dd)", windowDays), Value: actors},
		BespokeKPI{Label: "All-time flow events", Value: allFlows, Hint: "every retained defindex_flows row across both layers — history begins at the source's first ingested event, not vault genesis"},
		BespokeKPI{Label: "All-time vault deposits / withdrawals", Value: allVaultDeps + " / " + allVaultWds, Hint: "vault-layer (end-user) flow event counts, all retained history"},
		BespokeKPI{Label: "Observed since", Value: firstSeen, Hint: "date of the source's first retained flow event"},
	)
	return nil
}

// yieldSeriesBlocks appends the vault-layer deposit/withdraw count lines
// and the strategy-layer volume + event-count lines, all at the window's
// grain (hourly at 24h, daily otherwise).
func (s *Store) yieldSeriesBlocks(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	directional, err := s.collectKeyedSeries(ctx, defindexVaultCountSeriesQuery(windowDays), "flows",
		func(key string) string {
			if key == "deposit" {
				return yieldSeriesNameVaultDeposits
			}
			return yieldSeriesNameVaultWithdraws
		}, since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeYield vault series: %w", err)
	}
	blk.Series = append(blk.Series, directional...)

	volume, err := s.scanDailySeries(ctx, defindexStrategyVolumeSeriesQuery(windowDays), since)
	if err != nil {
		return err
	}
	if len(volume) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: yieldSeriesNameStrategyVolume, Unit: "token-units", Points: volume})
	}

	events, err := s.scanDailySeries(ctx, defindexStrategyEventsSeriesQuery(windowDays), since)
	if err != nil {
		return err
	}
	if len(events) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: yieldSeriesNameStrategyEvents, Unit: "events", Points: events})
	}
	return nil
}

// yieldVaultBreakdown fills the flows-by-vault composition (top vaults by
// window flow count + an aggregated "Others" slice) behind the donut.
func (s *Store) yieldVaultBreakdown(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	bd := BespokeBreakdown{Title: fmt.Sprintf("Flows by vault (%dd)", windowDays), Unit: "flows"}
	rows, err := s.db.QueryContext(ctx, defindexVaultBreakdownQuery(), since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeYield vault breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row BespokeBreakdownRow
		if err := rows.Scan(&row.Label, &row.Value, &row.Count); err != nil {
			return fmt.Errorf("timescale: bespokeYield vault breakdown scan: %w", err)
		}
		bd.Rows = append(bd.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("timescale: bespokeYield vault breakdown rows: %w", err)
	}
	bd.Rows = foldBreakdownRows(bd.Rows, yieldBreakdownTopN)
	if len(bd.Rows) > 0 {
		blk.Breakdowns = append(blk.Breakdowns, bd)
	}
	return nil
}

// yieldTables fills the per-vault flows table (counts always; single-asset
// amounts where truthful) and the long-standing per-strategy net-flow table.
func (s *Store) yieldTables(ctx context.Context, blk *BespokeBlock, since string) error {
	vaults, err := s.scanTable(ctx,
		BespokeTable{Title: "Flows by vault", Columns: []string{"Vault", "Deposits", "Withdrawals", "Deposited (vault asset, base units)", "Withdrawn (vault asset, base units)"}},
		defindexVaultFlowsTableQuery(), since)
	if err != nil {
		return err
	}
	if len(vaults.Rows) > 0 {
		blk.Tables = append(blk.Tables, vaults)
	}

	strategies, err := s.scanTable(ctx,
		BespokeTable{Title: "Net flow by strategy", Columns: []string{"Strategy", "Deposits", "Withdrawals", "Net flow", "Flows"}},
		defindexStrategyTableQuery(), since)
	if err != nil {
		return err
	}
	if len(strategies.Rows) > 0 {
		blk.Tables = append(blk.Tables, strategies)
	}
	return nil
}

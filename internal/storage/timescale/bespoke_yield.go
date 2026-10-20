package timescale

// Each per-category visual suite has its own file so suites can be built in parallel without colliding. Shared types
// (BespokeBlock/KPI/Series/Table/Breakdown), the dispatcher and the scan helpers stay in protocol_bespoke.go.

// Yield bespoke analytics (DeFindex), built from defindex_flows, which records both layers of a flow. Vault layer (who/when): actor is
// the end-user, contract_id the vault, amounts in amounts_vec (one entry per basket asset). Strategy layer (how much capital moved):
// actor is the vault contract, contract_id the strategy, amount a single-asset scalar.
//
// Amount honesty (the mixed-asset-sum trap): per-vault amounts show only for vaults whose every window flow has a single-entry
// amounts_vec; any multi-asset or missing vector shows '—' instead of a fabricated sum. Never sum amounts across vaults or strategies
// into one number-with-a-unit (different assets): cross-vault headlines are counts, and cross-strategy gross-volume KPIs carry a
// "summed base units across strategies" caveat in the Notes.

import (
	"context"
	"fmt"
)

// Window-stable series names (the grain lives in the point timestamps).
const (
	yieldSeriesNameVaultDeposits       = "Vault deposits"
	yieldSeriesNameVaultWithdraws      = "Vault withdrawals"
	yieldSeriesNameStrategyDepositVol  = "Strategy deposit volume"
	yieldSeriesNameStrategyWithdrawVol = "Strategy withdraw volume"
	yieldSeriesNameStrategyEvents      = "Strategy events"
)

// yieldBreakdownTopN caps the flows-by-vault donut, folding the tail into "Others" (counts only, see foldBreakdownRows).
const yieldBreakdownTopN = 10

// defindexWindowKPIQuery returns windowed headline figures: strategy-layer volume, vault-layer counts, depositors, vaults, actors. $1 =
// interval.
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

// defindexAllTimeKPIQuery returns retained-history totals across both layers; deliberately not window-bounded, history starts at first
// ingested event.
func defindexAllTimeKPIQuery() string {
	return `
		SELECT count(*)::text,
		  count(*) FILTER (WHERE layer = 'vault' AND direction = 'deposit')::text,
		  count(*) FILTER (WHERE layer = 'vault' AND direction = 'withdraw')::text,
		  COALESCE(min(ledger_close_time)::date::text, '—')
		FROM defindex_flows`
}

// defindexVaultCountSeriesQuery builds the (direction, bucket, count) vault-layer series at the window's grain. $1 = interval.
func defindexVaultCountSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT direction, to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), count(*)::text
		FROM defindex_flows
		WHERE ledger_close_time > now() - $1::interval AND layer = 'vault'` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		GROUP BY 1, 2 ORDER BY 1, 2 ASC`
}

// defindexStrategyVolumeSeriesQuery builds strategy-layer volume by direction (base units summed across strategies). 'harvest' is excluded:
// it is yield, not capital moved. $1 = interval.
func defindexStrategyVolumeSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT direction, to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), COALESCE(sum(amount),0)::text
		FROM defindex_flows
		WHERE ledger_close_time > now() - $1::interval AND layer = 'strategy' AND direction IN ('deposit', 'withdraw')` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		GROUP BY 1, 2 ORDER BY 1, 2 ASC`
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

// defindexVaultBreakdownQuery returns per-vault window flow counts, descending, labelled by curated router name else contract id.
// Counts only, since vaults hold different assets. $1 = interval.
func defindexVaultBreakdownQuery() string {
	return `
		SELECT COALESCE(r.name, f.contract_id), count(*)::text, count(*)
		FROM defindex_flows f LEFT JOIN routers r ON r.contract_id = f.contract_id
		WHERE f.ledger_close_time > now() - $1::interval AND f.layer = 'vault'
		GROUP BY 1 ORDER BY count(*) DESC, 1 ASC`
}

// defindexVaultFlowsTableQuery returns per-vault deposit/withdraw counts, plus amounts only when every flow carries a single-entry
// amounts_vec; otherwise '—' rather than a cross-asset sum. $1 = interval.
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

// bespokeYield builds the DeFindex vault bespoke block from defindex_flows; see the file doc for the amount-honesty rules.
func (s *Store) bespokeYield(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	if source != "defindex" {
		return nil, nil
	}
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "yield",
		Notes: []string{
			"Flow volume is deposit/withdraw defindex_flows.amount, summed by direction over the window from the STRATEGY layer — the vault layer records who/when but carries the amount as a per-strategy vector (amounts_vec), so its scalar amount is NULL; the strategy-layer scalars are the actual capital deployed. 'harvest' rows (strategy yield, not capital flow) are excluded from this series.",
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

// yieldSeriesBlocks appends the vault count lines and strategy volume/event lines at the window's grain (hourly at 24h, else daily).
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

	volume, err := s.collectKeyedSeries(ctx, defindexStrategyVolumeSeriesQuery(windowDays), "token-units",
		func(key string) string {
			if key == "deposit" {
				return yieldSeriesNameStrategyDepositVol
			}
			return yieldSeriesNameStrategyWithdrawVol
		}, since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeYield strategy volume series: %w", err)
	}
	blk.Series = append(blk.Series, volume...)

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

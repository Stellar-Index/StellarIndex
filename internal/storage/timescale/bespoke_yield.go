package timescale

// Split out of protocol_bespoke.go (2026-07-30) so per-category visual
// suites can be built in parallel without colliding on one file. Shared
// types (BespokeBlock/KPI/Series/Table/Breakdown) + the dispatcher + the
// scan helpers stay in protocol_bespoke.go.

// bespokeYield builds the DeFindex vault bespoke block from defindex_flows:
// windowed gross deposit / withdraw flow volume (by direction), per-vault net
// flow (deposit − withdraw, an AUM proxy), and unique actors. Confirmed on r1:
// direction ∈ {deposit, withdraw}; layer ∈ {strategy, vault}. The series and
// per-vault net scope to the vault layer to avoid double-counting a deposit
// that fans out into strategies.
import (
	"context"
	"fmt"
)

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
		},
	}

	var depositVol, withdrawVol, actors string
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(sum(amount) FILTER (WHERE direction = 'deposit'  AND layer = 'strategy'),0)::text,
		  COALESCE(sum(amount) FILTER (WHERE direction = 'withdraw' AND layer = 'strategy'),0)::text,
		  count(DISTINCT actor)::text
		FROM defindex_flows WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&depositVol, &withdrawVol, &actors)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeYield KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Deposit volume (%dd)", windowDays), Value: depositVol, Unit: "token-units", Hint: "gross strategy-layer deposit amount (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Withdraw volume (%dd)", windowDays), Value: withdrawVol, Unit: "token-units", Hint: "gross strategy-layer withdraw amount (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Unique actors (%dd)", windowDays), Value: actors},
	)

	series, err := s.scanDailySeries(ctx, `
		SELECT to_char(date_trunc('day', ledger_close_time), 'YYYY-MM-DD'), COALESCE(sum(amount),0)::text
		FROM defindex_flows
		WHERE ledger_close_time > now() - $1::interval AND layer = 'strategy'
		GROUP BY 1 ORDER BY 1 ASC`, since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily strategy flow volume", Unit: "token-units", Points: series})
	}

	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Net flow by strategy", Columns: []string{"Strategy", "Deposits", "Withdrawals", "Net flow", "Flows"}},
		`SELECT contract_id,
		   COALESCE(sum(amount) FILTER (WHERE direction = 'deposit'  AND layer = 'strategy'),0)::text,
		   COALESCE(sum(amount) FILTER (WHERE direction = 'withdraw' AND layer = 'strategy'),0)::text,
		   COALESCE(sum(CASE WHEN layer = 'strategy' AND direction = 'deposit'  THEN amount
		                     WHEN layer = 'strategy' AND direction = 'withdraw' THEN -amount
		                     ELSE 0 END),0)::text,
		   count(*) FILTER (WHERE layer = 'strategy')::text
		 FROM defindex_flows
		 WHERE ledger_close_time > now() - $1::interval AND layer = 'strategy'
		 GROUP BY contract_id
		 ORDER BY count(*) DESC LIMIT 25`, since)
	if err != nil {
		return nil, err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return blk, nil
}

// bespokeOracle builds the oracle bespoke block (reflector-dex/cex/fx, band,
// redstone) from oracle_updates scoped by source: feed count (distinct
// asset/quote), update cadence (updates/day), and a latest-prices table.
// price is a NUMERIC at the row's `decimals` scale — shown raw with the scale
// column, not rescaled (decimals can vary per feed).

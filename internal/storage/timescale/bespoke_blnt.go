package timescale

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// bespokeBLNT builds the BLNT V3 lending block from blnt_events with BLNT's own kinds and columns: pool-role position, auction and
// emission events, and backstop-role tiered flows. Amounts stay per asset (or per pool and tier); nothing is summed across tokens.
func (s *Store) bespokeBLNT(ctx context.Context, windowDays int) (*BespokeBlock, error) {
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "lending",
		Notes: []string{
			"Per-asset net supplied / net borrowed are signed sums of blnt_events.amount (underlying tokens) over the window: supply/supply_collateral add, withdraw/withdraw_collateral/clawback subtract for supplied; borrow and flash_loan add, repay subtracts for borrowed. They are WINDOW deltas scoped to ONE asset each, never summed across assets.",
			"Backstop flows are per (pool, tier): each BLNT tier holds its own backstop asset, so tiers are never summed into one volume figure. Amounts are the tier asset's base units.",
		},
	}
	for _, step := range []func(context.Context, *BespokeBlock, string, int) error{
		s.blntPositionBlocks, s.blntAuctionBlocks, s.blntBackstopBlocks, s.blntEmissionKPIs,
	} {
		if err := step(ctx, blk, since, windowDays); err != nil {
			return nil, err
		}
	}
	return blk, nil
}

// blntPoolEventsWhere scopes a read to pool-role position events in the window; contract_id is the pool for role pool.
const blntPoolEventsWhere = `
		  FROM blnt_events
		 WHERE contract_role = 'pool' AND event_kind IN (` + blntPositionKinds + `)
		   AND ledger_close_time > now() - $1::interval`

const blntPositionKPISQL = `
		SELECT count(DISTINCT user_address)::text,
		       count(*) FILTER (WHERE event_kind = 'flash_loan')::text,
		       count(*) FILTER (WHERE event_kind IN (` + blntSupplySideKinds + `))::text,
		       count(*) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `))::text,
		       count(DISTINCT contract_id)::text` + blntPoolEventsWhere

const blntAllTimeKPISQL = `
		SELECT count(*)::text,
		       count(*) FILTER (WHERE event_kind IN (` + blntSupplySideKinds + `))::text,
		       count(*) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `))::text,
		       count(DISTINCT user_address)::text,
		       count(DISTINCT contract_id)::text
		  FROM blnt_events
		 WHERE contract_role = 'pool' AND event_kind IN (` + blntPositionKinds + `)`

// blntNetFlowSQL leaves out an asset with an undecoded amount rather than serve a sum that silently skipped a row.
const blntNetFlowSQL = `
		SELECT asset,
		       COALESCE(sum(` + blntSupplyNetExpr + `),0)::text,
		       COALESCE(sum(` + blntBorrowNetExpr + `),0)::text,
		       count(*)::text` + blntPoolEventsWhere + `
		   AND asset IS NOT NULL
		 GROUP BY asset
		HAVING NOT bool_or(amount IS NULL)
		 ORDER BY count(*) DESC, asset LIMIT 25`

const blntPoolActivitySQL = `
		SELECT contract_id,
		       count(*) FILTER (WHERE event_kind IN (` + blntSupplySideKinds + `))::text,
		       count(*) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `))::text,
		       count(DISTINCT user_address)::text,
		       count(*)::text` + blntPoolEventsWhere + `
		 GROUP BY contract_id
		 ORDER BY count(*) DESC, contract_id LIMIT 25`

func blntDailyPositionSeriesSQL(windowDays int) string {
	return `
		SELECT to_char(date_trunc('day', ledger_close_time), 'YYYY-MM-DD'), count(*)::text` + blntPoolEventsWhere +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		 GROUP BY 1 ORDER BY 1 ASC`
}

// blntPositionBlocks fills the position and scale KPIs, the per-asset net-flow and per-pool tables, and the daily series.
func (s *Store) blntPositionBlocks(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var users, flashLoans, wSupply, wBorrow, wPools string
	if err := s.db.QueryRowContext(ctx, blntPositionKPISQL, since).Scan(&users, &flashLoans, &wSupply, &wBorrow, &wPools); err != nil {
		return fmt.Errorf("timescale: bespokeBLNT position KPIs: %w", err)
	}
	var atEvents, atSupply, atBorrow, atUsers, atPools string
	if err := s.db.QueryRowContext(ctx, blntAllTimeKPISQL).Scan(&atEvents, &atSupply, &atBorrow, &atUsers, &atPools); err != nil {
		return fmt.Errorf("timescale: bespokeBLNT scale KPIs (all-time): %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Active users (%dd)", windowDays), Value: users, Hint: "distinct user addresses with at least one position event in the window"},
		BespokeKPI{Label: fmt.Sprintf("Flash loans (%dd)", windowDays), Value: flashLoans},
		BespokeKPI{Label: fmt.Sprintf("Supply-side events (%dd)", windowDays), Value: wSupply, Unit: "events", Hint: "supply + supply_collateral + withdraw + withdraw_collateral + clawback events in the window"},
		BespokeKPI{Label: fmt.Sprintf("Borrow-side events (%dd)", windowDays), Value: wBorrow, Unit: "events", Hint: "borrow + repay + flash_loan events in the window"},
		BespokeKPI{Label: fmt.Sprintf("Active pools (%dd)", windowDays), Value: wPools, Unit: "pools", Hint: "distinct pools with at least one position event in the window"},
		BespokeKPI{Label: "All-time position events", Value: atEvents, Unit: "events", Hint: "every captured BLNT pool position event across all pools"},
		BespokeKPI{Label: "All-time supply-side events", Value: atSupply, Unit: "events"},
		BespokeKPI{Label: "All-time borrow-side events", Value: atBorrow, Unit: "events"},
		BespokeKPI{Label: "All-time unique users", Value: atUsers, Hint: "distinct user addresses across all captured position events"},
		BespokeKPI{Label: "All-time pools", Value: atPools, Unit: "pools", Hint: "distinct pool contracts ever seen in position events"},
	)
	netFlow := lendingNetFlowHeader
	netFlow.Title = fmt.Sprintf("Net flow by asset (%dd)", windowDays)
	if err := s.appendTable(ctx, blk, netFlow, blntNetFlowSQL, since); err != nil {
		return err
	}
	if err := s.appendTable(ctx, blk, lendingPoolActivityHeader, blntPoolActivitySQL, since); err != nil {
		return err
	}
	series, err := s.scanDailySeries(ctx, blntDailyPositionSeriesSQL(windowDays), since)
	if err != nil {
		return err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily position events", Unit: "events", Points: series})
	}
	return nil
}

const blntAuctionKPISQL = `
		SELECT count(*) FILTER (WHERE event_kind = 'new_auction')::text,
		       count(*) FILTER (WHERE event_kind = 'fill_auction')::text
		  FROM blnt_events
		 WHERE contract_role = 'pool' AND event_kind IN (` + blntAuctionKinds + `)
		   AND ledger_close_time > now() - $1::interval`

const blntRecentAuctionsSQL = `
		SELECT to_char(ledger_close_time, 'YYYY-MM-DD HH24:MI'),
		       CASE attributes->>'auction_type' WHEN '0' THEN 'user-liquidation' WHEN '1' THEN 'bad-debt' WHEN '2' THEN 'interest'
		            ELSE COALESCE(attributes->>'auction_type', '—') END,
		       CASE event_kind WHEN 'new_auction' THEN 'new' WHEN 'fill_auction' THEN 'fill' ELSE 'delete' END,
		       COALESCE(user_address, ''), COALESCE(attributes->>'fill_percent', '—')
		  FROM blnt_events
		 WHERE contract_role = 'pool' AND event_kind IN (` + blntAuctionKinds + `)
		   AND ledger_close_time > now() - $1::interval
		 ORDER BY ledger_close_time DESC, ledger DESC, tx_hash DESC, op_index DESC, event_index DESC LIMIT 25`

func (s *Store) blntAuctionBlocks(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var started, filled string
	if err := s.db.QueryRowContext(ctx, blntAuctionKPISQL, since).Scan(&started, &filled); err != nil {
		return fmt.Errorf("timescale: bespokeBLNT auction KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Auctions started (%dd)", windowDays), Value: started, Hint: "new_auction events: type 0 user-liquidation, 1 bad-debt, 2 interest"},
		BespokeKPI{Label: fmt.Sprintf("Auctions filled (%dd)", windowDays), Value: filled},
	)
	return s.appendTable(ctx, blk, lendingAuctionsHeader, blntRecentAuctionsSQL, since)
}

const blntBackstopCountSQL = `
		SELECT count(*)::text FROM blnt_events
		 WHERE contract_role = 'backstop' AND ledger_close_time > now() - $1::interval`

// blntBackstopFlowSQL sums tier-asset tokens (amount) of deposit and withdraw per (pool, tier); the share-only kinds carry shares in
// amount2 and are not summed. A group with an undecoded amount is left out rather than understated.
const blntBackstopFlowSQL = `
		SELECT pool, tier,
		       COALESCE(sum(amount) FILTER (WHERE event_kind = 'deposit'),0)::text,
		       COALESCE(sum(amount) FILTER (WHERE event_kind = 'withdraw'),0)::text,
		       count(*) FILTER (WHERE event_kind = 'deposit')::text,
		       count(*) FILTER (WHERE event_kind = 'withdraw')::text
		  FROM blnt_events
		 WHERE contract_role = 'backstop' AND event_kind IN ('deposit','withdraw')
		   AND pool IS NOT NULL AND tier IS NOT NULL
		   AND ledger_close_time > now() - $1::interval
		 GROUP BY pool, tier
		HAVING NOT bool_or(amount IS NULL)
		 ORDER BY pool, tier`

var blntBackstopFlowHeader = BespokeTable{
	Title:   "Backstop flow by pool and tier (window)",
	Columns: []string{"Pool", "Tier", "Deposited (tier asset)", "Withdrawn (tier asset)", "Deposits", "Withdrawals"},
	// The tier's backstop asset lives in the backstop's contract storage, not in blnt_events, so amounts stay base units.
	ColumnKinds: []BespokeColumnKind{BespokeCol(BespokeKindPool), BespokeCol(BespokeKindText), BespokeCol(BespokeKindAmount), BespokeCol(BespokeKindAmount), BespokeCol(BespokeKindCount), BespokeCol(BespokeKindCount)},
}

func (s *Store) blntBackstopBlocks(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var events string
	if err := s.db.QueryRowContext(ctx, blntBackstopCountSQL, since).Scan(&events); err != nil {
		return fmt.Errorf("timescale: bespokeBLNT backstop KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs, BespokeKPI{Label: fmt.Sprintf("Backstop events (%dd)", windowDays), Value: events})
	return s.appendTable(ctx, blk, blntBackstopFlowHeader, blntBackstopFlowSQL, since)
}

// blntEmissionSQL: a pool claim's amount is amount_claimed and a backstop claim's is blnt_amount, both BLNT base units.
const blntEmissionSQL = `
		SELECT COALESCE(sum(amount) FILTER (WHERE event_kind = 'claim'),0),
		       bool_or(event_kind = 'claim' AND amount IS NULL),
		       count(*) FILTER (WHERE event_kind = 'claim')::text,
		       count(*) FILTER (WHERE event_kind = 'gulp_emissions')::text,
		       count(*) FILTER (WHERE contract_role = 'pool' AND event_kind IN ('bad_debt','defaulted_debt'))::text,
		       count(*)
		  FROM blnt_events
		 WHERE contract_role IN ('pool','backstop') AND event_kind IN ('claim','gulp_emissions','bad_debt','defaulted_debt')
		   AND ledger_close_time > now() - $1::interval`

func (s *Store) blntEmissionKPIs(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var claimed canonical.Amount
	var nullClaim *bool
	var claims, gulps, creditRisk string
	var rows int64
	if err := s.db.QueryRowContext(ctx, blntEmissionSQL, since).Scan(&claimed, &nullClaim, &claims, &gulps, &creditRisk, &rows); err != nil {
		return fmt.Errorf("timescale: bespokeBLNT emissions: %w", err)
	}
	if rows == 0 {
		return nil
	}
	if nullClaim == nil || !*nullClaim {
		blk.KPIs = append(blk.KPIs, BespokeKPI{
			Label: fmt.Sprintf("Emissions claimed (%dd)", windowDays), Value: claimed.String(), Unit: "BLNT base units",
			Hint:   "summed pool claim (amount_claimed) + backstop claim (blnt_amount) in BLNT base units; a WINDOW sum, not USD",
			Amount: claimed.BigInt(), Token: "BLNT",
		})
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Claim events (%dd)", windowDays), Value: claims},
		BespokeKPI{Label: fmt.Sprintf("Emission gulps (%dd)", windowDays), Value: gulps, Hint: "pool and backstop gulp_emissions events in the window"},
		BespokeKPI{Label: fmt.Sprintf("Credit-risk events (%dd)", windowDays), Value: creditRisk, Hint: "bad_debt + defaulted_debt pool events"},
	)
	return nil
}

// appendTable scans one top-N table and appends it when it has rows.
func (s *Store) appendTable(ctx context.Context, blk *BespokeBlock, header BespokeTable, query string, args ...any) error {
	tbl, err := s.scanTable(ctx, header, query, args...)
	if err != nil {
		return err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return nil
}

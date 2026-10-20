package timescale

// Each per-category visual suite has its own file so suites can be built
// in parallel without colliding on one file. Shared types
// (BespokeBlock/KPI/Series/Table/Breakdown) + the dispatcher + the scan
// helpers stay in protocol_bespoke.go.

// bespokeLending builds the Blend lending bespoke block: PER-ASSET net supplied (supply +
// supply_collateral - withdraw - withdraw_collateral) and net borrowed (borrow - repay) from
// blend_positions, plus auction and backstop activity. Amounts are unsigned magnitudes (the sign
// is the event_kind), and are only ever summed WITHIN one asset because cross-asset sums mix
// per-token decimals; hence no cross-asset headline KPI and no per-pool Util%.
import (
	"context"
	"fmt"
	"strconv"
)

func (s *Store) bespokeLending(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	// sorocredit is a lending-category source with its own event surface
	// (credit_positions / credit_statements / credit_settlements /
	// credit_events, migration 0090) — not the Blend money-market tables —
	// so it gets a dedicated builder rather than the blend_* path below.
	if source == "sorocredit" {
		return s.bespokeCredit(ctx, windowDays)
	}
	if source != "blend" {
		return nil, nil
	}
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "lending",
		Notes: []string{
			"Per-asset net supplied / net borrowed are signed running sums of unsigned blend_positions.token_amount over the window — supply/supply_collateral add, withdraw/withdraw_collateral subtract for supplied; borrow and flash_loan add, repay subtracts for borrowed (a flash loan mints debt that stays open until a repay settles it). They are WINDOW deltas scoped to ONE asset each, not all-time TVL (the served tier is retention-scoped). Amounts are never summed across assets: tokens carry different decimals, so a cross-asset sum would be a meaningless number.",
			"Asset is a Soroban token contract id, shown shortened; amounts are in the token's base units (per-asset decimals).",
			"Pool-level figures are event/user COUNTS only. Real current-state TVL, utilisation and supply/borrow APYs need the Soroban pool-storage reader (reserve b_rate/d_rate + totals from contract storage); this block is event-derived and window-scoped until that ships.",
		},
	}

	if err := s.lendingPositionBlocks(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingAuctionBlocks(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingBackstopKPIs(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingEmissionKPIs(ctx, blk, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingScaleKPIs(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingActivitySeries(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingActivityBreakdowns(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingFlashLoanTable(ctx, blk, since); err != nil {
		return nil, err
	}
	return blk, nil
}

// Everything below is COUNT-first on purpose: blend_positions / blend_backstop_events rows mix
// many tokens at per-asset decimals with no USD valuation at this layer, so summing raw amounts
// across rows would be a meaningless number. Amounts appear only where one row's single asset is
// shown (the flash-loan table) or a sum is already scoped per-asset.
//
// Series names are window-stable (no "(Nd)" suffix, no "Daily" prefix): the grain (hourly at 24h,
// daily otherwise, via bridgeSeriesGrain) lives in the point timestamps, as in the CCTP series.

// lendingSupplySideKinds / lendingBorrowSideKinds partition all seven blend_positions event kinds
// into lender-leg movements (deposits AND withdrawals) and debt-leg movements (borrow, repay,
// flash_loan, a same-tx borrow). Together they cover every kind, so the two lines sum to total
// position events.
const (
	lendingSupplySideKinds = `'supply','supply_collateral','withdraw','withdraw_collateral'`
	lendingBorrowSideKinds = `'borrow','repay','flash_loan'`
)

// blendSupplyNetExpr / blendBorrowNetExpr sign blend_positions.token_amount
// into each leg's running net; every blend fold sums through these. A
// flash_loan mints d-tokens to its user (0045 body: tokens_out,
// d_tokens_minted) that only a later repay burns, so it opens debt as borrow does.
const (
	blendSupplyNetExpr = `CASE WHEN event_kind IN ('supply','supply_collateral') THEN token_amount
	                          WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -token_amount END`
	blendBorrowNetExpr = `CASE WHEN event_kind IN ('borrow','flash_loan') THEN token_amount
	                          WHEN event_kind = 'repay' THEN -token_amount END`
)

// lendingBackstopInflowKinds / lendingBackstopOutflowKinds are the backstop event kinds that move
// funds INTO vs OUT OF a backstop (deposit/donate vs withdraw/draw). The remaining kinds (claim,
// queue/dequeue_withdrawal, distribute, gulp_emissions, rw_zone*) are accounting or queueing
// events, excluded from the flow series but kept in the backstop-events KPI count.
const (
	lendingBackstopInflowKinds  = `'deposit','donate'`
	lendingBackstopOutflowKinds = `'withdraw','draw'`
)

// lendingSideSeriesQuery builds the per-bucket position-event counts for one side (bridgeSeriesGrain); kinds MUST be a lending*SideKinds
// constant.
func lendingSideSeriesQuery(windowDays int, kinds string) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), count(*)::text
		  FROM blend_positions
		 WHERE ledger_close_time > now() - $1::interval
		   AND event_kind IN (` + kinds + `)` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		 GROUP BY 1 ORDER BY 1 ASC`
}

// lendingBackstopFlowSeriesQuery builds per-bucket backstop flow counts for one direction; kinds MUST be a lendingBackstop*Kinds constant.
func lendingBackstopFlowSeriesQuery(windowDays int, kinds string) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), count(*)::text
		  FROM blend_backstop_events
		 WHERE ledger_close_time > now() - $1::interval
		   AND event_kind IN (` + kinds + `)` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		 GROUP BY 1 ORDER BY 1 ASC`
}

// lendingAuctionSeriesQuery builds per-bucket auction-event counts across all three kinds; blend_auctions' time column is ts.
func lendingAuctionSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ts), '` + format + `'), count(*)::text
		  FROM blend_auctions
		 WHERE ts > now() - $1::interval` +
		completeDaysOnly(windowDays, "ts") + `
		 GROUP BY 1 ORDER BY 1 ASC`
}

// lendingPerPoolSeriesQuery builds the top-5-pools series ordered by window events desc, pool, bucket. The pool tiebreak is load-bearing:
// tied pools would interleave by bucket and collectLendingPoolSeries's consecutive-key fold would fragment a line.
func lendingPerPoolSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		WITH top AS (
		  SELECT pool, count(*) AS events
		    FROM blend_positions
		   WHERE ledger_close_time > now() - $1::interval
		   GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 5)
		SELECT p.pool,
		       to_char(date_trunc('` + trunc + `', p.ledger_close_time), '` + format + `'),
		       count(*)::text
		  FROM blend_positions p JOIN top USING (pool)
		 WHERE p.ledger_close_time > now() - $1::interval` +
		completeDaysOnly(windowDays, "p.ledger_close_time") + `
		 GROUP BY p.pool, 2, top.events
		 ORDER BY top.events DESC, p.pool, 2 ASC`
}

// lendingAllTimeKPIQuery is the unwindowed lifetime scale of the Blend money market, deliberately not window-bounded (window-keyed cache).
func lendingAllTimeKPIQuery() string {
	return `
		SELECT count(*)::text,
		       count(*) FILTER (WHERE event_kind IN (` + lendingSupplySideKinds + `))::text,
		       count(*) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `))::text,
		       count(DISTINCT user_address)::text,
		       count(DISTINCT pool)::text
		  FROM blend_positions`
}

// lendingScaleKPIs fills window and all-time supply/borrow event counts and active pools; counts, not amounts.
func (s *Store) lendingScaleKPIs(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var wSupply, wBorrow, wPools string
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE event_kind IN (`+lendingSupplySideKinds+`))::text,
		       count(*) FILTER (WHERE event_kind IN (`+lendingBorrowSideKinds+`))::text,
		       count(DISTINCT pool)::text
		  FROM blend_positions
		 WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&wSupply, &wBorrow, &wPools)
	if err != nil {
		return fmt.Errorf("timescale: bespokeLending scale KPIs (window): %w", err)
	}
	var atEvents, atSupply, atBorrow, atUsers, atPools string
	if err := s.db.QueryRowContext(ctx, lendingAllTimeKPIQuery()).
		Scan(&atEvents, &atSupply, &atBorrow, &atUsers, &atPools); err != nil {
		return fmt.Errorf("timescale: bespokeLending scale KPIs (all-time): %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Supply-side events (%dd)", windowDays), Value: wSupply, Unit: "events", Hint: "supply + supply_collateral + withdraw + withdraw_collateral events in the window"},
		BespokeKPI{Label: fmt.Sprintf("Borrow-side events (%dd)", windowDays), Value: wBorrow, Unit: "events", Hint: "borrow + repay + flash_loan events in the window"},
		BespokeKPI{Label: fmt.Sprintf("Active pools (%dd)", windowDays), Value: wPools, Unit: "pools", Hint: "distinct pools with at least one position event in the window"},
		BespokeKPI{Label: "All-time position events", Value: atEvents, Unit: "events", Hint: "every captured blend_positions event across all pools (r1 history starts 2024-05)"},
		BespokeKPI{Label: "All-time supply-side events", Value: atSupply, Unit: "events"},
		BespokeKPI{Label: "All-time borrow-side events", Value: atBorrow, Unit: "events"},
		BespokeKPI{Label: "All-time unique users", Value: atUsers, Hint: "distinct user addresses across all captured position events"},
		BespokeKPI{Label: "All-time pools", Value: atPools, Unit: "pools", Hint: "distinct pool contracts ever seen in position events"},
	)
	return nil
}

// lendingActivitySeries appends supply vs borrow events, backstop inflow vs outflow, auction events and one line per top-5 pool; all
// counts.
func (s *Store) lendingActivitySeries(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	for _, sp := range []struct {
		name  string
		query string
	}{
		{"Supply-side events", lendingSideSeriesQuery(windowDays, lendingSupplySideKinds)},
		{"Borrow-side events", lendingSideSeriesQuery(windowDays, lendingBorrowSideKinds)},
		{"Backstop inflow events", lendingBackstopFlowSeriesQuery(windowDays, lendingBackstopInflowKinds)},
		{"Backstop outflow events", lendingBackstopFlowSeriesQuery(windowDays, lendingBackstopOutflowKinds)},
		{"Auction events", lendingAuctionSeriesQuery(windowDays)},
	} {
		series, err := s.scanDailySeries(ctx, sp.query, since)
		if err != nil {
			return err
		}
		if len(series) > 0 {
			blk.Series = append(blk.Series, BespokeSeries{Name: sp.name, Unit: "events", Points: series})
		}
	}
	perPool, err := s.collectLendingPoolSeries(ctx, lendingPerPoolSeriesQuery(windowDays), since)
	if err != nil {
		return err
	}
	blk.Series = append(blk.Series, perPool...)
	return nil
}

// collectLendingPoolSeries folds lendingPerPoolSeriesQuery rows into per-pool series in events-descending order, labelled by truncated
// contract id.
func (s *Store) collectLendingPoolSeries(ctx context.Context, query, since string) ([]BespokeSeries, error) {
	rows, err := s.db.QueryContext(ctx, query, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeLending per-pool series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out     []BespokeSeries
		lastKey string
	)
	for rows.Next() {
		var key string
		var pt BespokeSeriesPt
		if err := rows.Scan(&key, &pt.Date, &pt.Value); err != nil {
			return nil, fmt.Errorf("timescale: bespokeLending per-pool series scan: %w", err)
		}
		if len(out) == 0 || key != lastKey {
			out = append(out, BespokeSeries{Name: "Pool · " + truncLendingID(key), Unit: "events"})
			lastKey = key
		}
		out[len(out)-1].Points = append(out[len(out)-1].Points, pt)
	}
	return out, rows.Err()
}

// lendingActivityBreakdowns fills position events by pool (top 10) and by kind; event-count weighted, the only honest weight across mixed
// assets.
func (s *Store) lendingActivityBreakdowns(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	byPool, err := s.collectLendingBreakdown(ctx,
		BespokeBreakdown{Title: "Activity by pool", Unit: "events"},
		`SELECT pool, count(*)::text, count(*)
		   FROM blend_positions
		  WHERE ledger_close_time > now() - $1::interval
		  GROUP BY pool ORDER BY count(*) DESC LIMIT 10`, since, truncLendingID)
	if err != nil {
		return err
	}
	if len(byPool.Rows) > 0 {
		blk.Breakdowns = append(blk.Breakdowns, byPool)
	}

	byKind, err := s.collectLendingBreakdown(ctx,
		BespokeBreakdown{Title: "Position events by kind", Unit: "events"},
		`SELECT event_kind, count(*)::text, count(*)
		   FROM blend_positions
		  WHERE ledger_close_time > now() - $1::interval
		  GROUP BY event_kind ORDER BY count(*) DESC`, since, func(k string) string { return k })
	if err != nil {
		return err
	}
	if len(byKind.Rows) > 0 {
		blk.Breakdowns = append(blk.Breakdowns, byKind)
	}

	blk.Notes = append(blk.Notes,
		fmt.Sprintf("Activity breakdowns, event-count KPIs and the event series are COUNTS of position events over the %dd window, not value-weighted volume: blend_positions rows mix many tokens at per-asset decimals with no USD valuation at this layer, so summing raw amounts across assets would be meaningless. Pools are labelled by truncated contract id — Blend pools have no on-chain name.", windowDays),
	)
	return nil
}

// collectLendingBreakdown runs one (key, value_text, count) composition query; query is a hard-coded constant, never request-derived.
func (s *Store) collectLendingBreakdown(ctx context.Context, base BespokeBreakdown, query, since string, label func(string) string) (BespokeBreakdown, error) {
	rows, err := s.db.QueryContext(ctx, query, since)
	if err != nil {
		return base, fmt.Errorf("timescale: bespokeLending breakdown %q: %w", base.Title, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var row BespokeBreakdownRow
		if err := rows.Scan(&key, &row.Value, &row.Count); err != nil {
			return base, fmt.Errorf("timescale: bespokeLending breakdown %q scan: %w", base.Title, err)
		}
		row.Label = label(key)
		base.Rows = append(base.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return base, fmt.Errorf("timescale: bespokeLending breakdown %q rows: %w", base.Title, err)
	}
	return base, nil
}

// lendingFlashLoanTable fills recent flash loans, RECENCY-ordered: ranking mixed-asset raw amounts would compare different decimal scales.
// Omitted when no flash loan landed in the window.
func (s *Store) lendingFlashLoanTable(ctx context.Context, blk *BespokeBlock, since string) error {
	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Recent flash loans", Columns: []string{"When", "Pool", "Asset", "Amount (raw token units)", "Borrower", "Tx"}},
		`SELECT to_char(ledger_close_time, 'YYYY-MM-DD HH24:MI'),
		        pool,
		        asset,
		        token_amount::text,
		        COALESCE(counterparty, user_address),
		        tx_hash
		   FROM blend_positions
		  WHERE event_kind = 'flash_loan' AND ledger_close_time > now() - $1::interval
		  ORDER BY ledger_close_time DESC LIMIT 10`, since)
	if err != nil {
		return err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
		blk.Notes = append(blk.Notes,
			"Recent flash loans are recency-ordered (never amount-ordered — raw amounts of different tokens are not comparable). Amount is the flashed asset's own base units; Borrower is the contract that received the loan (topic[3] of the flash_loan event, falling back to the initiating user when absent).",
		)
	}
	return nil
}

// truncLendingID renders "CAJJ…BXBD" for a display label; duplicates the API's truncPositionsContract because storage never imports api
// packages.
func truncLendingID(id string) string {
	if len(id) <= 9 {
		return id
	}
	return id[:4] + "…" + id[len(id)-4:]
}

// lendingEmissionKPIs augments the Blend lending block with emission and credit-risk KPIs from
// blend_emissions: claimed-emission volume, claim/gulp counts and a (bad_debt + defaulted_debt)
// event count. Claim amounts are token base units, not USD. Empty-safe.

// bespokeCredit builds the sorocredit (consumer-USDC credit / CDP) bespoke block from the four
// credit_* hypertables.
//
// CRITICAL: credit_settlements rows are the on-wire "Liquidation" event but are recurring SCHEDULED
// keeper settlements, NOT distressed liquidations. Every label and Note says so; never surface a
// "liquidations" risk signal from this data.
//
// Returns nil (not an error) when no credit_* row exists in the window. Amounts are token base
// units, never USD (no published price). A SAC wrapping a classic asset is 7-decimal on chain,
// never the off-chain 6-decimal USDC convention (see creditAmountUnitsNote,
// creditSettlementVolumeHint).
const (
	creditAmountUnitsNote      = "Amounts are in token base units (USDC settlements at 7-decimal USDC base units; statement amounts at the protocol's i128 scale) — NOT USD. sorocredit has no published price and never contributes to VWAP."
	creditSettlementVolumeHint = "summed scheduled-settlement amount in 7-decimal USDC base units (NOT a liquidation/risk signal)"
)

func (s *Store) bespokeCredit(ctx context.Context, windowDays int) (*BespokeBlock, error) {
	a, err := s.CreditWindowAnalytics(ctx, windowDays)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeCredit analytics: %w", err)
	}
	if a == nil {
		return nil, nil // no activity in the window — omit the panel
	}

	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "lending",
		Notes: []string{
			"Settlements are SCHEDULED settlements decoded from the on-wire \"Liquidation\" event — a single keeper settles published statements on a recurring schedule (~1:1 with statements). These are NOT distressed liquidations; do not read them as a risk/liquidation signal.",
			"Open positions is a WINDOW-SCOPED proxy: positions opened in the window whose collateral child has no withdrawal (cash-out) observed in the window. It is not an all-time live-position count (the served tier is retention-scoped).",
			creditAmountUnitsNote,
		},
	}

	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Positions opened (%dd)", windowDays), Value: strconv.FormatInt(a.PositionsOpened, 10), Hint: "NewCollateralContract events (one child position per open) in the window"},
		BespokeKPI{Label: fmt.Sprintf("Open positions (%dd)", windowDays), Value: strconv.FormatInt(a.OpenPositions, 10), Hint: "opened in the window without an observed withdrawal in the window (window-scoped proxy, not all-time)"},
		BespokeKPI{Label: fmt.Sprintf("Unique users (%dd)", windowDays), Value: strconv.FormatInt(a.UniqueUsers, 10), Hint: "distinct position owners (G-addresses)"},
		BespokeKPI{Label: fmt.Sprintf("Statements published (%dd)", windowDays), Value: strconv.FormatInt(a.Statements, 10), Hint: "StatementPublished events (periodic per-position charge statements)"},
		BespokeKPI{Label: fmt.Sprintf("Scheduled settlements (%dd)", windowDays), Value: strconv.FormatInt(a.Settlements, 10), Hint: "recurring keeper settlements of published statements — NOT distressed liquidations"},
		BespokeKPI{Label: fmt.Sprintf("Settlement volume (%dd)", windowDays), Value: a.SettlementVolume.String(), Unit: "USDC-units", Hint: creditSettlementVolumeKPIHint(a.SettlementsNotFullySummed)},
		BespokeKPI{Label: fmt.Sprintf("Withdrawals (%dd)", windowDays), Value: strconv.FormatInt(a.Withdrawals, 10), Hint: "position cash-out events in the window"},
	)
	if !a.LatestActivity.IsZero() {
		blk.KPIs = append(blk.KPIs, BespokeKPI{
			Label: "Latest activity",
			Value: a.LatestActivity.UTC().Format("2006-01-02 15:04"),
			Unit:  "UTC",
			Hint:  "most recent credit event across positions/statements/settlements/withdrawals",
		})
	}

	// Scheduled-settlement volume per bucket, in native USDC base units, not USD. The name is
	// window-stable; the grain lives in the point timestamps (the CCTP series convention).
	series, err := s.scanDailySeries(ctx, creditSettlementSeriesQuery(windowDays), since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Settlement volume", Unit: "USDC-units", Points: series})
	}
	if a.SettlementsNotFullySummed > 0 {
		blk.Notes = append(blk.Notes, "Settlement volume (KPI and series) is a LOWER BOUND: it sums only USDC primary legs; "+creditPartialSettlementsPhrase(a.SettlementsNotFullySummed)+" carry a non-USDC or undecoded primary leg, or extra debt legs, whose amounts are not summed.")
	}

	if err := s.creditPositionExtras(ctx, blk, windowDays, since); err != nil {
		return nil, err
	}

	// Recent scheduled settlements. Ordered newest-first; settled_amount is
	// rendered as text so the i128 NUMERIC never round-trips through int64.
	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Recent scheduled settlements", Columns: []string{"When", "Position", "Debt asset", "Settled amount", "Settler"}},
		`SELECT to_char(ledger_close_time, 'YYYY-MM-DD HH24:MI'),
		        position_uuid,
		        COALESCE(debt_asset, '—'),
		        COALESCE(settled_amount::text, '—'),
		        settler_account
		   FROM credit_settlements WHERE ledger_close_time > now() - $1::interval
		  ORDER BY ledger_close_time DESC LIMIT 25`, since)
	if err != nil {
		return nil, err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}

	return blk, nil
}

// creditSettlementVolumeKPIHint returns the settlement-volume KPI hint, a
// LOWER BOUND naming the excluded settlements when any is not fully summed.
func creditSettlementVolumeKPIHint(notFullySummed int64) string {
	if notFullySummed <= 0 {
		return creditSettlementVolumeHint
	}
	return "LOWER BOUND: " + creditSettlementVolumeHint + "; excludes the non-USDC, undecoded or extra debt legs of " + creditPartialSettlementsPhrase(notFullySummed)
}

func creditPartialSettlementsPhrase(n int64) string {
	if n == 1 {
		return "1 settlement"
	}
	return strconv.FormatInt(n, 10) + " settlements"
}

// creditSettlementSeriesQuery builds scheduled-settlement volume at the window's grain, summing only USDC primary legs (7-decimal base
// units) so it stays single-denomination; bespokeCredit flags a lower bound when a settlement is not fully summed.
func creditSettlementSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), COALESCE(sum(settled_amount) FILTER (WHERE ` + creditUSDCLegFilter + `),0)::text
		FROM credit_settlements WHERE ledger_close_time > now() - $1::interval` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		GROUP BY 1 ORDER BY 1 ASC`
}

// creditPositionsOpenedSeriesQuery builds the positions-opened count series, one NewCollateralContract event per position
// (credit_positions).
func creditPositionsOpenedSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), count(*)::text
		FROM credit_positions WHERE ledger_close_time > now() - $1::interval` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		GROUP BY 1 ORDER BY 1 ASC`
}

// creditAllTimePositionsKPIQuery is the unwindowed lifetime scale of the position book; credit_positions is the only credit_* table with an
// owner column, so unique users is scoped to position opens.
func creditAllTimePositionsKPIQuery() string {
	return `
		SELECT count(*)::text, count(DISTINCT owner)::text
		  FROM credit_positions`
}

// creditPositionExtras appends the credit_positions-derived series (positions opened) and all-time position-book KPIs.
func (s *Store) creditPositionExtras(ctx context.Context, blk *BespokeBlock, windowDays int, since string) error {
	series, err := s.scanDailySeries(ctx, creditPositionsOpenedSeriesQuery(windowDays), since)
	if err != nil {
		return err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Positions opened", Unit: "positions", Points: series})
	}

	var atPositions, atOwners string
	if err := s.db.QueryRowContext(ctx, creditAllTimePositionsKPIQuery()).
		Scan(&atPositions, &atOwners); err != nil {
		return fmt.Errorf("timescale: bespokeCredit all-time position KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: "Positions opened (all-time)", Value: atPositions, Unit: "positions", Hint: "every NewCollateralContract event ever captured (r1 history starts 2026-03)"},
		BespokeKPI{Label: "Unique users (all-time)", Value: atOwners, Hint: "distinct position owners across all captured opens — credit_positions is the only credit table carrying an owner, so this is scoped to position opens"},
	)
	return nil
}

// bespokeYield builds the DeFindex vault bespoke block from defindex_flows: windowed gross
// deposit/withdraw volume, per-vault net flow (an AUM proxy) and unique actors. The series and
// per-vault net scope to the vault layer to avoid double-counting a deposit that fans out into
// strategies.

package timescale

// Split out of protocol_bespoke.go (2026-07-30) so per-category visual
// suites can be built in parallel without colliding on one file. Shared
// types (BespokeBlock/KPI/Series/Table/Breakdown) + the dispatcher + the
// scan helpers stay in protocol_bespoke.go.

// bespokeLending builds the Blend lending bespoke block: PER-ASSET net
// supplied (supply + supply_collateral − withdraw − withdraw_collateral) and
// net borrowed (borrow − repay) from blend_positions, plus auction and
// backstop activity. blend_positions amounts are unsigned magnitudes — the
// sign is the event_kind, not the value — so net positions are signed sums of
// the gross magnitudes, and they are only ever summed WITHIN one asset
// (cross-asset sums mix per-token decimals — see the count-first block
// comment below; the old headline cross-asset "Net supplied/borrowed" KPIs
// and per-pool Util% were dropped 2026-07-31 for exactly that reason).
// Confirmed on r1: event_kind ∈ {supply, withdraw, supply_collateral,
// withdraw_collateral, borrow, repay, flash_loan}; token_amount +
// b_or_d_amount are both ≥ 0.
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
			"Per-asset net supplied / net borrowed are signed running sums of unsigned blend_positions.token_amount over the window — supply/supply_collateral add, withdraw/withdraw_collateral subtract for supplied; borrow adds, repay subtracts for borrowed. They are WINDOW deltas scoped to ONE asset each, not all-time TVL (the served tier is retention-scoped); flash_loan is excluded. Amounts are never summed across assets: tokens carry different decimals, so a cross-asset sum would be a meaningless number.",
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

// ─── Blend visual-suite extensions (2026-07-30) ─────────────────────────
//
// Everything below is COUNT-first on purpose: blend_positions /
// blend_backstop_events rows mix many tokens at per-asset decimals, and no
// USD valuation exists for them at this layer — summing different tokens'
// raw amounts across rows would be a meaningless (and dishonest) number.
// Amounts appear only where a single row's single asset is shown (the
// flash-loan table) or where an existing surface already scopes a sum
// per-asset. Ground-truthed on r1 2026-07-30: blend_positions 973,390 rows
// (2024-05-02 →), 15 pools; blend_backstop_events 93,243 rows;
// blend_auctions 9,908 rows.
//
// Series names are window-stable (no "(Nd)" suffix, no "Daily" prefix):
// the grain — hourly at the 24h window, daily otherwise, via
// bridgeSeriesGrain — lives in the point timestamps, mirroring the CCTP
// series convention.

// lendingSupplySideKinds / lendingBorrowSideKinds partition all seven
// blend_positions event kinds into the two sides of the money market:
// supply-side = lender-leg movements (deposits AND their withdrawals),
// borrow-side = debt-leg movements (borrow, repay, flash_loan — a
// flash loan is a same-tx borrow). Together they cover every kind, so
// the two lines sum to total position events.
const (
	lendingSupplySideKinds = `'supply','supply_collateral','withdraw','withdraw_collateral'`
	lendingBorrowSideKinds = `'borrow','repay','flash_loan'`
)

// lendingBackstopInflowKinds / lendingBackstopOutflowKinds are the
// backstop event kinds that move funds INTO vs OUT OF a backstop
// (deposit/donate vs withdraw/draw). The remaining kinds (claim,
// queue/dequeue_withdrawal, distribute, gulp_emissions, rw_zone*) are
// accounting/queueing events, not fund flows, and are excluded from the
// flow series (they stay in the backstop-events KPI count).
const (
	lendingBackstopInflowKinds  = `'deposit','donate'`
	lendingBackstopOutflowKinds = `'withdraw','draw'`
)

// lendingSideSeriesQuery builds the per-bucket position-event count series
// for one side of the money market at the window's grain (hourly at 24h —
// bridgeSeriesGrain). kinds MUST be one of the lending*SideKinds constants
// (never request-derived).
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

// lendingBackstopFlowSeriesQuery builds the per-bucket backstop flow-event
// count series for one direction. kinds MUST be one of the
// lendingBackstop*Kinds constants.
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

// lendingAuctionSeriesQuery builds the per-bucket auction-event count
// series (all three lifecycle kinds — new/fill/delete; the started-vs-
// filled split lives in the KPIs). blend_auctions' time column is ts.
func lendingAuctionSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ts), '` + format + `'), count(*)::text
		  FROM blend_auctions
		 WHERE ts > now() - $1::interval` +
		completeDaysOnly(windowDays, "ts") + `
		 GROUP BY 1 ORDER BY 1 ASC`
}

// lendingPerPoolSeriesQuery builds the top-5-pools-by-window-events
// activity series: (pool, bucket, events) rows ordered by each pool's
// window event count descending, then bucket — the same fold shape as the
// CCTP per-chain series.
func lendingPerPoolSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		WITH top AS (
		  SELECT pool, count(*) AS events
		    FROM blend_positions
		   WHERE ledger_close_time > now() - $1::interval
		   GROUP BY 1 ORDER BY 2 DESC LIMIT 5)
		SELECT p.pool,
		       to_char(date_trunc('` + trunc + `', p.ledger_close_time), '` + format + `'),
		       count(*)::text
		  FROM blend_positions p JOIN top USING (pool)
		 WHERE p.ledger_close_time > now() - $1::interval` +
		completeDaysOnly(windowDays, "p.ledger_close_time") + `
		 GROUP BY p.pool, 2, top.events
		 ORDER BY top.events DESC, 2 ASC`
}

// lendingAllTimeKPIQuery is the unwindowed lifetime scale of the Blend
// money market: total position events, supply-side / borrow-side event
// counts, distinct users, and distinct pools. Deliberately NOT
// window-bounded (an explicit all-time aggregate; the whole block is
// built under the window-keyed protocol-detail cache).
func lendingAllTimeKPIQuery() string {
	return `
		SELECT count(*)::text,
		       count(*) FILTER (WHERE event_kind IN (` + lendingSupplySideKinds + `))::text,
		       count(*) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `))::text,
		       count(DISTINCT user_address)::text,
		       count(DISTINCT pool)::text
		  FROM blend_positions`
}

// lendingScaleKPIs fills the window supply/borrow event counts + active
// pools, and their all-time equivalents. Counts, not amounts — see the
// section comment.
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

// lendingActivitySeries appends the chart lines: supply-side vs
// borrow-side position events (a paired two-line chart covering every
// event kind), backstop inflow vs outflow events, auction events, and one
// activity line per top-5 pool. All counts (see the section comment).
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

// collectLendingPoolSeries folds the ordered (pool, bucket, count) rows of
// lendingPerPoolSeriesQuery into per-pool BespokeSeries, preserving the
// query's events-descending pool order (the CCTP per-chain fold shape).
// Pools are labelled by truncated contract id — no pool-name registry
// exists in the served tier.
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

// lendingActivityBreakdowns fills the two composition datasets (the donut
// complement to the series): position events by pool (top 10) and by event
// kind (all seven). Both are event-count weighted — the only honest weight
// across mixed-asset rows (see the section comment); Value and Count carry
// the same event tally so count-rendering and value-rendering consumers
// agree.
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

// collectLendingBreakdown runs one (key, value_text, count) composition
// query and labels its rows via label. query is a hard-coded constant per
// call site (never request-derived).
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

// lendingFlashLoanTable fills the recent-flash-loans table — the showcase
// event class the auction table doesn't cover (r1: 806 flash loans in the
// trailing 30d). Rows are RECENCY-ordered, never amount-ordered: ranking
// mixed-asset raw amounts against each other would compare numbers at
// different per-asset decimal scales. Each row shows its own single
// asset's raw amount honestly; Tx carries the full hash like the bridge
// largest-transfers table. Empty-safe: omitted when no flash loan landed
// in the window.
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

// truncLendingID renders "CAJJ…BXBD" for a contract/account id — a
// display label for breakdown slices and per-pool series names where a
// 56-char strkey would overwhelm the chart. A private duplicate of the
// API layer's truncPositionsContract on purpose: storage never imports
// api packages (the same no-upward-import reasoning as sorocredit.go's
// row types).
func truncLendingID(id string) string {
	if len(id) <= 9 {
		return id
	}
	return id[:4] + "…" + id[len(id)-4:]
}

// lendingEmissionKPIs augments the Blend lending block with emission /
// credit-risk KPIs from blend_emissions (migration 0045) — claimed-emission
// volume + claim/gulp counts, and a credit-risk (bad_debt + defaulted_debt)
// event count surfaced honestly. Claim amounts are token base units, not
// USD. Empty-safe: a no-op when no emission event was captured.

// bespokeCredit builds the sorocredit (consumer-USDC credit / CDP protocol)
// lending bespoke block from the four credit_* hypertables (migration 0090):
// positions opened + a window-scoped open-position proxy, statements
// published, SCHEDULED settlements (volume + count), withdrawals, and a
// recent-settlements table.
//
// CRITICAL semantic: credit_settlements rows are the on-wire "Liquidation"
// event but are recurring SCHEDULED keeper settlements of published
// statements — NOT distressed liquidations. Every settlement label + the
// appended Note says so; a "liquidations" risk signal must never be surfaced
// from this data (migration 0090 header + internal/sources/sorocredit).
//
// Empty-safe: returns nil (not an error) when no credit_* row exists in the
// window, so /v1/protocols/sorocredit degrades to its generic analytics —
// r1's credit_* tables are empty until the sorocredit projector-replay runs
// post-deploy. Amounts are USDC / token base units (per-asset decimals),
// never USD (sorocredit has no published price).
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
			"Amounts are in token base units (USDC settlements/withdrawals at 6-decimal USDC base units; statement amounts at the protocol's i128 scale) — NOT USD. sorocredit has no published price and never contributes to VWAP.",
		},
	}

	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Positions opened (%dd)", windowDays), Value: strconv.FormatInt(a.PositionsOpened, 10), Hint: "NewCollateralContract events (one child position per open) in the window"},
		BespokeKPI{Label: fmt.Sprintf("Open positions (%dd)", windowDays), Value: strconv.FormatInt(a.OpenPositions, 10), Hint: "opened in the window without an observed withdrawal in the window (window-scoped proxy, not all-time)"},
		BespokeKPI{Label: fmt.Sprintf("Unique users (%dd)", windowDays), Value: strconv.FormatInt(a.UniqueUsers, 10), Hint: "distinct position owners (G-addresses)"},
		BespokeKPI{Label: fmt.Sprintf("Statements published (%dd)", windowDays), Value: strconv.FormatInt(a.Statements, 10), Hint: "StatementPublished events (periodic per-position charge statements)"},
		BespokeKPI{Label: fmt.Sprintf("Scheduled settlements (%dd)", windowDays), Value: strconv.FormatInt(a.Settlements, 10), Hint: "recurring keeper settlements of published statements — NOT distressed liquidations"},
		BespokeKPI{Label: fmt.Sprintf("Settlement volume (%dd)", windowDays), Value: a.SettlementVolume.String(), Unit: "USDC-units", Hint: "summed scheduled-settlement amount in 6-decimal USDC base units (NOT a liquidation/risk signal)"},
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

	// Scheduled-settlement volume per bucket (the protocol's dominant
	// recurring flow). Native USDC base units, not USD. Name is
	// window-stable ("Settlement volume", previously "Daily settlement
	// volume"): the grain — hourly at the 24h window, daily otherwise —
	// lives in the point timestamps (the CCTP series convention).
	series, err := s.scanDailySeries(ctx, creditSettlementSeriesQuery(windowDays), since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Settlement volume", Unit: "USDC-units", Points: series})
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

// creditSettlementSeriesQuery builds the scheduled-settlement volume
// series at the window's grain (hourly at 24h via bridgeSeriesGrain,
// daily otherwise). Summing settled_amount is honest here — unlike the
// mixed-asset Blend tables, every settlement's primary leg is the same
// 6-decimal USDC unit (migration 0090).
func creditSettlementSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), COALESCE(sum(settled_amount),0)::text
		FROM credit_settlements WHERE ledger_close_time > now() - $1::interval` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		GROUP BY 1 ORDER BY 1 ASC`
}

// creditPositionsOpenedSeriesQuery builds the positions-opened count
// series at the window's grain — one NewCollateralContract event per
// opened position (credit_positions).
func creditPositionsOpenedSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'), count(*)::text
		FROM credit_positions WHERE ledger_close_time > now() - $1::interval` +
		completeDaysOnly(windowDays, "ledger_close_time") + `
		GROUP BY 1 ORDER BY 1 ASC`
}

// creditAllTimePositionsKPIQuery is the unwindowed lifetime scale of the
// sorocredit position book: positions ever opened + distinct owners.
// credit_positions is the only credit_* table with a real owner column
// (credit_events carries no owner — see the migration 0090 shapes), so
// the all-time unique-users figure is honestly scoped to position opens.
// Deliberately NOT window-bounded (r1 2026-07-30: 105,896 rows — a
// trivially cheap aggregate under the window-keyed detail cache).
func creditAllTimePositionsKPIQuery() string {
	return `
		SELECT count(*)::text, count(DISTINCT owner)::text
		  FROM credit_positions`
}

// creditPositionExtras appends the credit_positions-derived visual-suite
// extensions: the positions-opened series (window grain) and the
// all-time position-book KPIs alongside the existing window-scoped set.
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

// bespokeYield builds the DeFindex vault bespoke block from defindex_flows:
// windowed gross deposit / withdraw flow volume (by direction), per-vault net
// flow (deposit − withdraw, an AUM proxy), and unique actors. Confirmed on r1:
// direction ∈ {deposit, withdraw}; layer ∈ {strategy, vault}. The series and
// per-vault net scope to the vault layer to avoid double-counting a deposit
// that fans out into strategies.

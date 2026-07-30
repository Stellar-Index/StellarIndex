package timescale

// Split out of protocol_bespoke.go (2026-07-30) so per-category visual
// suites can be built in parallel without colliding on one file. Shared
// types (BespokeBlock/KPI/Series/Table/Breakdown) + the dispatcher + the
// scan helpers stay in protocol_bespoke.go.

// bespokeLending builds the Blend lending bespoke block: per-asset net
// supplied (supply + supply_collateral − withdraw − withdraw_collateral) and
// net borrowed (borrow − repay) from blend_positions, plus auction and
// backstop activity. blend_positions amounts are unsigned magnitudes — the
// sign is the event_kind, not the value — so net positions are signed sums of
// the gross magnitudes. Confirmed on r1: event_kind ∈ {supply, withdraw,
// supply_collateral, withdraw_collateral, borrow, repay, flash_loan};
// token_amount + b_or_d_amount are both ≥ 0.
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
			"Net supplied / net borrowed are signed running sums of unsigned blend_positions.token_amount over the window — supply/supply_collateral add, withdraw/withdraw_collateral subtract for supplied; borrow adds, repay subtracts for borrowed. They are WINDOW deltas, not all-time TVL (the served tier is retention-scoped); flash_loan is excluded.",
			"Asset is a Soroban token contract id, shown shortened; amounts are in the token's base units (per-asset decimals).",
			"Per-pool 'Util %' is the window borrow/supply ratio — a coarse proxy, not on-chain utilisation (which is current reserve borrowed/supplied). Real current-state TVL + supply/borrow APYs need the Soroban pool-storage reader (reserve b_rate/d_rate + totals from contract storage); this block is event-derived and window-scoped until that ships.",
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
	return blk, nil
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

	// Daily scheduled-settlement volume (the protocol's dominant recurring
	// flow). Native USDC base units, not USD.
	series, err := s.scanDailySeries(ctx, `
		SELECT to_char(date_trunc('day', ledger_close_time), 'YYYY-MM-DD'), COALESCE(sum(settled_amount),0)::text
		FROM credit_settlements WHERE ledger_close_time > now() - $1::interval
		GROUP BY 1 ORDER BY 1 ASC`, since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily settlement volume", Unit: "USDC-units", Points: series})
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

// bespokeYield builds the DeFindex vault bespoke block from defindex_flows:
// windowed gross deposit / withdraw flow volume (by direction), per-vault net
// flow (deposit − withdraw, an AUM proxy), and unique actors. Confirmed on r1:
// direction ∈ {deposit, withdraw}; layer ∈ {strategy, vault}. The series and
// per-vault net scope to the vault layer to avoid double-counting a deposit
// that fans out into strategies.

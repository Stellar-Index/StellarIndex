// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// assertLendingCountsNotAmountSums guards the lending honesty rule: series
// and breakdown queries over the mixed-asset blend tables must weight by
// event COUNT, never by summed raw token amounts (rows mix many tokens at
// per-asset decimals with no USD valuation — a cross-asset sum is
// meaningless).
func assertLendingCountsNotAmountSums(t *testing.T, name, q string) {
	t.Helper()
	if !strings.Contains(q, "count(*)") {
		t.Errorf("%s must be event-count weighted", name)
	}
	for _, forbidden := range []string{"sum(token_amount", "sum(b_or_d_amount", "sum(amount"} {
		if strings.Contains(q, forbidden) {
			t.Errorf("%s must not sum raw token amounts across assets (%s)", name, forbidden)
		}
	}
}

// TestLendingSideKindsPartition guards that the two series sides together
// cover all seven blend_positions event kinds exactly once — so the
// supply-side and borrow-side lines sum to total position events.
func TestLendingSideKindsPartition(t *testing.T) {
	all := lendingSupplySideKinds + "," + lendingBorrowSideKinds
	for _, kind := range []string{
		"'supply'", "'withdraw'", "'supply_collateral'", "'withdraw_collateral'",
		"'borrow'", "'repay'", "'flash_loan'",
	} {
		if !strings.Contains(all, kind) {
			t.Errorf("event kind %s missing from the side partition", kind)
		}
		if strings.Count(all, kind) != 1 {
			t.Errorf("event kind %s must appear exactly once across the two sides", kind)
		}
	}
}

// TestLendingSeriesQueryGrain guards the hourly-at-24h / daily-otherwise
// bucketing rule for every windowed lending series builder (the same rule
// TestBridgeSeriesGrain pins for the shared helper).
func TestLendingSeriesQueryGrain(t *testing.T) {
	builders := map[string]func(int) string{
		"supply-side series": func(d int) string { return lendingSideSeriesQuery(d, lendingSupplySideKinds) },
		"borrow-side series": func(d int) string { return lendingSideSeriesQuery(d, lendingBorrowSideKinds) },
		"backstop inflow":    func(d int) string { return lendingBackstopFlowSeriesQuery(d, lendingBackstopInflowKinds) },
		"backstop outflow":   func(d int) string { return lendingBackstopFlowSeriesQuery(d, lendingBackstopOutflowKinds) },
		"auction series":     lendingAuctionSeriesQuery,
		"per-pool series":    lendingPerPoolSeriesQuery,
		"credit settlements": creditSettlementSeriesQuery,
		"credit positions":   creditPositionsOpenedSeriesQuery,
	}
	for name, build := range builders {
		hourly := build(1)
		if !strings.Contains(hourly, "date_trunc('hour'") || !strings.Contains(hourly, "HH24:00") {
			t.Errorf("%s at 24h must bucket by hour with the hour in the timestamp format", name)
		}
		daily := build(30)
		if !strings.Contains(daily, "date_trunc('day'") || strings.Contains(daily, "HH24:00") {
			t.Errorf("%s at 30d must bucket by day with a date-only timestamp format", name)
		}
		for _, q := range []string{hourly, daily} {
			if !strings.Contains(q, "now() - $1::interval") {
				t.Errorf("%s must be window-bounded", name)
			}
		}
	}
}

// TestLendingSeriesQueriesCountBased guards that the blend activity series
// count events rather than summing mixed-asset raw amounts, and that the
// two flow-direction filters select the intended event kinds.
func TestLendingSeriesQueriesCountBased(t *testing.T) {
	for name, q := range map[string]string{
		"supply-side series": lendingSideSeriesQuery(30, lendingSupplySideKinds),
		"borrow-side series": lendingSideSeriesQuery(30, lendingBorrowSideKinds),
		"backstop inflow":    lendingBackstopFlowSeriesQuery(30, lendingBackstopInflowKinds),
		"backstop outflow":   lendingBackstopFlowSeriesQuery(30, lendingBackstopOutflowKinds),
		"auction series":     lendingAuctionSeriesQuery(30),
		"per-pool series":    lendingPerPoolSeriesQuery(30),
	} {
		assertLendingCountsNotAmountSums(t, name, q)
	}

	if q := lendingSideSeriesQuery(30, lendingBorrowSideKinds); !strings.Contains(q, "'flash_loan'") {
		t.Error("borrow-side series must include flash_loan (a same-tx borrow)")
	}
	inflow := lendingBackstopFlowSeriesQuery(30, lendingBackstopInflowKinds)
	if !strings.Contains(inflow, "'deposit'") || !strings.Contains(inflow, "'donate'") {
		t.Error("backstop inflow series must select deposit + donate")
	}
	if strings.Contains(inflow, "'claim'") || strings.Contains(inflow, "'gulp_emissions'") {
		t.Error("backstop flow series must exclude accounting events (claim / gulp_emissions are not fund flows)")
	}
	outflow := lendingBackstopFlowSeriesQuery(30, lendingBackstopOutflowKinds)
	if !strings.Contains(outflow, "'withdraw'") || !strings.Contains(outflow, "'draw'") {
		t.Error("backstop outflow series must select withdraw + draw")
	}
}

// TestLendingPerPoolSeriesQueryShape guards the top-5 cap and the
// volume-descending fold order the per-pool series collector depends on
// (rows for one pool must arrive contiguously, most-active pool first).
func TestLendingPerPoolSeriesQueryShape(t *testing.T) {
	q := lendingPerPoolSeriesQuery(30)
	if !strings.Contains(q, "LIMIT 5") {
		t.Error("per-pool series must cap at the top 5 pools by window events")
	}
	if !strings.Contains(q, "ORDER BY top.events DESC, 2 ASC") {
		t.Error("per-pool series must order pools by window events descending, then bucket, so the fold groups rows per pool")
	}
}

// TestLendingAllTimeKPIQueryShape guards that the lifetime scale KPIs are
// explicitly unwindowed, count-based, and carry the distinct-user and
// distinct-pool tallies.
func TestLendingAllTimeKPIQueryShape(t *testing.T) {
	q := lendingAllTimeKPIQuery()
	if strings.Contains(q, "$1") || strings.Contains(q, "interval") {
		t.Error("all-time KPIs must not be window-bounded")
	}
	assertLendingCountsNotAmountSums(t, "all-time KPIs", q)
	if !strings.Contains(q, "DISTINCT user_address") {
		t.Error("all-time KPIs must count distinct users")
	}
	if !strings.Contains(q, "DISTINCT pool") {
		t.Error("all-time KPIs must count distinct pools")
	}
}

// TestCreditQueriesShape guards the sorocredit extensions: the settlement
// series may sum settled_amount (single-denomination USDC — the one
// honest sum), the positions series is count-based, and the all-time
// position KPIs are unwindowed and scoped to credit_positions (the only
// credit_* table with an owner column).
func TestCreditQueriesShape(t *testing.T) {
	settle := creditSettlementSeriesQuery(30)
	if !strings.Contains(settle, "sum(settled_amount)") {
		t.Error("settlement series must sum settled_amount (single-denomination USDC base units)")
	}
	if !strings.Contains(settle, "credit_settlements") {
		t.Error("settlement series must read credit_settlements")
	}

	opened := creditPositionsOpenedSeriesQuery(30)
	if !strings.Contains(opened, "credit_positions") || !strings.Contains(opened, "count(*)") {
		t.Error("positions-opened series must count credit_positions rows")
	}

	kpi := creditAllTimePositionsKPIQuery()
	if strings.Contains(kpi, "$1") || strings.Contains(kpi, "interval") {
		t.Error("all-time position KPIs must not be window-bounded")
	}
	if !strings.Contains(kpi, "DISTINCT owner") || !strings.Contains(kpi, "credit_positions") {
		t.Error("all-time position KPIs must count distinct owners from credit_positions")
	}
}

// TestTruncLendingID guards the display truncation for pool labels
// ("CAJJ…BXBD") and the pass-through of already-short ids.
func TestTruncLendingID(t *testing.T) {
	if got := truncLendingID("CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"); got != "CAJJ…BXBD" {
		t.Errorf("truncLendingID(long) = %q, want CAJJ…BXBD", got)
	}
	if got := truncLendingID("short"); got != "short" {
		t.Errorf("truncLendingID(short) = %q, want pass-through", got)
	}
}

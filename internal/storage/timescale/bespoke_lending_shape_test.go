// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
	if strings.Contains(q, "$1") || sqlContainsFold(q, "interval") {
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
	if strings.Contains(kpi, "$1") || sqlContainsFold(kpi, "interval") {
		t.Error("all-time position KPIs must not be window-bounded")
	}
	if !strings.Contains(kpi, "DISTINCT owner") || !strings.Contains(kpi, "credit_positions") {
		t.Error("all-time position KPIs must count distinct owners from credit_positions")
	}
}

// TestCreditUSDCDecimalsAreClassicSevenNotOffChainSix guards the sorocredit
// bespoke block's served decimal-scale documentation. sorocredit's USDC leg
// is USDC's Stellar-classic SAC wrapper, and every classic Stellar asset —
// including a SAC wrapping one — is uniformly 7-decimal fixed point
// on-chain (see cmd/stellarindex-aggregator/main.go: "classic (7-decimal)
// credit asset"), never the off-chain (e.g. Ethereum) 6-decimal USDC
// convention. A consumer trusting a "6-decimal" hint would misread every
// served settlement/withdrawal amount by 10x.
func TestCreditUSDCDecimalsAreClassicSevenNotOffChainSix(t *testing.T) {
	for name, note := range map[string]string{
		"creditAmountUnitsNote":      creditAmountUnitsNote,
		"creditSettlementVolumeHint": creditSettlementVolumeHint,
	} {
		if !strings.Contains(note, "7-decimal") {
			t.Errorf("%s must document the classic Stellar 7-decimal USDC scale, got %q", name, note)
		}
		if strings.Contains(note, "6-decimal") {
			t.Errorf("%s must not claim USDC's off-chain 6-decimal scale for an on-chain classic-asset amount, got %q", name, note)
		}
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

// blendNetArm matches one signed arm of a blend_positions net CASE:
// `WHEN event_kind = 'k' THEN [-]token_amount` or the IN (...) form.
var blendNetArm = regexp.MustCompile(`WHEN event_kind (?:= '(\w+)'|IN \(([^)]*)\))\s+THEN (-?)token_amount`)

// blendNetSigns reads every signed token_amount arm out of q as
// kind -> +1/-1, failing on a kind signed both ways.
func blendNetSigns(t *testing.T, name, q string) map[string]int {
	t.Helper()
	signs := map[string]int{}
	for _, m := range blendNetArm.FindAllStringSubmatch(q, -1) {
		kinds := []string{m[1]}
		if m[1] == "" {
			kinds = strings.Split(strings.NewReplacer("'", "", " ", "").Replace(m[2]), ",")
		}
		sign := 1
		if m[3] == "-" {
			sign = -1
		}
		for _, k := range kinds {
			if prev, ok := signs[k]; ok && prev != sign {
				t.Errorf("%s signs %s both ways:\n%s", name, k, q)
			}
			signs[k] = sign
		}
	}
	return signs
}

var (
	blendSupplyLegSigns = map[string]int{"supply": 1, "supply_collateral": 1, "withdraw": -1, "withdraw_collateral": -1}
	blendBorrowLegSigns = map[string]int{"borrow": 1, "flash_loan": 1, "repay": -1}
	blendBothLegSigns   = map[string]int{
		"supply": 1, "supply_collateral": 1, "withdraw": -1, "withdraw_collateral": -1,
		"borrow": 1, "flash_loan": 1, "repay": -1,
	}
)

// assertBlendFold pins a fold's sign table and that every event_kind
// filter selecting the debt leg also selects flash_loan — a CASE arm
// the WHERE/FILTER never feeds is no arm at all.
func assertBlendFold(t *testing.T, name, q string, want map[string]int) {
	t.Helper()
	if got := blendNetSigns(t, name, q); !reflect.DeepEqual(got, want) {
		t.Errorf("%s net signs = %v, want %v:\n%s", name, got, want, q)
	}
	for _, m := range regexp.MustCompile(`event_kind IN \(([^)]*)\)`).FindAllStringSubmatch(q, -1) {
		if strings.Contains(m[1], "'repay'") && !strings.Contains(m[1], "'flash_loan'") {
			t.Errorf("%s filters the borrow leg without flash_loan (IN (%s)):\n%s", name, m[1], q)
		}
	}
}

// TestBlendFoldsCountFlashLoanAsDebt — a Blend flash_loan mints d-tokens
// that only a later repay burns (migration 0045 body: tokens_out,
// d_tokens_minted). A fold that drops the flash_loan but subtracts its
// repay serves flash_loan 1000 + repay 1000 as an open -1000 debt, and a
// kept flash loan as no debt at all. Every blend net fold must sign it
// as borrow does.
func TestBlendFoldsCountFlashLoanAsDebt(t *testing.T) {
	ctx := context.Background()
	empty := scriptedResult{cols: []string{"x"}}

	store, conn := newScriptedStore(t, empty)
	if _, err := store.BlendPositionsByUser(ctx, "GUSER"); err != nil {
		t.Fatalf("BlendPositionsByUser: %v", err)
	}
	assertBlendFold(t, "BlendPositionsByUser", conn.only(t).sql, blendBothLegSigns)

	store, conn = newScriptedStore(t, empty, empty)
	if _, err := store.blendPositionHolders(ctx); err != nil {
		t.Fatalf("blendPositionHolders: %v", err)
	}
	if len(conn.stmts) != 2 {
		t.Fatalf("blendPositionHolders issued %d statements, want 2", len(conn.stmts))
	}
	assertBlendFold(t, "blendPositionHolders supply", conn.stmts[0].sql, blendSupplyLegSigns)
	assertBlendFold(t, "blendPositionHolders borrow", conn.stmts[1].sql, blendBorrowLegSigns)

	kpis := scriptedResult{cols: []string{"users", "flash"}, rows: [][]driver.Value{{"1", "1"}}}
	store, conn = newScriptedStore(t, kpis, empty, empty, empty)
	if err := store.lendingPositionBlocks(ctx, &BespokeBlock{}, "30 days", 30); err != nil {
		t.Fatalf("lendingPositionBlocks: %v", err)
	}
	assertBlendFold(t, "Net position by asset", conn.stmts[1].sql, blendBothLegSigns)

	store, conn = newScriptedStore(t, empty)
	if _, err := store.ListBlendPools(ctx); err != nil {
		t.Fatalf("ListBlendPools: %v", err)
	}
	assertBlendFold(t, "ListBlendPools", conn.only(t).sql, blendBothLegSigns)
}

// TestBlendNetExprsCoverMigrationKinds keeps the two net expressions in
// lockstep with the blend_positions event_kind CHECK: every kind the
// table accepts moves exactly one leg.
func TestBlendNetExprsCoverMigrationKinds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "migrations", "0045_create_blend_money_market.up.sql"))
	if err != nil {
		t.Fatalf("read migration 0045: %v", err)
	}
	m := regexp.MustCompile(`(?s)event_kind\s+text\s+NOT NULL CHECK \(event_kind IN \((.*?)\)\)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("blend_positions event_kind CHECK not found in migration 0045")
	}
	want := map[string]bool{}
	for _, k := range strings.Split(strings.NewReplacer("'", "", " ", "", "\n", "").Replace(string(m[1])), ",") {
		want[k] = true
	}
	supply := blendNetSigns(t, "blendSupplyNetExpr", blendSupplyNetExpr)
	borrow := blendNetSigns(t, "blendBorrowNetExpr", blendBorrowNetExpr)
	got := map[string]bool{}
	for _, leg := range []map[string]int{supply, borrow} {
		for k := range leg {
			if got[k] {
				t.Errorf("event kind %s moves both legs", k)
			}
			got[k] = true
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("net expressions cover %v, migration 0045 accepts %v", got, want)
	}
}

// TestBlendNetFoldsUseSharedExprs fails when a blend_positions net is
// hand-rolled again instead of summing blendSupplyNetExpr /
// blendBorrowNetExpr — each copy is a place the flash_loan leg can drift.
func TestBlendNetFoldsUseSharedExprs(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "bespoke_lending.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "blend_positions") && blendNetArm.Match(src) {
			t.Errorf("%s hand-rolls a blend_positions net CASE; sum blendSupplyNetExpr / blendBorrowNetExpr instead", f)
		}
	}
}

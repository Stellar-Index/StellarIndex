// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
	"time"
)

func isHoldersProbe(q string) bool {
	return strings.Contains(q, "SELECT rank FROM stellar.asset_holders_rollup") && strings.Contains(q, "LIMIT 1")
}

func isHoldersRows(q string) bool {
	return strings.Contains(q, "stellar.asset_holders_rollup") && strings.Contains(q, "account_id, balance, computed_at")
}

func isHoldersCount(q string) bool {
	return strings.Contains(q, "stellar.asset_holders_counts")
}

// TestHoldersRollupSwapIsAtomic pins that the five live/staging swaps are one multi-pair EXCHANGE TABLES, so
// a crash between swaps cannot leave the board new while counts/stats/histograms hold the previous cycle.
func TestHoldersRollupSwapIsAtomic(t *testing.T) {
	// All five live tables that must swap as a group.
	wantPairs := []string{
		"stellar.asset_holders_rollup_staging AND stellar.asset_holders_rollup",
		"stellar.asset_holders_counts_staging AND stellar.asset_holders_counts",
		"stellar.accounts_stats_staging AND stellar.accounts_stats",
		"stellar.accounts_wealth_histogram_staging AND stellar.accounts_wealth_histogram",
		"stellar.accounts_trustline_histogram_staging AND stellar.accounts_trustline_histogram",
	}

	stmts := holdersRollupStatements(time.Now())

	// Exactly one statement may contain EXCHANGE TABLES — a single atomic swap.
	var exchangeStmts []string
	for _, s := range stmts {
		if strings.Contains(s, "EXCHANGE TABLES") {
			exchangeStmts = append(exchangeStmts, s)
		}
	}
	if len(exchangeStmts) != 1 {
		t.Fatalf("holdersRollupStatements has %d EXCHANGE TABLES statements, want exactly 1 atomic multi-pair swap (RA-2)", len(exchangeStmts))
	}

	// normalize whitespace so the multi-line SQL literal compares cleanly.
	swap := strings.Join(strings.Fields(exchangeStmts[0]), " ")
	for _, p := range wantPairs {
		if !strings.Contains(swap, p) {
			t.Errorf("atomic EXCHANGE is missing pair %q; swap=%q", p, swap)
		}
	}

	// The atomic swap must be the final statement: every staging arm has to be
	// filled before the group swap fires.
	last := stmts[len(stmts)-1]
	if !strings.Contains(last, "EXCHANGE TABLES") {
		t.Errorf("the atomic EXCHANGE must be the last statement, got %q", strings.Join(strings.Fields(last), " "))
	}
}

// TestHoldersRollupStatementsShareOneCycleStamp: every staging INSERT in a cycle must bake in the same
// computed_at, not each table's `DEFAULT now()`, or the consistency checks trip on every read of a healthy
// cycle.
func TestHoldersRollupStatementsShareOneCycleStamp(t *testing.T) {
	cycleAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	stamp := "toDateTime('" + cycleAt.Format(holdersRollupTimeLayout) + "', 'UTC')"

	stmts := holdersRollupStatements(cycleAt)
	checked := 0
	for _, s := range stmts {
		if !strings.Contains(s, "INSERT INTO") {
			continue
		}
		checked++
		if !strings.Contains(s, stamp) {
			t.Errorf("insert statement does not bake in the shared cycle stamp %s:\n%s", stamp, s)
		}
	}
	if checked == 0 {
		t.Fatal("no INSERT statement found — this test would pass vacuously")
	}
}

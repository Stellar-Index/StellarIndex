// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestHoldersRollupSwapIsAtomic pins RA-2: the five live↔staging swaps must be
// issued as ONE multi-pair EXCHANGE TABLES statement, not five sequential
// EXCHANGE calls. ClickHouse commits a multi-pair EXCHANGE as a single metadata
// transaction, so a crash/ctx-cancel/CH-restart between swaps cannot leave the
// board swapped-new while counts/stats/histograms hold the previous cycle's
// data — the half-swapped state holdersRollupBoard trusts as authoritative.
//
// Proven red against the pre-fix code (five separate EXCHANGE statements): the
// exactly-one assertion counts 5 and fails.
func TestHoldersRollupSwapIsAtomic(t *testing.T) {
	// All five live tables that must swap as a group.
	wantPairs := []string{
		"stellar.asset_holders_rollup_staging AND stellar.asset_holders_rollup",
		"stellar.asset_holders_counts_staging AND stellar.asset_holders_counts",
		"stellar.accounts_stats_staging AND stellar.accounts_stats",
		"stellar.accounts_wealth_histogram_staging AND stellar.accounts_wealth_histogram",
		"stellar.accounts_trustline_histogram_staging AND stellar.accounts_trustline_histogram",
	}

	// Exactly one statement may contain EXCHANGE TABLES — a single atomic swap.
	var exchangeStmts []string
	for _, s := range holdersRollupStatements {
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
	last := holdersRollupStatements[len(holdersRollupStatements)-1]
	if !strings.Contains(last, "EXCHANGE TABLES") {
		t.Errorf("the atomic EXCHANGE must be the last statement, got %q", strings.Join(strings.Fields(last), " "))
	}
}

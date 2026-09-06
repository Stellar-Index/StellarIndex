// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestCreatorsRollupStatsDeriveFromTheBoard pins the property that makes
// the served coverage span honest by construction: the stats arm — which
// carries from_ledger/thru_ledger — must aggregate the STAGING BOARD the
// same cycle just wrote, never re-read stellar.account_movements.
//
// If the span came from a second scan of the archive it could describe a
// different set of rows than the board it qualifies, and the endpoint
// would report a coverage span its own numbers do not back. Deriving it
// from the board makes that divergence unrepresentable.
func TestCreatorsRollupStatsDeriveFromTheBoard(t *testing.T) {
	stats := creatorsRollupStatement(t, "account_creators_stats_staging")

	for _, metric := range []string{"from_ledger", "thru_ledger", "from_time", "thru_time", "creations_total"} {
		if !strings.Contains(stats, metric) {
			t.Errorf("stats statement is missing the %q metric", metric)
		}
	}
	if !strings.Contains(stats, "FROM stellar.account_creators_rollup_staging") {
		t.Error("stats statement must aggregate the staging board written by this same cycle")
	}
	if strings.Contains(stats, "stellar.account_movements") {
		t.Error("stats statement re-reads the movement archive; the span would then " +
			"describe a different row set than the board it qualifies")
	}
	// The span must be min/max over the board's own ledger columns, not a
	// literal.
	if !strings.Contains(stats, "min(first_ledger)") || !strings.Contains(stats, "max(last_ledger)") {
		t.Error("coverage span must be derived (min/max over the board), not asserted")
	}
}

// TestCreatorsRollupSwapIsAtomic: the board and the span that qualifies
// it must swap in ONE metadata transaction. A board swapped new beside
// the previous cycle's span is exactly the overstatement this surface
// exists to avoid, and AccountCreators would serve it as authoritative.
func TestCreatorsRollupSwapIsAtomic(t *testing.T) {
	var exchanges []string
	for _, step := range creatorsRollupStatements {
		if strings.Contains(step.sql, "EXCHANGE TABLES") {
			exchanges = append(exchanges, step.sql)
		}
	}
	if len(exchanges) != 1 {
		t.Fatalf("found %d EXCHANGE statements, want exactly 1 multi-pair swap", len(exchanges))
	}
	for _, pair := range []string{
		"stellar.account_creators_rollup_staging AND stellar.account_creators_rollup",
		"stellar.account_creators_stats_staging AND stellar.account_creators_stats",
	} {
		if !strings.Contains(exchanges[0], pair) {
			t.Errorf("the single EXCHANGE is missing the pair %q", pair)
		}
	}
	if exchanges[0] != creatorsRollupStatements[len(creatorsRollupStatements)-1].sql {
		t.Error("the EXCHANGE must be the last statement, after every staging arm is filled")
	}
	// The working table is not served and must never be swapped.
	if strings.Contains(exchanges[0], "account_creators_ops") {
		t.Error("the working table is not a served table and must not be exchanged")
	}
}

// TestCreatorsRollupDedupesTheArchive: stellar.account_movements is a
// ReplacingMergeTree, so un-merged duplicate parts are normal. Counting
// rows straight out of it would inflate accounts_created — a silently
// wrong league table. The walked pass that lands the working table must
// collapse duplicates over the table's full ORDER BY key.
//
// Grouping per window loses nothing: the partition expression is a
// function of `ledger`, which is itself part of that ORDER BY key, so
// every row of a duplicate group lands in the same window.
func TestCreatorsRollupDedupesTheArchive(t *testing.T) {
	board := creatorsRollupStatement(t, "account_creators_ops")

	if !strings.Contains(board, "argMax(") {
		t.Error("board arm must collapse ReplacingMergeTree duplicates (argMax over ingested_at)")
	}
	if !strings.Contains(board, "GROUP BY address, ledger, tx_hash, op_index, leg_index, direction") {
		t.Error("the dedupe must group by account_movements' full ORDER BY key; " +
			"a narrower key would drop real creations, a wider one would keep duplicates")
	}
	// Only the funder arm is a creation record; counting both directions
	// would double every creator's total.
	if !strings.Contains(board, "direction = 'sent'") {
		t.Error("board arm must read only the funder (sent) arm of the movement pair")
	}
	if !strings.Contains(board, "movement_kind = 'create_account'") {
		t.Error("board arm must filter to create_account movements")
	}
}

// TestCreatorsRollupScansMovementsOnce: stellar.account_movements is the
// expensive table — 10,309,271,697 rows / 583.54 GiB on r1 2026-09-06.
// Exactly one statement may touch it, the walked one that lands the
// working table; every other figure derives from those rows, which is
// what makes the board and its coverage span describe the same data.
func TestCreatorsRollupScansMovementsOnce(t *testing.T) {
	var touching []int
	for i, step := range creatorsRollupStatements {
		if strings.Contains(step.sql, "stellar.account_movements") {
			touching = append(touching, i+1)
		}
	}
	if len(touching) != 1 {
		t.Fatalf("statements touching stellar.account_movements: %v, want exactly 1", touching)
	}
	step := creatorsRollupStatements[touching[0]-1]
	if !step.walk {
		t.Error("the pass over stellar.account_movements must be walked per ledger window")
	}
	if !strings.Contains(step.sql, "WHERE ledger BETWEEN ? AND ?") {
		t.Error("the walked pass must bound ledger to the window, which is what prunes " +
			"stellar.account_movements to one partition")
	}
	if !strings.HasPrefix(step.sql, "INSERT INTO stellar.account_creators_ops") {
		t.Error("the single pass over the archive must be the one filling the working table")
	}
}

// TestCreatorsRollupJoinsOutsideTheWalk pins the placement that the
// walk alone would not have fixed.
//
// The board's LEFT JOIN builds one row per account that currently exists
// — 10,928,611 rows at 3.18 GiB measured on r1 2026-09-06 — and that
// build side does not shrink when the movement scan is partitioned. A
// join left inside the walked step would therefore rebuild the same hash
// table on every one of the 65 windows and re-read the whole account
// entry range each time, while leaving the cycle's largest single memory
// consumer un-walked. It belongs in the once-per-cycle step that reads
// the working table.
func TestCreatorsRollupJoinsOutsideTheWalk(t *testing.T) {
	for i, step := range creatorsRollupStatements {
		if step.walk && strings.Contains(step.sql, "JOIN") {
			t.Errorf("walked step %d carries a JOIN; its build side is the account "+
				"population and would be paid once per window:\n%s", i+1, step.sql)
		}
	}
	board := creatorsRollupStatement(t, "account_creators_rollup_staging")
	if !strings.Contains(board, "LEFT JOIN") {
		t.Fatal("the board no longer joins the live account entries; live_accounts " +
			"and live_stroops would stop describing the created set")
	}
	if !strings.Contains(board, "FROM stellar.account_creators_ops AS c") {
		t.Error("the board must join the working table the walk wrote, not re-scan the archive")
	}
	// The build side is pinned to the account-entry population so the
	// cycle's peak cannot silently switch to the creation population,
	// which grows with chain history instead.
	if !strings.Contains(board, "query_plan_join_swap_table = 0") {
		t.Error("the board's join build side is unpinned; which population sets the " +
			"cycle's peak would then be a planner estimate")
	}
}

// clampLedger guards the stats column's Int64 against values a ledger
// sequence can never hold. Returning 0 routes them into the "warming"
// branch instead of onto the wire as a coverage claim.
func TestClampLedger(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int64
		want uint32
	}{
		{"a real ledger", 64184370, 64184370},
		{"the max uint32", 4294967295, 4294967295},
		{"empty rollup reads as no span", 0, 0},
		{"negative is not a ledger", -1, 0},
		{"beyond uint32 is not a ledger", 4294967296, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampLedger(tc.in); got != tc.want {
				t.Errorf("clampLedger(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// creatorsRollupStatement returns the single INSERT that fills the named
// staging table, failing if the cycle does not fill it exactly once.
func creatorsRollupStatement(t *testing.T, table string) string {
	t.Helper()
	var found []string
	for _, step := range creatorsRollupStatements {
		if strings.HasPrefix(step.sql, "INSERT INTO stellar."+table) {
			found = append(found, step.sql)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d INSERTs into %s, want exactly 1", len(found), table)
	}
	return found[0]
}

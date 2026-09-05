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
	for _, stmt := range creatorsRollupStatements {
		if strings.Contains(stmt, "EXCHANGE TABLES") {
			exchanges = append(exchanges, stmt)
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
	if exchanges[0] != creatorsRollupStatements[len(creatorsRollupStatements)-1] {
		t.Error("the EXCHANGE must be the last statement, after every staging arm is filled")
	}
}

// TestCreatorsRollupDedupesTheArchive: stellar.account_movements is a
// ReplacingMergeTree, so un-merged duplicate parts are normal. Counting
// rows straight out of it would inflate accounts_created — a silently
// wrong league table. The board arm must collapse duplicates over the
// table's full ORDER BY key.
func TestCreatorsRollupDedupesTheArchive(t *testing.T) {
	board := creatorsRollupStatement(t, "account_creators_rollup_staging")

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
	for _, stmt := range creatorsRollupStatements {
		if strings.HasPrefix(stmt, "INSERT INTO stellar."+table) {
			found = append(found, stmt)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d INSERTs into %s, want exactly 1", len(found), table)
	}
	return found[0]
}

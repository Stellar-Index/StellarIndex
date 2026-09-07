// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestSponsorsRollupReadsNoOperationBody is the cost guard AND the
// correctness note behind this rollup's whole shape.
//
// The sponsored account could be read out of a Begin operation's
// body_xdr. It is not, because body_xdr is stellar.operations' wide
// column and ClickHouse reads it a granule at a time: measured on r1
// over ledgers 64,000,000-64,277,243, including it cost 61.06 GiB
// against 12.05 GiB without. The sponsored identity is instead taken
// from the End operation's source_account, which the XDR guarantees is
// the sponsored account and which was verified two ways on r1 (SDK
// decode of sampled bodies, and an identical board over the whole
// window computed both ways).
//
// A future edit that reaches for body_xdr would silently multiply the
// cycle's cost, so it fails here instead.
func TestSponsorsRollupReadsNoOperationBody(t *testing.T) {
	for i, step := range sponsorsRollupStatements {
		if strings.Contains(step.sql, "body_xdr") {
			t.Errorf("statement %d reads body_xdr; the sponsored account comes from the "+
				"End operation's source_account precisely so it does not have to", i+1)
		}
		if strings.Contains(step.sql, "base64Decode") {
			t.Errorf("statement %d base64-decodes an operation body; see the comment above", i+1)
		}
	}
}

// TestSponsorsRollupScansOperationsOnce: stellar.operations is the
// expensive table. Exactly one statement may touch it — the one that
// lands the narrow working projection. Everything else derives from
// that projection, which is what makes the board and its coverage span
// describe the same rows by construction.
func TestSponsorsRollupScansOperationsOnce(t *testing.T) {
	var touching []int
	for i, step := range sponsorsRollupStatements {
		if strings.Contains(step.sql, "stellar.operations") {
			touching = append(touching, i+1)
		}
	}
	if len(touching) != 1 {
		t.Fatalf("statements touching stellar.operations: %v, want exactly 1", touching)
	}
	step := sponsorsRollupStatements[touching[0]-1]
	stmt := step.sql
	if !strings.Contains(stmt, "INSERT INTO stellar.account_sponsors_ops") {
		t.Error("the single pass over stellar.operations must be the one filling the working table")
	}
	// The pass is walked one lake partition at a time. Unwalked it is a
	// single statement over a 24.74-billion-row, 2.18 TiB table whose
	// dedupe state count grows with the whole archive.
	if !step.walk {
		t.Error("the pass over stellar.operations must be walked per ledger window")
	}
	if !strings.Contains(stmt, "WHERE ledger_seq BETWEEN ? AND ?") {
		t.Error("the walked pass must bound ledger_seq to the window, which is what " +
			"prunes stellar.operations to one partition")
	}
	// ReplacingMergeTree duplicates must be collapsed over the table's
	// full ORDER BY key, not trusted away.
	if !strings.Contains(stmt, "argMax(") {
		t.Error("the operations pass must collapse ReplacingMergeTree duplicates")
	}
	if !strings.Contains(stmt, "GROUP BY o.ledger_seq, o.tx_index, o.op_index") {
		t.Error("dedupe must group by stellar.operations' full ORDER BY key")
	}
	for _, op := range []string{opBeginSponsoring, opEndSponsoring, opRevokeSponsoring} {
		if !strings.Contains(stmt, op) {
			t.Errorf("the operations pass does not select %s", op)
		}
	}
}

// TestSponsorsRollupCountsOnlyAppliedOperations is the guard for #494: a
// served league table that counted sponsorship arrangements which never
// took effect.
//
// stellar.operations carries no success flag and never will — the lake
// stores what the ledger CONTAINED, so extractOps keeps the operations of
// failed transactions by design. Measured on r1 2026-09-07, 2,426,813 of
// the archive's 22,413,991 sponsorship operations (10.8%) sit in failed
// transactions. Ungated the board served 11,162,397 sponsorships_started
// and 87,193 revocations_issued against true figures of 9,972,887 and
// 39,492; 323 of its 2,740 ranked accounts had every operation they were
// credited with inside a failed transaction, one was credited with 27,528
// revocations against a real 63, and 2,408 of the 2,417 rows that survive
// the correction were ranked in the wrong place.
//
// The sibling cycle gates by pairing the operation with an APPLIED EFFECT
// (a CAP-67 transfer movement, which a failed transaction cannot
// produce). That route does not exist here: under CAP-33 the
// is-sponsoring-future-reserves-for relationship lives only for the
// duration of the transaction and is written to no ledger entry, so 0 of
// 4,745 Begin and End operations over ledgers 63,000,000-63,010,000 have
// any stellar.ledger_entry_changes row at their own operation identity,
// the 4,294 that applied included. The transaction's own success flag is
// the only evidence of application there is.
//
// Three properties make that gate sound, and each is pinned here because
// dropping any one of them puts wrong numbers on a served board rather
// than an error in a log:
//
//   - the join key is the FULL transaction identity, which is also
//     stellar.transactions' whole ORDER BY key. A looser key would gate an
//     operation on some other transaction's outcome.
//   - the flag is resolved with argMax over the version column, not read
//     off whichever duplicate row the scan reached first. Duplicates are
//     not hypothetical: over ledgers 63,000,000-63,099,999 every one of
//     that range's 33,380,486 distinct (ledger_seq, tx_index) keys carries
//     more than one row.
//   - the build side is PINNED. The window's transactions outnumber its
//     sponsorship operations by roughly 460 to 1 (519,663,457 against
//     1,129,122 in partition 63), so a planner estimate that swapped them
//     would build a hash table over half a billion rows in a step that
//     measures 1.28 GiB pinned.
func TestSponsorsRollupCountsOnlyAppliedOperations(t *testing.T) {
	stmt := sponsorsOperationsStatement(t)

	if !strings.Contains(stmt, "stellar.transactions") {
		t.Fatalf("the pass over stellar.operations does not join stellar.transactions, so it "+
			"counts sponsorship operations from FAILED transactions — arrangements that "+
			"never took effect, ranked as though they had:\n%s", stmt)
	}
	if !strings.Contains(stmt, "HAVING argMax(t.successful, t.ingested_at) = 1") {
		t.Error("the success flag must be resolved with argMax over ingested_at, the version " +
			"column, so an un-merged duplicate row cannot decide whether an arrangement counts")
	}
	if strings.Contains(stmt, "t.successful = 1") {
		t.Error("reading `successful` off a raw row trusts ReplacingMergeTree dedupe that has " +
			"not happened; resolve it with argMax over ingested_at instead")
	}
	if !strings.Contains(stmt, "ON t.ledger_seq = o.ledger_seq AND t.tx_index = o.tx_index") {
		t.Error("the gate's join key must be the full (ledger_seq, tx_index) transaction " +
			"identity, or an operation is gated on another transaction's outcome")
	}
	if !strings.Contains(stmt, "WHERE t.ledger_seq BETWEEN ? AND ?") {
		t.Error("the transactions side must carry its own window predicate; a join condition " +
			"prunes neither side's partitions, so without it every window rescans the archive")
	}
	if !strings.Contains(stmt, "query_plan_join_swap_table = 0") {
		t.Error("the build side must be pinned to the window's sponsorship operations; " +
			"unpinned, a planner estimate can build the hash table over the transactions")
	}
	// The projection's own columns still come from the operation. The
	// transaction contributes the verdict and nothing else — taking a
	// sponsor identity from it would attribute every arrangement in a
	// transaction to that transaction's fee payer.
	for _, col := range []string{"t.source_account", "t.fee_charged", "t.memo"} {
		if strings.Contains(stmt, col) {
			t.Errorf("the transactions side selects %q; it may contribute the success verdict "+
				"only:\n%s", col, stmt)
		}
	}
}

// sponsorsOperationsStatement returns the cycle's single pass over
// stellar.operations.
func sponsorsOperationsStatement(t *testing.T) string {
	t.Helper()
	var found []string
	for _, step := range sponsorsRollupStatements {
		if strings.Contains(step.sql, "stellar.operations") {
			found = append(found, step.sql)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d statements touching stellar.operations, want exactly 1", len(found))
	}
	return found[0]
}

// TestSponsorsRollupExcludesAmbiguousAttribution: attribution assumes a
// transaction has ONE sponsor. Transactions with more must be excluded
// and counted, never folded into whichever sponsor sorted first.
func TestSponsorsRollupExcludesAmbiguousAttribution(t *testing.T) {
	board := sponsorsRollupStatement(t, "account_sponsors_rollup_staging")
	if !strings.Contains(board, "n_sponsors = 1") {
		t.Error("board must attribute only transactions with a single distinct sponsor")
	}
	stats := sponsorsRollupStatement(t, "account_sponsors_stats_staging")
	if !strings.Contains(stats, "ambiguous_txs") || !strings.Contains(stats, "n_sponsors > 1") {
		t.Error("the excluded multi-sponsor transactions must be counted into stats, " +
			"so the exclusion is published rather than silent")
	}
}

// TestSponsorsRollupSpanDerivesFromTheScannedRows: the coverage span
// must come from the working table this cycle wrote, so it cannot
// describe a different row set than the board it qualifies.
func TestSponsorsRollupSpanDerivesFromTheScannedRows(t *testing.T) {
	stats := sponsorsRollupStatement(t, "account_sponsors_stats_staging")
	for _, want := range []string{
		"'from_ledger', toInt64(min(lseq)) FROM stellar.account_sponsors_ops",
		"'thru_ledger', toInt64(max(lseq)) FROM stellar.account_sponsors_ops",
	} {
		if !strings.Contains(stats, want) {
			t.Errorf("coverage span must be derived from the scanned rows; missing: %s", want)
		}
	}
}

// TestSponsorsRollupSwapIsAtomic: board and span swap together or not
// at all.
func TestSponsorsRollupSwapIsAtomic(t *testing.T) {
	var exchanges []string
	for _, step := range sponsorsRollupStatements {
		if strings.Contains(step.sql, "EXCHANGE TABLES") {
			exchanges = append(exchanges, step.sql)
		}
	}
	if len(exchanges) != 1 {
		t.Fatalf("found %d EXCHANGE statements, want exactly 1", len(exchanges))
	}
	for _, pair := range []string{
		"stellar.account_sponsors_rollup_staging AND stellar.account_sponsors_rollup",
		"stellar.account_sponsors_stats_staging AND stellar.account_sponsors_stats",
	} {
		if !strings.Contains(exchanges[0], pair) {
			t.Errorf("the single EXCHANGE is missing the pair %q", pair)
		}
	}
	if exchanges[0] != sponsorsRollupStatements[len(sponsorsRollupStatements)-1].sql {
		t.Error("the EXCHANGE must be the last statement")
	}
	// The working table is not served and must never be swapped.
	if strings.Contains(exchanges[0], "account_sponsors_ops") {
		t.Error("the working table is not a served table and must not be exchanged")
	}
}

func sponsorsRollupStatement(t *testing.T, table string) string {
	t.Helper()
	var found []string
	for _, step := range sponsorsRollupStatements {
		if strings.HasPrefix(step.sql, "INSERT INTO stellar."+table) {
			found = append(found, step.sql)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d INSERTs into %s, want exactly 1", len(found), table)
	}
	return found[0]
}

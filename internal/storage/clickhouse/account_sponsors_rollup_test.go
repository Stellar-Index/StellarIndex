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
	for i, stmt := range sponsorsRollupStatements {
		if strings.Contains(stmt, "body_xdr") {
			t.Errorf("statement %d reads body_xdr; the sponsored account comes from the "+
				"End operation's source_account precisely so it does not have to", i+1)
		}
		if strings.Contains(stmt, "base64Decode") {
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
	for i, stmt := range sponsorsRollupStatements {
		if strings.Contains(stmt, "stellar.operations") {
			touching = append(touching, i+1)
		}
	}
	if len(touching) != 1 {
		t.Fatalf("statements touching stellar.operations: %v, want exactly 1", touching)
	}
	stmt := sponsorsRollupStatements[touching[0]-1]
	if !strings.Contains(stmt, "INSERT INTO stellar.account_sponsors_ops") {
		t.Error("the single pass over stellar.operations must be the one filling the working table")
	}
	// ReplacingMergeTree duplicates must be collapsed over the table's
	// full ORDER BY key, not trusted away.
	if !strings.Contains(stmt, "argMax(") {
		t.Error("the operations pass must collapse ReplacingMergeTree duplicates")
	}
	if !strings.Contains(stmt, "GROUP BY ledger_seq, tx_index, op_index") {
		t.Error("dedupe must group by stellar.operations' full ORDER BY key")
	}
	for _, op := range []string{opBeginSponsoring, opEndSponsoring, opRevokeSponsoring} {
		if !strings.Contains(stmt, op) {
			t.Errorf("the operations pass does not select %s", op)
		}
	}
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
	for _, stmt := range sponsorsRollupStatements {
		if strings.Contains(stmt, "EXCHANGE TABLES") {
			exchanges = append(exchanges, stmt)
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
	if exchanges[0] != sponsorsRollupStatements[len(sponsorsRollupStatements)-1] {
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
	for _, stmt := range sponsorsRollupStatements {
		if strings.HasPrefix(stmt, "INSERT INTO stellar."+table) {
			found = append(found, stmt)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d INSERTs into %s, want exactly 1", len(found), table)
	}
	return found[0]
}

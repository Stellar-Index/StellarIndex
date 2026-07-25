package clickhouse

import (
	"strings"
	"testing"
)

// Regression tests for audit DAT-10 (ClickHouse ReplacingMergeTree reads that
// neither FINAL nor dedup, over-counting un-merged duplicate rows).
//
// StreamEntryChanges and CountOpScopedEntryChanges are free functions that
// dial a real ClickHouse connection via openRead (not an injectable
// r.conn field), so they cannot be driven end-to-end with the stubConn
// harness in this package without a live server. The queries were extracted
// to package-level consts specifically so their SQL text — the artifact the
// fix actually changes — stays independently, deterministically testable
// (the same pattern account_movements.go's cbLookupCreatesQuery already
// uses).

func TestStreamEntryChangesQuery_UsesFinal(t *testing.T) {
	if !strings.Contains(streamEntryChangesQuery, "stellar.ledger_entry_changes FINAL") {
		t.Fatalf("streamEntryChangesQuery = %q, want `stellar.ledger_entry_changes FINAL`", streamEntryChangesQuery)
	}
	// The per-op grouping order callers rely on (see StreamEntryChanges' doc
	// comment) must survive the fix unchanged.
	if !strings.Contains(streamEntryChangesQuery, "ORDER BY ledger_seq, tx_hash, op_index, change_index") {
		t.Fatalf("streamEntryChangesQuery = %q, want the original per-op ORDER BY preserved", streamEntryChangesQuery)
	}
}

func TestCountOpScopedEntryChangesQuery_UsesUniqExact(t *testing.T) {
	if !strings.Contains(countOpScopedEntryChangesQuery, "uniqExact(ledger_seq, tx_hash, op_index, change_index)") {
		t.Fatalf("countOpScopedEntryChangesQuery = %q, want a uniqExact(...) primary-key dedup, not count()", countOpScopedEntryChangesQuery)
	}
	if strings.Contains(countOpScopedEntryChangesQuery, "SELECT count()") {
		t.Fatalf("countOpScopedEntryChangesQuery = %q, must not use bare count() (over-counts un-merged duplicate parts)", countOpScopedEntryChangesQuery)
	}
}

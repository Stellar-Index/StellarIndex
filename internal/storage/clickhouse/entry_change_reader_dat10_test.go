package clickhouse

import (
	"strings"
	"testing"
)

// Tests for ReplacingMergeTree reads: a read with neither FINAL nor a dedup over-counts un-merged duplicates.
// StreamEntryChanges and CountOpScopedEntryChanges dial a real connection via openRead, so stubConn cannot
// drive them; their queries are package-level consts so the SQL text is testable.

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

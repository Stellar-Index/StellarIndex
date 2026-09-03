package clickhouse

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// BackfillContractActiveLedgers and BackfillContractInstanceChanges dial their
// own connection via openRead, so — like BackfillTxHashIndex — they cannot be
// driven end-to-end by the stubConn harness. Their per-window INSERT…SELECT is
// a package-level const precisely so its text stays testable; the walk that
// drives it is covered in ledger_window_test.go.

// readDeployDDL returns one deploy/clickhouse artifact as text.
func readDeployDDL(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "clickhouse", name))
	if err != nil {
		t.Fatalf("read deploy/clickhouse/%s: %v", name, err)
	}
	return string(raw)
}

// stripSQLComments drops `--` comment lines, so a lockstep comparison reads
// only executable DDL — the artifacts carry long runbook comments that
// legitimately mention expressions in prose.
func stripSQLComments(ddl string) string {
	var b strings.Builder
	for _, line := range strings.Split(ddl, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestContractActiveLedgersBackfillQuery_Shape pins the two things this
// INSERT…SELECT gets wrong most easily.
//
// GROUP BY, not DISTINCT: they compute the same set, but DISTINCT cannot use
// external aggregation, so it CANNOT spill — a dense 5M-ledger window's
// distinct set blew the 8 GiB limit on the first r1 run (Code 241 at window
// [40M,45M]). GROUP BY plus max_bytes_before_external_group_by spills to disk
// and the window merely gets slower.
//
// The window predicate is what bounds each iteration's work at all; losing it
// turns a resumable job into one unbounded scan of contract_events.
func TestContractActiveLedgersBackfillQuery_Shape(t *testing.T) {
	q := contractActiveLedgersBackfillQuery

	if !strings.Contains(q, "GROUP BY contract_id, ledger_seq") {
		t.Errorf("lost its GROUP BY collapse:\n%s", q)
	}
	if strings.Contains(q, "DISTINCT") {
		t.Errorf("reverted to DISTINCT — it cannot spill, and this window OOMed at 8 GiB "+
			"on r1 (Code 241, window [40M,45M]):\n%s", q)
	}
	if !strings.Contains(q, "max_bytes_before_external_group_by") {
		t.Errorf("lost the external-group-by spill that makes growth cost time, not failures:\n%s", q)
	}
	for _, s := range []string{
		"max_threads = 4",
		"max_memory_usage = 8589934592",
		"max_execution_time = 1800",
		"WHERE ledger_seq BETWEEN ? AND ?",
		"INSERT INTO stellar.contract_active_ledgers (contract_id, ledger_seq, close_time)",
		"FROM stellar.contract_events",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("contractActiveLedgersBackfillQuery missing %q:\n%s", s, q)
		}
	}
	// Two placeholders, matching the (lo, hi) the walk binds.
	if got := strings.Count(q, "?"); got != 2 {
		t.Fatalf("has %d placeholders, but the walk binds 2 (lo, hi):\n%s", got, q)
	}
	// The index deliberately carries NO counts, which is what makes an
	// overlapping re-run collapse cleanly in the ReplacingMergeTree instead of
	// double-counting (the migration-0059 class).
	if strings.Contains(q, "count(") || strings.Contains(q, "sum(") {
		t.Errorf("the activity index gained an aggregate count — a re-run would then "+
			"double-count rather than collapse:\n%s", q)
	}
}

// TestContractInstanceBackfillQuery_MatchesTheMaterializedView is the lockstep
// assertion the Go doc comment asks for in prose: the backfill and
// contract_instance_changes_mv must extract the SAME fixed offsets from the
// SAME wire layout, because they fill ONE table — the MV live-forward, the
// backfill behind it. A drifted offset in either does not fail; it writes
// wrong contract hashes or wrong wasm hashes into the explorer's code-history
// timeline, stamped as complete.
//
// Every expression below is byte-offset-bearing, so an off-by-one is a silent
// data corruption rather than an error.
func TestContractInstanceBackfillQuery_MatchesTheMaterializedView(t *testing.T) {
	q := contractInstanceBackfillQuery
	mv := stripSQLComments(readDeployDDL(t, "contract_instance_changes.sql"))

	shared := []string{
		// Projection: contract id, then the executable verdict.
		"lower(hex(substring(tryBase64Decode(key_xdr), 9, 32)))",
		"toUInt8(substring(tryBase64Decode(entry_xdr), 61, 4) = unhex('00000001'))",
		"lower(hex(substring(tryBase64Decode(entry_xdr), 65, 32)))",
		// Source predicate: contract_data instance entries only.
		"entry_type = 'contract_data'",
		"length(key_xdr) = 64",
		"substring(tryBase64Decode(key_xdr), 1, 8) = unhex('0000000600000001')",
		"substring(tryBase64Decode(key_xdr), 41, 4) = unhex('00000014')",
		"entry_xdr != ''",
		"substring(tryBase64Decode(entry_xdr), 57, 4) = unhex('00000013')",
		"FROM stellar.ledger_entry_changes",
	}
	for _, expr := range shared {
		if !strings.Contains(q, expr) {
			t.Errorf("contractInstanceBackfillQuery missing %q — it has drifted from the MV:\n%s", expr, q)
		}
		if !strings.Contains(mv, expr) {
			t.Errorf("deploy/clickhouse/contract_instance_changes.sql missing %q — the MV has "+
				"drifted from the backfill", expr)
		}
	}

	// tryBase64Decode, never the throwing form: inside the MV a decode throw
	// fails the whole source INSERT into ledger_entry_changes, i.e. blocks
	// ingest. The backfill mirrors it so the two agree on malformed rows
	// (skip) rather than one skipping and the other aborting the window.
	for name, sql := range map[string]string{"backfill": q, "materialized view": mv} {
		for _, line := range strings.Split(sql, "\n") {
			if strings.Contains(line, "base64Decode(") && !strings.Contains(line, "tryBase64Decode(") {
				t.Errorf("%s uses the THROWING base64Decode: %q", name, strings.TrimSpace(line))
			}
		}
	}

	// The backfill's own bounds — the MV needs none (it runs per insert).
	for _, s := range []string{
		"WHERE ledger_seq BETWEEN ? AND ?",
		"max_threads = 4",
		"max_memory_usage = 8589934592",
		"max_execution_time = 1800",
		"INSERT INTO stellar.contract_instance_changes",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("contractInstanceBackfillQuery missing %q:\n%s", s, q)
		}
	}
	if got := strings.Count(q, "?"); got != 2 {
		t.Fatalf("has %d placeholders, but the walk binds 2 (lo, hi):\n%s", got, q)
	}

	// Column order is load-bearing: this is an INSERT…SELECT, so the SELECT
	// list is matched to the column list POSITIONALLY. is_sac and wasm_hash
	// swapping would write a hex hash into a UInt8 flag.
	const wantCols = "(contract_hash, ledger_seq, change_index, close_time, is_sac, wasm_hash)"
	if !strings.Contains(strings.Join(strings.Fields(q), " "), wantCols) {
		t.Errorf("contractInstanceBackfillQuery column list is not %s:\n%s", wantCols, q)
	}
}

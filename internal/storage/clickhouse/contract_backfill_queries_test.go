package clickhouse

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// BackfillContractActiveLedgers and BackfillContractInstanceChanges dial their own connection via openRead,
// so the stubConn harness cannot drive them; their per-window INSERT...SELECT is package-level so its text
// stays testable.

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

// TestContractActiveLedgersBackfillQuery_Shape: the INSERT...SELECT must use GROUP BY, not DISTINCT, which
// cannot spill and exceeds the memory limit on dense windows. It must keep the window predicate, or the
// resumable job becomes one unbounded scan.
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

// TestContractInstanceBackfillQuery_MatchesTheMaterializedView pins lockstep: the backfill and
// contract_instance_changes_mv fill ONE table, so they must extract the SAME byte offsets. A drift
// silently writes wrong contract or wasm hashes into the code-history timeline, stamped complete.
func TestContractInstanceBackfillQuery_MatchesTheMaterializedView(t *testing.T) {
	q := contractInstanceBackfillQuery(ContractInstanceChangesTable)
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

	// tryBase64Decode, never the throwing form: a decode throw inside the MV fails the whole source INSERT and
	// blocks ingest. The backfill mirrors it so both skip malformed rows rather than one skipping and one
	// aborting.
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

	// FINAL on the RMT source: without it a corrected re-ingest's stale part
	// rides the same INSERT as the fix and ties it on the target's now()
	// version, so code-history can serve the stale wasm_hash.
	if !strings.Contains(q, "FROM stellar.ledger_entry_changes FINAL\n\tWHERE ledger_seq BETWEEN ? AND ?") {
		t.Errorf("contractInstanceBackfillQuery must read `stellar.ledger_entry_changes FINAL` bounded by the window predicate:\n%s", q)
	}

	// Column order is load-bearing: this is an INSERT…SELECT, so the SELECT
	// list is matched to the column list POSITIONALLY. is_sac and wasm_hash
	// swapping would write a hex hash into a UInt8 flag.
	const wantCols = "(contract_hash, ledger_seq, tx_hash, change_index, intra_ledger_seq, close_time, is_sac, wasm_hash)"
	if !strings.Contains(strings.Join(strings.Fields(q), " "), wantCols) {
		t.Errorf("contractInstanceBackfillQuery column list is not %s:\n%s", wantCols, q)
	}
}

// TestContractInstanceBackfillQuery_TargetsOnlyKnownTables: the target name is
// formatted into SQL, so anything but the two timeline tables is refused
// before a connection is opened.
func TestContractInstanceBackfillQuery_TargetsOnlyKnownTables(t *testing.T) {
	const unreachable = "127.0.0.1:1"
	noLog := func(string, ...any) {}
	err := BackfillContractInstanceChangesInto(context.Background(), unreachable,
		"contract_instance_changes; DROP TABLE stellar.ledgers", 2, 10, 5, noLog)
	if err == nil || !strings.Contains(err.Error(), "is not contract_instance_changes") {
		t.Fatalf("unknown target: err = %v, want the allow-list refusal", err)
	}
	for _, table := range []string{ContractInstanceChangesTable, ContractInstanceChangesV2Table} {
		if q := contractInstanceBackfillQuery(table); !strings.Contains(q, "INSERT INTO stellar."+table+"\n") {
			t.Errorf("backfill into %s targets the wrong table:\n%s", table, q)
		}
	}
}

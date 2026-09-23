package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// createStmt returns the whitespace-collapsed CREATE statement for name in
// ddl, inline `--` comments stripped, or "" when absent. name must be
// followed by a non-identifier byte, so contract_instance_changes does not
// match contract_instance_changes_mv.
func createStmt(ddl, kind, name string) string {
	head := "CREATE " + kind + " IF NOT EXISTS " + name
	for from := 0; ; {
		i := strings.Index(ddl[from:], head)
		if i < 0 {
			return ""
		}
		start := from + i
		end := start + len(head)
		if end < len(ddl) && (ddl[end] == '_' || ddl[end] >= 'a' && ddl[end] <= 'z') {
			from = end
			continue
		}
		stmt := ddl[start:]
		stmt = stmt[:strings.Index(stmt, ";")]
		var b strings.Builder
		for _, line := range strings.Split(stmt, "\n") {
			if j := strings.Index(line, "--"); j >= 0 {
				line = line[:j]
			}
			b.WriteString(line)
			b.WriteByte(' ')
		}
		return strings.Join(strings.Fields(b.String()), " ")
	}
}

// TestContractInstanceChanges_KeyIsTransactionScoped pins T356/T377.
// change_index restarts at 0 on every TRANSACTION (extractLedgerEntryChanges),
// so under ORDER BY (contract_hash, ledger_seq, change_index) two
// transactions writing one contract's instance in the same ledger share a key
// and the ReplacingMergeTree keeps one of them. tx_hash makes the key the
// change's identity; intra_ledger_seq must be projected for the read order.
// Both the operator mirror and the fresh-host tier1 copy are checked.
func TestContractInstanceChanges_KeyIsTransactionScoped(t *testing.T) {
	for _, file := range []string{"contract_instance_changes.sql", "tier1_schema.sql"} {
		ddl := stripSQLComments(readDeployDDL(t, file))
		table := createStmt(ddl, "TABLE", "stellar.contract_instance_changes")
		mv := createStmt(ddl, "MATERIALIZED VIEW", "stellar.contract_instance_changes_mv")
		if table == "" || mv == "" {
			t.Fatalf("%s: contract_instance_changes table or MV not found", file)
		}
		if !strings.HasSuffix(table, "ORDER BY (contract_hash, ledger_seq, tx_hash, change_index)") {
			t.Errorf("%s: sort key is not (contract_hash, ledger_seq, tx_hash, change_index) — "+
				"change_index alone collides across a ledger's transactions:\n%s", file, table)
		}
		for _, col := range []string{"tx_hash String", "intra_ledger_seq UInt32"} {
			if !strings.Contains(table, col) {
				t.Errorf("%s: table does not declare %q:\n%s", file, col, table)
			}
		}
		for _, col := range []string{" tx_hash,", " intra_ledger_seq,"} {
			if !strings.Contains(mv, col) {
				t.Errorf("%s: MV does not project%s — the column would read its DEFAULT on "+
					"every live row:\n%s", file, strings.TrimSuffix(col, ","), mv)
			}
		}
	}
}

// TestContractInstanceChangesTxKeyMigration_MirrorsCanonicalDDL: the r1
// migration builds v2 with the canonical DDL under the cut-over names. A v2
// that drifted from it would be renamed onto the canonical name and serve a
// shape no fresh host has.
func TestContractInstanceChangesTxKeyMigration_MirrorsCanonicalDDL(t *testing.T) {
	canon := stripSQLComments(readDeployDDL(t, "contract_instance_changes.sql"))
	mig := readDeployDDL(t, "contract_instance_changes_tx_key.sql")
	for _, marker := range []string{
		"-- si-apply-scope: operator",
		"-- si-cutover-object: stellar.contract_instance_changes_v2\n",
		"-- si-cutover-object: stellar.contract_instance_changes_v2_mv\n",
	} {
		if !strings.Contains(mig, marker) {
			t.Errorf("migration header missing %q", marker)
		}
	}
	mig = stripSQLComments(mig)

	wantTable := createStmt(canon, "TABLE", "stellar.contract_instance_changes")
	gotTable := strings.ReplaceAll(createStmt(mig, "TABLE", "stellar.contract_instance_changes_v2"),
		"stellar.contract_instance_changes_v2", "stellar.contract_instance_changes")
	if gotTable != wantTable {
		t.Errorf("v2 table drifted from the canonical DDL:\n got %s\nwant %s", gotTable, wantTable)
	}
	wantMV := createStmt(canon, "MATERIALIZED VIEW", "stellar.contract_instance_changes_mv")
	gotMV := createStmt(mig, "MATERIALIZED VIEW", "stellar.contract_instance_changes_v2_mv")
	gotMV = strings.ReplaceAll(gotMV, "stellar.contract_instance_changes_v2_mv", "stellar.contract_instance_changes_mv")
	gotMV = strings.ReplaceAll(gotMV, "stellar.contract_instance_changes_v2", "stellar.contract_instance_changes")
	if gotMV != wantMV {
		t.Errorf("v2 MV drifted from the canonical DDL:\n got %s\nwant %s", gotMV, wantMV)
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

// instanceIndexStub answers the two timeline probes (usable, and the
// key-shape probe with keyShapeErr) and records the timeline reads.
func instanceIndexStub(keyShapeErr error) *stubConn {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes LIMIT 1"):
			if keyShapeErr != nil {
				return nil, keyShapeErr
			}
			return &stubRows{}, nil
		case strings.Contains(q, "contract_instance_changes LIMIT 1"):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case strings.Contains(q, "is_sac, wasm_hash"):
			return &stubRows{data: [][]any{{uint8(0), strings.Repeat("ab", 32)}}}, nil
		default:
			return &stubRows{}, nil
		}
	}
	return conn
}

func lastTimelineRead(t *testing.T, conn *stubConn) string {
	t.Helper()
	for i := len(conn.queries) - 1; i >= 0; i-- {
		if !strings.Contains(conn.queries[i], "LIMIT 1") || strings.Contains(conn.queries[i], "is_sac") {
			return conn.queries[i]
		}
	}
	t.Fatal("no timeline read issued")
	return ""
}

// TestContractInstanceIndexedReads_OrderSameLedgerWritesByWalkPosition: on
// the tx-keyed table both reads rank a ledger's writes by intra_ledger_seq
// before change_index, so the ledger-final executable is the newest row.
// Against the old ORDER BY ledger_seq DESC, change_index DESC two
// transactions' writes (both change_index 0) tie and either may win.
func TestContractInstanceIndexedReads_OrderSameLedgerWritesByWalkPosition(t *testing.T) {
	var cid xdr.Hash
	copy(cid[:], []byte("contract-id-32-bytes-padding----"))

	conn := instanceIndexStub(nil)
	r := &ExplorerReader{conn: conn}
	if _, err := r.ContractCodeHistory(context.Background(), testContractID); err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	q := lastTimelineRead(t, conn)
	for _, must := range []string{
		"ORDER BY ledger_seq DESC, intra_ledger_seq DESC, change_index DESC",
		"ORDER BY ledger_seq ASC, intra_ledger_seq ASC, change_index ASC",
	} {
		if !strings.Contains(q, must) {
			t.Errorf("code-history read missing %q:\n%s", must, q)
		}
	}

	conn = instanceIndexStub(nil)
	r = &ExplorerReader{conn: conn}
	if _, _, err := r.contractWasmHash(context.Background(), cid); err != nil {
		t.Fatalf("contractWasmHash: %v", err)
	}
	if q := lastTimelineRead(t, conn); !strings.Contains(q, "ORDER BY ledger_seq DESC, intra_ledger_seq DESC, change_index DESC") {
		t.Errorf("wasm-hash read does not rank by intra_ledger_seq:\n%s", q)
	}
}

// TestContractInstanceIndexedReads_OldKeyTableFallsBack: r1's table predates
// intra_ledger_seq until its cut-over. The reads must detect that and use the
// change_index order rather than 500 with "Unknown identifier".
func TestContractInstanceIndexedReads_OldKeyTableFallsBack(t *testing.T) {
	var cid xdr.Hash
	copy(cid[:], []byte("contract-id-32-bytes-padding----"))
	noColumn := &clickhouse.Exception{
		Code: 47, Name: "UNKNOWN_IDENTIFIER",
		Message: "Missing columns: 'intra_ledger_seq' 'tx_hash'",
	}

	conn := instanceIndexStub(noColumn)
	r := &ExplorerReader{conn: conn}
	if _, err := r.ContractCodeHistory(context.Background(), testContractID); err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	if q := lastTimelineRead(t, conn); strings.Contains(q, "intra_ledger_seq") ||
		!strings.Contains(q, "ORDER BY ledger_seq DESC, change_index DESC") {
		t.Errorf("old-key code-history read = %s, want the change_index order", q)
	}

	conn = instanceIndexStub(noColumn)
	r = &ExplorerReader{conn: conn}
	if _, ok, err := r.contractWasmHash(context.Background(), cid); err != nil || !ok {
		t.Fatalf("contractWasmHash: ok=%v err=%v", ok, err)
	}
	if q := lastTimelineRead(t, conn); strings.Contains(q, "intra_ledger_seq") {
		t.Errorf("old-key wasm-hash read names intra_ledger_seq:\n%s", q)
	}
}

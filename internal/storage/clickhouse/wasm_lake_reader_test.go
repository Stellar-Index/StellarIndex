package clickhouse

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Regression tests for ContractCodeHistory's read
// of stellar.ledger_entry_changes must be LIMITed. Its sibling instance lookups
// (contractWasmHash, SACClassicAssetName) are all `ORDER BY ledger_seq DESC
// LIMIT 1`; an unbounded one streams and XDR-decodes every captured change to a
// contract's instance key, so a contract that rewrites its instance entry often
// (instance-STORAGE writes, not only `update_contract` upgrades) reintroduces an
// unbounded scan even once the skip index is tightened.
//
// These use the stubConn/stubRows harness from tx_hash_index_test.go. stubConn
// does not execute SQL, so the bound assertions are query-SHAPE assertions —
// proof of the SQL the reader emits — while TestContractCodeHistory_CollapsesTo
// DistinctExecutables below pins the decoded output values end to end.

// testContractID is a well-formed C-strkey (the same one the explorer handler
// tests use), so strkey.Decode succeeds and the reader reaches its query.
const testContractID = "CAM7DY53G63XA4AJRS24Z6VFYAFSSF76C3RZ45BE5YU3FQS5255OOABP"

// instanceEntryB64 builds the base64 LedgerEntry the lake stores for a
// contract-instance contract_data change pointing at wasmHash — the exact shape
// ContractCodeHistory decodes.
func instanceEntryB64(t *testing.T, wasmHash xdr.Hash) string {
	t.Helper()
	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("contract-id-32-bytes-padding----"))
	cid := xdr.ContractId(cidRaw)
	hash := wasmHash
	inst := xdr.ScContractInstance{
		Executable: xdr.ContractExecutable{
			Type:     xdr.ContractExecutableTypeContractExecutableWasm,
			WasmHash: &hash,
		},
	}
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &inst},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal instance entry: %v", err)
	}
	return b64
}

func wasmHashN(n byte) xdr.Hash {
	var h xdr.Hash
	for i := range h {
		h[i] = n
	}
	return h
}

// TestContractCodeHistory_BoundsTheScan pins the cap. Against the un-fixed
// reader the emitted SQL carries no LIMIT at all and the only bound arg is the
// key list, so both assertions below fail.
func TestContractCodeHistory_BoundsTheScan(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "stellar.contract_instance_changes") {
			// Index probe: no rows → unavailable → legacy scan path.
			return &stubRows{}, nil
		}
		if !strings.Contains(q, "FROM stellar.ledger_entry_changes") {
			t.Fatalf("unexpected query: %s", q)
		}
		return &stubRows{}, nil
	}
	r := &ExplorerReader{conn: conn}

	if _, err := r.ContractCodeHistory(context.Background(), testContractID); err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	// Query 0 is the instance-index probe; the legacy scan is the last.
	q := conn.queries[len(conn.queries)-1]
	args := conn.args[len(conn.args)-1]
	if !strings.Contains(q, "FROM stellar.ledger_entry_changes") {
		t.Fatalf("last query = %q, want the legacy changes scan", q)
	}

	if !strings.Contains(q, "LIMIT ?") {
		t.Fatalf("query = %q, want a bound `LIMIT ?` — an unbounded instance-change "+
			"scan is exactly what C-F1 item 3 removed", q)
	}
	// The cap must be the one the code declares, passed as a bound arg after
	// the key list.
	if len(args) != 2 {
		t.Fatalf("query args = %v, want [keys, limit]", args)
	}
	got, ok := args[1].(int)
	if !ok || got != contractCodeHistoryMaxRows {
		t.Fatalf("limit arg = %v (%T), want %d", args[1], args[1], contractCodeHistoryMaxRows)
	}
	if contractCodeHistoryMaxRows <= 0 || contractCodeHistoryMaxRows > 100_000 {
		t.Fatalf("contractCodeHistoryMaxRows = %d, want a positive cap well under a "+
			"pathological scan", contractCodeHistoryMaxRows)
	}

	// Truncation direction matters: the cap is taken NEWEST-first and re-sorted
	// ascending for the caller, so a truncated timeline keeps the CURRENT
	// executable and drops the oldest tail — never the reverse.
	if !strings.Contains(q, "ORDER BY ledger_seq DESC, intra_ledger_seq DESC, change_index DESC, ingested_at DESC") {
		t.Fatalf("query = %q, want the capped inner select ordered newest-first", q)
	}
	inner := strings.Index(q, "ORDER BY ledger_seq DESC")
	outer := strings.Index(q, "ORDER BY ledger_seq ASC")
	if inner < 0 || outer < 0 || outer < inner {
		t.Fatalf("query = %q, want the newest-first LIMIT applied INSIDE and the "+
			"result re-sorted ascending outside", q)
	}
	if lim := strings.Index(q, "LIMIT ?"); lim < inner || lim > outer {
		t.Fatalf("query = %q, want the LIMIT inside the newest-first subquery", q)
	}
}

// TestContractCodeHistory_CollapsesToDistinctExecutables pins the decoded
// output: consecutive identical wasm hashes collapse to one version, a change
// back to an earlier hash is a NEW version, and each version carries the ledger
// + close time of the change that introduced it.
func TestContractCodeHistory_CollapsesToDistinctExecutables(t *testing.T) {
	hashA, hashB := wasmHashN(0xAA), wasmHashN(0xBB)
	entryA, entryB := instanceEntryB64(t, hashA), instanceEntryB64(t, hashB)
	base := time.Unix(1700000000, 0).UTC()

	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "stellar.contract_instance_changes") {
			return &stubRows{}, nil // index unavailable → legacy path
		}
		// Ascending, as the query's outer ORDER BY delivers them:
		// A, A (no-op rewrite), B (upgrade), A (rollback).
		return &stubRows{data: [][]any{
			{uint32(100), base, entryA},
			{uint32(101), base.Add(5 * time.Second), entryA},
			{uint32(102), base.Add(10 * time.Second), entryB},
			{uint32(103), base.Add(15 * time.Second), entryA},
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	got, err := r.ContractCodeHistory(context.Background(), testContractID)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	want := []ContractCodeVersion{
		{Ledger: 100, CloseTime: base, WasmHash: hex.EncodeToString(hashA[:])},
		{Ledger: 102, CloseTime: base.Add(10 * time.Second), WasmHash: hex.EncodeToString(hashB[:])},
		{Ledger: 103, CloseTime: base.Add(15 * time.Second), WasmHash: hex.EncodeToString(hashA[:])},
	}
	if len(got) != len(want) {
		t.Fatalf("versions = %+v, want %d entries (%+v)", got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("version[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestContractCodeHistory_IndexedPath pins the fast path over
// stellar.contract_instance_changes: when the
// probe finds the index usable, the reader must walk the keyed timeline
// (contract_hash primary-key predicate, wasm-only rows, the same
// newest-first cap re-sorted ascending) and never touch the changes log;
// the pre-extracted hashes collapse exactly like the legacy decode.
func TestContractCodeHistory_IndexedPath(t *testing.T) {
	rawA, rawB := wasmHashN(0xAA), wasmHashN(0xBB)
	hashA := hex.EncodeToString(rawA[:])
	hashB := hex.EncodeToString(rawB[:])
	base := time.Unix(1700000000, 0).UTC()

	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "FROM stellar.ledger_entry_changes") {
			t.Fatalf("indexed path must not touch the changes log: %s", q)
		}
		if strings.Contains(q, "LIMIT 1") { // probe: one row → usable
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		return &stubRows{data: [][]any{
			{uint32(100), base, hashA},
			{uint32(101), base.Add(5 * time.Second), hashA},
			{uint32(102), base.Add(10 * time.Second), hashB},
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	got, err := r.ContractCodeHistory(context.Background(), testContractID)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	want := []ContractCodeVersion{
		{Ledger: 100, CloseTime: base, WasmHash: hashA},
		{Ledger: 102, CloseTime: base.Add(10 * time.Second), WasmHash: hashB},
	}
	if len(got) != len(want) {
		t.Fatalf("versions = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("version[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	q := conn.queries[len(conn.queries)-1]
	for _, must := range []string{
		"FROM stellar.contract_instance_changes",
		"WHERE contract_hash = ?",
		"is_sac = 0",
		"ORDER BY ledger_seq DESC, intra_ledger_seq DESC, change_index DESC",
		"LIMIT ?",
	} {
		if !strings.Contains(q, must) {
			t.Fatalf("indexed query = %q, missing %q", q, must)
		}
	}
	args := conn.args[len(conn.args)-1]
	if len(args) != 2 {
		t.Fatalf("indexed query args = %v, want [contract_hash, limit]", args)
	}
	if lim, ok := args[1].(int); !ok || lim != contractCodeHistoryMaxRows {
		t.Fatalf("limit arg = %v, want %d", args[1], contractCodeHistoryMaxRows)
	}
}

// indexMissStub serves a usable instance index holding no timeline rows for
// this contract; legacy is what the changes-log scan returns.
func indexMissStub(legacy [][]any) *stubConn {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "FROM stellar.ledger_entry_changes"):
			return &stubRows{data: legacy}, nil
		case strings.Contains(q, "contract_instance_changes LIMIT 1"):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		default: // the indexed timeline read
			return &stubRows{}, nil
		}
	}
	return conn
}

// TestContractCodeHistory_IndexMissFallsBackToLegacyScan: a usable
// index holding no per-contract row is NOT proof the contract never
// upgraded — instanceChangesIndexAvailable is a table-global LIMIT-1
// emptiness probe that cannot see partial per-contract backfill coverage
// (the same class already fixed for contractWasmHash). So the reader must
// fall through to the changes-log scan rather than serving the empty
// timeline as authoritative.
func TestContractCodeHistory_IndexMissFallsBackToLegacyScan(t *testing.T) {
	conn := indexMissStub(nil)
	r := &ExplorerReader{conn: conn}

	got, err := r.ContractCodeHistory(context.Background(), testContractID)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("versions = %+v, want none when both the index and the legacy scan are empty", got)
	}
	var sawTimeline, sawLegacy bool
	for _, q := range conn.queries {
		sawTimeline = sawTimeline || strings.Contains(q, "WHERE contract_hash = ? AND is_sac = 0")
		sawLegacy = sawLegacy || strings.Contains(q, "FROM stellar.ledger_entry_changes")
	}
	if !sawTimeline {
		t.Fatalf("queries = %q, want the indexed timeline read", conn.queries)
	}
	if !sawLegacy {
		t.Fatalf("queries = %q, want the legacy scan after an unproven index miss", conn.queries)
	}
}

// TestContractCodeHistory_SACInIndexSkipsLegacyScan: a SAC's index rows are
// all is_sac = 1, so its wasm timeline is empty although the index covers it.
// That empty history is authoritative on both key shapes; the changes-log
// scan it would otherwise fall into cannot finish within the read budget.
func TestContractCodeHistory_SACInIndexSkipsLegacyScan(t *testing.T) {
	noColumn := &clickhouse.Exception{
		Code: 47, Name: "UNKNOWN_IDENTIFIER",
		Message: "Missing columns: 'intra_ledger_seq' 'tx_hash'",
	}
	for name, keyShapeErr := range map[string]error{"tx-keyed": nil, "old-key": noColumn} {
		t.Run(name, func(t *testing.T) {
			var timelineArgs, presenceArgs []any
			conn := &stubConn{}
			conn.respond = func(q string) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "FROM stellar.ledger_entry_changes"):
					t.Fatalf("a contract present in the index fell through to the legacy scan: %s", q)
					return nil, nil
				case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes LIMIT 1"):
					if keyShapeErr != nil {
						return nil, keyShapeErr
					}
					return &stubRows{}, nil
				case strings.Contains(q, "contract_instance_changes LIMIT 1"): // availability probe
					return &stubRows{data: [][]any{{uint32(1)}}}, nil
				case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash FROM ("):
					return &stubRows{}, nil // no wasm rows
				default: // per-contract presence read: the SAC's rows exist
					return &stubRows{data: [][]any{{uint8(1)}}}, nil
				}
			}
			r := &ExplorerReader{conn: conn}

			got, err := r.ContractCodeHistory(context.Background(), testContractID)
			if err != nil {
				t.Fatalf("ContractCodeHistory: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("versions = %+v, want none for a SAC", got)
			}
			for i, q := range conn.queries {
				switch {
				case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash FROM ("):
					timelineArgs = conn.args[i]
				case strings.Contains(q, "WHERE contract_hash = ?"):
					presenceArgs = conn.args[i]
					if strings.Contains(q, "is_sac") || strings.Contains(q, "intra_ledger_seq") {
						t.Fatalf("presence read must name only contract_hash: %s", q)
					}
				}
			}
			if len(timelineArgs) == 0 || len(presenceArgs) != 1 || presenceArgs[0] != timelineArgs[0] {
				t.Fatalf("presence read args = %v, want [%v]", presenceArgs, timelineArgs)
			}
		})
	}
}

// TestContractCodeHistory_PartialIndexMissFallsBackToLegacy is the
// sibling gap: ContractCodeHistory trusted an EMPTY per-contract
// result from contract_instance_changes as an authoritative "never
// upgraded", even though instanceChangesIndexAvailable is the same
// table-global LIMIT-1 emptiness probe that cannot see partial per-contract
// backfill coverage. Only contractWasmHash guarded against this; this read
// did not.
//
// Without the fallback, an applied-but-still-backfilling index would make ContractCodeHistory
// return an empty timeline for any contract the backfill hadn't reached
// yet, even though the changes log holds its real upgrade history. The
// fallback mirrors contractWasmHash: only a NON-EMPTY indexed result is
// trusted; an empty one falls through to the legacy changes-log scan.
func TestContractCodeHistory_PartialIndexMissFallsBackToLegacy(t *testing.T) {
	wantHash := wasmHashN(0xEF)

	var legacyRead bool
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes"):
			// Key-shape probe: the tx-keyed table.
			return &stubRows{}, nil
		case strings.Contains(q, "contract_instance_changes") && strings.Contains(q, "SELECT ledger_seq FROM"):
			// Availability probe: the index EXISTS and is non-empty
			// (some other contract has been backfilled) -> "usable".
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash FROM ("):
			// Per-contract lookup: THIS contract's instance history has not
			// been backfilled yet -> zero rows (a PARTIAL-coverage miss,
			// invisible to the probe).
			return &stubRows{}, nil
		case strings.Contains(q, "SELECT 1 FROM stellar.contract_instance_changes"):
			// Per-contract presence read: no row, the backfill has not
			// reached this contract.
			return &stubRows{}, nil
		case strings.Contains(q, "stellar.entry_history_watermark"):
			// No genesis watermark: the miss is unproven.
			return &stubRows{}, nil
		case strings.Contains(q, "FROM stellar.ledger_entry_changes"):
			// Legacy changes-log scan resolves the real upgrade history.
			legacyRead = true
			return &stubRows{data: [][]any{{uint32(1), time.Unix(0, 0).UTC(), instanceEntryB64(t, wantHash)}}}, nil
		default:
			t.Fatalf("unexpected query: %s", q)
			return nil, nil
		}
	}
	r := &ExplorerReader{conn: conn}

	got, err := r.ContractCodeHistory(context.Background(), testContractID)
	if err != nil {
		t.Fatalf("ContractCodeHistory returned error: %v", err)
	}
	if !legacyRead {
		t.Fatal("the legacy ledger_entry_changes read was never issued: the empty indexed " +
			"result was served as an authoritative 'never upgraded' instead of falling back")
	}
	if len(got) != 1 || got[0].WasmHash != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("history = %+v, want one version with hash %x from the legacy scan", got, wantHash)
	}
}

// TestContractCodeHistory_GenesisWatermark: an index miss skips the
// ledger_entry_changes scan only when a genesis watermark covers ledger 1.
func TestContractCodeHistory_GenesisWatermark(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wm       [][]any
		wantScan bool
	}{
		{"present and covering", [][]any{{uint32(500)}}, false},
		{"absent", nil, true},
		{"zero (below any ledger)", [][]any{{uint32(0)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var scanned bool
			conn := &stubConn{}
			conn.respond = func(q string) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes"):
					return &stubRows{}, nil
				case strings.Contains(q, "SELECT ledger_seq FROM"):
					return &stubRows{data: [][]any{{uint32(1)}}}, nil
				case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash FROM ("),
					strings.Contains(q, "SELECT 1 FROM stellar.contract_instance_changes"):
					return &stubRows{}, nil
				case strings.Contains(q, "stellar.entry_history_watermark"):
					return &stubRows{data: tc.wm}, nil
				case strings.Contains(q, "FROM stellar.ledger_entry_changes"):
					scanned = true
					return &stubRows{}, nil
				default:
					t.Fatalf("unexpected query: %s", q)
					return nil, nil
				}
			}
			r := newExplorerReader(conn)
			if _, err := r.ContractCodeHistory(context.Background(), testContractID); err != nil {
				t.Fatal(err)
			}
			if scanned != tc.wantScan {
				t.Fatalf("legacy scan = %v, want %v", scanned, tc.wantScan)
			}
		})
	}
}

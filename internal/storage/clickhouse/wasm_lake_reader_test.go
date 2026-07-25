package clickhouse

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Regression tests for audit-2026-07-23 C-F1 item 3: ContractCodeHistory's read
// of stellar.ledger_entry_changes had NO LIMIT. Its sibling instance lookups
// (contractWasmHash, SACClassicAssetName) are all `ORDER BY ledger_seq DESC
// LIMIT 1`; this one streamed and XDR-decoded every captured change to a
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
		if !strings.Contains(q, "FROM stellar.ledger_entry_changes") {
			t.Fatalf("unexpected query: %s", q)
		}
		return &stubRows{}, nil
	}
	r := &ExplorerReader{conn: conn}

	if _, err := r.ContractCodeHistory(context.Background(), testContractID); err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries, want 1", len(conn.queries))
	}
	q := conn.queries[0]

	if !strings.Contains(q, "LIMIT ?") {
		t.Fatalf("query = %q, want a bound `LIMIT ?` — an unbounded instance-change "+
			"scan is exactly what C-F1 item 3 removed", q)
	}
	// The cap must be the one the code declares, passed as a bound arg after
	// the key list.
	args := conn.args[0]
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
	if !strings.Contains(q, "ORDER BY ledger_seq DESC, change_index DESC, ingested_at DESC") {
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
	conn.respond = func(string) (driver.Rows, error) {
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

//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// T356/T377: contract_instance_changes was keyed (contract_hash, ledger_seq,
// change_index), and change_index restarts per TRANSACTION. Two transactions
// writing one contract's instance in the same ledger — both at change_index 0
// — collapsed to one row on merge, so a same-ledger upgrade vanished from the
// code history and the survivor was the last INSERTED, not the ledger-final
// write. This drives the shipped DDL (tier1 + the r1 migration's v2), its MV,
// the backfill INSERT and both indexed reader queries on a real ClickHouse.
//
// Red against the old key: after OPTIMIZE … FINAL only one ledger-L row
// survives, so the count assertions fail and the history loses a version.

const (
	t356Ledger0 = uint32(72_356_001) // deploy: executable h0
	t356Ledger  = uint32(72_356_050) // tx A installs h1, then tx B installs h2
)

func t356Hash(b byte) xdr.Hash {
	var h xdr.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func t356InstanceKeyAndEntry(t *testing.T, cid xdr.ContractId, wasm xdr.Hash) (string, string) {
	t.Helper()
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   addr,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	inst := xdr.ScContractInstance{Executable: xdr.ContractExecutable{
		Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &wasm,
	}}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   addr,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &inst},
		},
	}}
	k, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal instance key: %v", err)
	}
	e, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal instance entry: %v", err)
	}
	return k, e
}

func t356CodeKeyAndEntry(t *testing.T, wasm xdr.Hash) (string, string) {
	t.Helper()
	var key xdr.LedgerKey
	if err := key.SetContractCode(wasm); err != nil {
		t.Fatalf("contract_code key: %v", err)
	}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.ContractCodeEntry{Hash: wasm, Code: []byte("\x00asm\x01\x00\x00\x00")},
	}}
	k, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal code key: %v", err)
	}
	e, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal code entry: %v", err)
	}
	return k, e
}

// applyDeployStatements executes every statement of one deploy/clickhouse
// artifact — for the migration, that is its Step-1 CREATEs only; the later
// steps are operator-run comments.
func applyDeployStatements(t *testing.T, ctx context.Context, conn driver.Conn, name string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	for _, s := range splitSQLStatements(string(raw)) {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %.80q: %v", name, s, err)
		}
	}
}

func t356CountRows(t *testing.T, ctx context.Context, conn driver.Conn, table, contractHash string, ledger uint32) uint64 {
	t.Helper()
	if err := conn.Exec(ctx, "OPTIMIZE TABLE stellar."+table+" FINAL"); err != nil {
		t.Fatalf("optimize %s: %v", table, err)
	}
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM stellar."+table+
		" FINAL WHERE contract_hash = ? AND ledger_seq = ?", contractHash, ledger).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestContractInstanceChanges_SameLedgerTransactionsBothSurvive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	applyDeployStatements(t, ctx, conn, "contract_instance_changes_tx_key.sql")
	t.Cleanup(func() {
		_ = conn.Exec(context.Background(), "DROP VIEW IF EXISTS stellar.contract_instance_changes_v2_mv")
		_ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS stellar.contract_instance_changes_v2")
	})

	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("t356-two-tx-same-ledger-contract"))
	cid := xdr.ContractId(cidRaw)
	contractHash := hex.EncodeToString(cidRaw[:])
	contractStrkey, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatalf("strkey: %v", err)
	}
	h0, h1, h2 := t356Hash(0x10), t356Hash(0x11), t356Hash(0x12)
	instKey, entry0 := t356InstanceKeyAndEntry(t, cid, h0)
	_, entry1 := t356InstanceKeyAndEntry(t, cid, h1)
	_, entry2 := t356InstanceKeyAndEntry(t, cid, h2)
	codeKey, codeEntry := t356CodeKeyAndEntry(t, h2)
	closeTime := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: t356Ledger0, CloseTime: closeTime, TxHash: "t356-deploy", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: t356Ledger0, CloseTime: closeTime, TxHash: "t356-deploy", OpIndex: 0, ChangeIndex: 1,
			IntraLedgerSeq: 4, ChangeType: "created", EntryType: "contract_code", KeyXDR: codeKey, EntryXDR: codeEntry,
		},
		// Tx B is ledger-final (intra 9) and is handed to the writer FIRST,
		// so an insertion-order survivor would be tx A's h1.
		{
			LedgerSeq: t356Ledger, CloseTime: closeTime, TxHash: "t356-tx-b", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry2,
		},
		{
			LedgerSeq: t356Ledger, CloseTime: closeTime, TxHash: "t356-tx-a", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 4, ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry1,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// The MVs (canonical and the migration's v2) keep both same-ledger writes.
	for _, table := range []string{chstore.ContractInstanceChangesTable, chstore.ContractInstanceChangesV2Table} {
		if n := t356CountRows(t, ctx, conn, table, contractHash, t356Ledger); n != 2 {
			t.Fatalf("%s via MV: %d rows at ledger %d, want 2 (one per transaction)", table, n, t356Ledger)
		}
	}

	// The backfill INSERT, into the migration's target, re-derives the same.
	if err := conn.Exec(ctx, "TRUNCATE TABLE stellar.contract_instance_changes_v2"); err != nil {
		t.Fatalf("truncate v2: %v", err)
	}
	if err := chstore.BackfillContractInstanceChangesInto(ctx, addr, chstore.ContractInstanceChangesV2Table,
		t356Ledger0, t356Ledger, 1000, t.Logf); err != nil {
		t.Fatalf("backfill v2: %v", err)
	}
	if n := t356CountRows(t, ctx, conn, chstore.ContractInstanceChangesV2Table, contractHash, t356Ledger); n != 2 {
		t.Fatalf("backfill: %d rows at ledger %d, want 2", n, t356Ledger)
	}
	var ils []uint32
	var txs []string
	q, err := conn.Query(ctx, `SELECT intra_ledger_seq, tx_hash FROM stellar.contract_instance_changes_v2 FINAL
		WHERE contract_hash = ? AND ledger_seq = ? ORDER BY intra_ledger_seq`, contractHash, t356Ledger)
	if err != nil {
		t.Fatalf("read v2: %v", err)
	}
	for q.Next() {
		var s uint32
		var h string
		if err := q.Scan(&s, &h); err != nil {
			t.Fatalf("scan v2: %v", err)
		}
		ils, txs = append(ils, s), append(txs, h)
	}
	_ = q.Close()
	if len(ils) != 2 || ils[0] != 4 || ils[1] != 9 || txs[0] != "t356-tx-a" || txs[1] != "t356-tx-b" {
		t.Fatalf("backfilled (intra_ledger_seq, tx_hash) = %v %v, want [4 9] [t356-tx-a t356-tx-b]", ils, txs)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	hist, err := er.ContractCodeHistory(ctx, contractStrkey)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	want := []string{hex.EncodeToString(h0[:]), hex.EncodeToString(h1[:]), hex.EncodeToString(h2[:])}
	if len(hist) != len(want) {
		t.Fatalf("code history = %+v, want hashes %v (the same-ledger h1 must not vanish)", hist, want)
	}
	for i, v := range hist {
		if v.WasmHash != want[i] {
			t.Fatalf("code history[%d] = %s, want %s (ledger-final h2 last): %+v", i, v.WasmHash, want[i], hist)
		}
	}

	info, err := er.ContractWasm(ctx, contractStrkey)
	if err != nil {
		t.Fatalf("ContractWasm: %v", err)
	}
	if info.WasmHash != want[2] {
		t.Fatalf("ContractWasm hash = %s, want the ledger-final %s", info.WasmHash, want[2])
	}
}

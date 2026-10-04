//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The genesis watermark is written by Set...Watermark (ordering-guarded) and
// consumed by ContractCodeHistory: an index miss skips the ledger_entry_changes
// scan only under a covering mark. Runs the real INSERT and SELECT.
func TestContractInstanceGenesisWatermark_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	const name = chstore.ContractInstanceChangesTable
	clearMark := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		if err := conn.Exec(cctx, `ALTER TABLE stellar.entry_history_watermark DELETE WHERE name = ? SETTINGS mutations_sync = 2`, name); err != nil {
			t.Fatalf("clear mark: %v", err)
		}
	}
	clearMark()
	t.Cleanup(clearMark)

	const ledger = uint32(72_720_001)
	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("inv2188-watermark-roundtrip-c001"))
	cid := xdr.ContractId(cidRaw)
	strk, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	instKey, entry := t356InstanceKeyAndEntry(t, cid, t356Hash(0x31))
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{{
		LedgerSeq: ledger, CloseTime: time.Date(2025, 4, 2, 0, 0, 0, 0, time.UTC), TxHash: "inv2188-tx",
		IntraLedgerSeq: 1, ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry,
	}}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	// Leave the index non-empty (so it is "available") but without this
	// contract: an index miss the writer's mark alone can authorise.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.contract_instance_changes DELETE WHERE contract_hash = ? SETTINGS mutations_sync = 2`,
		hex.EncodeToString(cidRaw[:])); err != nil {
		t.Fatalf("clear MV row: %v", err)
	}
	other, h33 := t356Hash(0x32), t356Hash(0x33)
	if err := conn.Exec(ctx, `INSERT INTO stellar.contract_instance_changes
		(contract_hash, ledger_seq, tx_hash, change_index, intra_ledger_seq, close_time, is_sac, wasm_hash)
		VALUES (?, 1, 'inv2188-other', 0, 1, now(), 0, ?)`, hex.EncodeToString(h33[:]), hex.EncodeToString(other[:])); err != nil {
		t.Fatalf("seed index row: %v", err)
	}

	history := func() int {
		er, err := chstore.NewExplorerReader(ctx, addr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		defer func() { _ = er.Close() }()
		h, err := er.ContractCodeHistory(ctx, strk)
		if err != nil {
			t.Fatalf("ContractCodeHistory: %v", err)
		}
		return len(h)
	}
	if n := history(); n != 1 {
		t.Fatalf("no mark: history = %d versions, want 1 (the scan must run)", n)
	}
	if err := chstore.SetContractInstanceChangesGenesisWatermark(ctx, addr, name, 500); err != nil {
		t.Fatal(err)
	}
	if n := history(); n != 0 {
		t.Fatalf("covering mark: history = %d versions, want 0 (scan skipped)", n)
	}
	// A later, lower mark must not replace the higher one.
	if err := chstore.SetContractInstanceChangesGenesisWatermark(ctx, addr, name, 300); err != nil {
		t.Fatal(err)
	}
	var rows uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.entry_history_watermark WHERE name = ?`, name).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("mark rows = %d (%v), want 1: a lower thru must record nothing", rows, err)
	}
}
